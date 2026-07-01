package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type WeatherStationServer struct {
	config *Config
	clock  Clock
	al     Alerter
	db     *sql.DB
	wu     *WUClient
	sw     *SpaceWeatherClient
	// thumbSem bounds concurrent ImageMagick thumbnail generation so a cold
	// gallery load can't spawn dozens of `convert` processes at once (see
	// genThumb). Buffered to maxConcurrentThumbs; nil means no limit.
	thumbSem chan struct{}
	// inflightSem caps concurrent in-flight HTTP requests (see limitInFlight);
	// len() is the current in-flight count reported by /healthz. nil means no cap.
	inflightSem chan struct{}
	// lastCaptureProgress is the unix time of the most recent capture-loop
	// iteration; healthy() uses it to detect a wedged loop. 0 until first iteration.
	lastCaptureProgress atomic.Int64
	// startTime is process start, for /healthz uptime.
	startTime time.Time
}

// externalProcs counts external subprocesses (ffmpeg/convert/rsync) currently
// running, for the /healthz gauge and metrics log. Accounting only — the real
// hard cap on subprocesses is the systemd unit's TasksMax.
var externalProcs atomic.Int64

// runExternal runs cmd to completion, accounting for it in externalProcs. Callers
// set any context deadline and cmd.WaitDelay before calling.
func runExternal(cmd *exec.Cmd) ([]byte, error) {
	externalProcs.Add(1)
	defer externalProcs.Add(-1)
	return cmd.CombinedOutput()
}

// backgroundLoop runs the capture loop on the calling goroutine and starts the
// polling/scoring/lifecycle loops as tracked goroutines. All honor ctx: on cancel
// the capture loop stops before starting a new ffmpeg (no half-written frames) and
// backgroundLoop waits for the others to drain before returning, so the caller can
// safely close the DB.
func (ws *WeatherStationServer) backgroundLoop(ctx context.Context) {
	var wg sync.WaitGroup
	start := func(fn func(context.Context)) {
		wg.Go(func() { fn(ctx) })
	}

	if ws.wu != nil {
		start(ws.weatherPollLoop)
	}
	// Space-weather polling (NOAA SWPC Kp index) drives aurora detection.
	if ws.sw != nil {
		start(ws.spaceWeatherLoop)
	}
	// Highlights scoring: rate captured frames and maintain the pinned set.
	start(ws.scoreLoop)
	// Image storage lifecycle: thin prior days to ~1/hour, archive+evict aged
	// days, and keep ImageDir under the configured disk budget.
	start(ws.imageLifecycleLoop)

	// Image capture loop.
	var lastAlertDay string
	for i := int64(0); ; i++ {
		if ctx.Err() != nil {
			break
		}
		// Liveness stamp: healthy() flags the process unhealthy (→ watchdog
		// restart) if this stops advancing, i.e. the loop is wedged.
		ws.lastCaptureProgress.Store(ws.clock.Now().Unix())
		slog.Debug("running image capture loop", "iteration", i)

		wait := time.Second * time.Duration(ws.config.RefreshSecs)
		if err := ws.captureAndOverlay(); err != nil {
			now := ws.clock.NowPacific()
			today := now.Format(time.DateOnly)
			if today != lastAlertDay {
				ws.al.Fire("Error refreshing WS image", fmt.Sprintf("err: %+v", err))
				lastAlertDay = today
			}
			wait = time.Minute
		}
		// Interruptible sleep so shutdown is prompt.
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
	wg.Wait()
}

// weatherPollLoop fetches weather data from WU every 5 minutes and stores it in SQLite.
func (ws *WeatherStationServer) weatherPollLoop(ctx context.Context) {
	// Backfill recent days on startup.
	ws.backfillRecentDays()

	// Fetch immediately on startup
	ws.fetchAndStoreWeather()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	lastBackfillDay := time.Now().Day()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			ws.fetchAndStoreWeather()
			// Re-run backfill once per day (when the day rolls over).
			if t.Day() != lastBackfillDay {
				lastBackfillDay = t.Day()
				ws.backfillRecentDays()
			}
		}
	}
}

func (ws *WeatherStationServer) fetchAndStoreWeather() {
	obs, err := ws.wu.FetchCurrent()
	if err != nil {
		slog.Error("failed to fetch weather", "err", err)
		return
	}
	if err := InsertObservation(ws.db, obs); err != nil {
		slog.Error("failed to store observation", "err", err)
		return
	}
	slog.Info("weather updated",
		"temp", obs.Temp,
		"humidity", obs.Humidity,
		"wind", obs.WindSpeed,
		"precip", obs.PrecipRate)
}

// backfillRecentDays scans the last 30 days. For each date that hasn't been
// backfilled at least 48 hours after the end of that day, it fetches the full
// day's history from WU and records the backfill timestamp. This ensures gaps
// from polling intervals or downtime are eventually filled once WU has
// complete data for the day.
func (ws *WeatherStationServer) backfillRecentDays() {
	now := time.Now()
	for daysAgo := 0; daysAgo <= 30; daysAgo++ {
		date := now.AddDate(0, 0, -daysAgo)
		dateStr := date.Format("20060102")

		// End of this date = start of next day.
		endOfDay := time.Date(date.Year(), date.Month(), date.Day()+1, 0, 0, 0, 0, date.Location())
		threshold := endOfDay.Add(48 * time.Hour)

		backfilledAt, err := GetBackfillTimestamp(ws.db, dateStr)
		if err != nil {
			slog.Error("failed to read backfill log", "date", dateStr, "err", err)
			continue
		}

		// Skip if already backfilled at least 48h after end of day.
		if backfilledAt > 0 && time.Unix(backfilledAt, 0).After(threshold) {
			continue
		}

		observations, err := ws.wu.FetchHistory(dateStr)
		if err != nil {
			slog.Error("backfill failed", "date", dateStr, "err", err)
			continue
		}
		inserted := 0
		for i := range observations {
			if err := InsertObservation(ws.db, &observations[i]); err != nil {
				slog.Error("failed to store backfill observation", "err", err)
				continue
			}
			inserted++
		}
		if err := SetBackfillTimestamp(ws.db, dateStr, now.Unix()); err != nil {
			slog.Error("failed to update backfill log", "date", dateStr, "err", err)
		}
		slog.Info("backfilled", "date", dateStr, "fetched", len(observations), "inserted", inserted)
	}
}

// overlayStyleArgs are the shared ImageMagick text flags for the weather overlay.
// White text on a translucent dark box (-undercolor, ~35% black) stays legible on
// any background — bright overcast, blue sky, foliage, or night — which the old
// flat gray fill could not. Shared by the full overlay and the timestamp-only
// fallback so their styling never drifts.
const overlayStyleArgs = `-font Helvetica -antialias -gravity NorthWest ` +
	`-undercolor '#00000059' -fill '#ffffff' -pointsize 50`

// nbsp is a non-breaking space (U+00A0) used to pad overlay lines. A regular
// leading space does not work: ImageMagick 6 (the Pi's `convert`) drops a
// *leading* ASCII space when sizing the -undercolor box, so the left padding
// vanished while a trailing space still widened the box on the right. A
// non-breaking space survives that trim, giving symmetric left/right breathing
// room around the text on both IM6 (prod) and IM7 (dev).
const nbsp = "\u00a0"

// overlayTimeout bounds the ImageMagick overlay/timestamp `convert` calls. Without
// it a hung convert would wedge the capture loop forever (and, before the
// thumbnail fix, could accumulate). Matches the style of the ffmpeg timeout above.
const overlayTimeout = 30 * time.Second

// captureAndOverlay captures an RTSP frame, overlays weather data, and saves it.
func (ws *WeatherStationServer) captureAndOverlay() (err error) {
	now := ws.clock.NowPacific()

	// Ensure image directory exists for today
	relPath := imagePath(now)
	absPath := filepath.Join(ws.config.ImageDir, relPath)
	if err = os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return Wrap(err, "creating image directory")
	}

	// Capture RTSP frame. CommandContext guarantees we don't wedge forever
	// if the RTSP stream stalls mid-read; -timeout makes ffmpeg itself bail on
	// stalled sockets first so we rarely need the SIGKILL path. (-timeout is the
	// rtsp socket I/O timeout in microseconds; it is accepted across ffmpeg
	// versions, whereas -rw_timeout is rejected by older builds e.g. ffmpeg 5.0.)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y",
		"-rtsp_transport", "tcp",
		"-timeout", "15000000",
		"-i", ws.config.RTSPStream,
		"-qscale:v", "3",
		"-frames:v", "1",
		absPath)
	cmd.WaitDelay = 5 * time.Second
	cmdOutput, err := runExternal(cmd)
	if err != nil {
		err = Wrapf(err, "%s", string(cmdOutput))
		err = Wrap(err, "failed to capture RTSP frame")
		return
	}

	// Fallback: if overlay fails, still copy raw frame as current.jpg
	currentPath := filepath.Join(ws.config.ImageDir, "current.jpg")
	defer func() {
		if err == nil {
			return
		}
		// Just copy the raw capture as the current image with a timestamp
		timeStr := now.Format("2006-01-02   3:04PM")
		addTimeCmd := fmt.Sprintf(
			`convert %s `+overlayStyleArgs+` `+
				`-annotate +20+5 '`+nbsp+`%v`+nbsp+`' %s`,
			absPath, timeStr, currentPath)
		octx, ocancel := context.WithTimeout(context.Background(), overlayTimeout)
		defer ocancel()
		cmd = exec.CommandContext(octx, "bash", "-c", addTimeCmd)
		cmd.WaitDelay = 5 * time.Second
		cmdOutput, innerErr := runExternal(cmd)
		if innerErr != nil {
			slog.Error("failed to write timestamp fallback", "err", innerErr, "output", string(cmdOutput))
		}
	}()

	// Read latest weather from DB
	obs, dbErr := LatestObservation(ws.db)
	if dbErr != nil || obs == nil {
		err = Errorf("no weather data available yet (db err: %v)", dbErr)
		return
	}

	// Overlay weather data on image
	timeStr := now.Format("2006-01-02   3:04PM")
	temp := math.Round(obs.Temp)
	feelsLike := math.Round(obs.FeelsLike)
	wind := math.Round(obs.WindSpeed)
	windGust := math.Round(obs.WindGust)
	humidity := math.Round(obs.Humidity)
	precip := obs.PrecipRate
	pressure := obs.Pressure

	lineHeight := 60
	imageMagickCmd := fmt.Sprintf(
		`convert %s `+overlayStyleArgs+` `+
			`-annotate +20+5 '`+nbsp+`%v`+nbsp+`' `+
			`-annotate +20+%d '`+nbsp+`Temp: %.0fF (feels-like %.0fF)`+nbsp+`' `+
			`-annotate +20+%d '`+nbsp+`Wind: %.0f mph (Gusts: %.0f mph)`+nbsp+`' `+
			`-annotate +20+%d '`+nbsp+`Humidity: %.0f%%  Rain: %.2f in/hr`+nbsp+`' `+
			`-annotate +20+%d '`+nbsp+`Pressure: %.2f inHg`+nbsp+`' %s`,
		absPath, timeStr,
		5+lineHeight, temp, feelsLike,
		5+2*lineHeight, wind, windGust,
		5+3*lineHeight, humidity, precip,
		5+4*lineHeight, pressure,
		currentPath)
	octx, ocancel := context.WithTimeout(context.Background(), overlayTimeout)
	defer ocancel()
	cmd = exec.CommandContext(octx, "bash", "-c", imageMagickCmd)
	cmd.WaitDelay = 5 * time.Second
	cmdOutput, err = runExternal(cmd)
	if err != nil {
		err = Wrapf(err, "%s", string(cmdOutput))
		err = Wrap(err, "failed to overlay weather data")
		return
	}

	// Register image in DB
	if dbErr := InsertImage(ws.db, &ImageRecord{
		Timestamp: now.Unix(),
		Path:      relPath,
	}); dbErr != nil {
		slog.Error("failed to register image in DB", "err", dbErr)
	}

	// Pre-generate the gallery thumbnail so the Browse grid is warm and the
	// /thumb/ handler never has to shell out to ImageMagick on demand. Best-
	// effort: a thumbnail failure must not fail the capture.
	tctx, tcancel := context.WithTimeout(context.Background(), 15*time.Second)
	if terr := ws.genThumb(tctx, absPath, thumbPath(ws.config.ImageDir, relPath)); terr != nil {
		slog.Warn("thumbnail pre-generation failed", "path", relPath, "err", terr)
	}
	tcancel()

	return nil
}
