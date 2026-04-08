package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type WeatherStationServer struct {
	config *Config
	clock  Clock
	al     Alerter
	db     *sql.DB
	wu     *WUClient
}

func (ws *WeatherStationServer) backgroundLoop() {
	// Start weather polling in a separate goroutine
	if ws.wu != nil {
		go ws.weatherPollLoop()
	}

	// Image capture loop
	var lastAlertDay string
	for i := int64(0); ; i++ {
		slog.Debug("running image capture loop", "iteration", i)
		if err := ws.captureAndOverlay(); err == nil {
			time.Sleep(time.Second * time.Duration(ws.config.RefreshSecs))
			continue
		} else {
			now := ws.clock.NowPacific()
			today := now.Format(time.DateOnly)
			if today != lastAlertDay {
				ws.al.Fire("Error refreshing WS image", fmt.Sprintf("err: %+v", err))
				lastAlertDay = today
			}
			time.Sleep(time.Minute)
		}
	}
}

// weatherPollLoop fetches weather data from WU every 5 minutes and stores it in SQLite.
func (ws *WeatherStationServer) weatherPollLoop() {
	// Backfill recent days on startup.
	ws.backfillRecentDays()

	// Fetch immediately on startup
	ws.fetchAndStoreWeather()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	lastBackfillDay := time.Now().Day()
	for t := range ticker.C {
		ws.fetchAndStoreWeather()
		// Re-run backfill once per day (when the day rolls over).
		if t.Day() != lastBackfillDay {
			lastBackfillDay = t.Day()
			ws.backfillRecentDays()
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

// captureAndOverlay captures an RTSP frame, overlays weather data, and saves it.
func (ws *WeatherStationServer) captureAndOverlay() (err error) {
	now := ws.clock.NowPacific()

	// Ensure image directory exists for today
	relPath := imagePath(now)
	absPath := filepath.Join(ws.config.ImageDir, relPath)
	if err = os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return Wrap(err, "creating image directory")
	}

	// Capture RTSP frame
	ffmpegCmd := fmt.Sprintf(
		"ffmpeg -y -rtsp_transport tcp -i %s -qscale:v 3 -frames:v 1 %s",
		ws.config.RTSPStream, absPath)
	cmd := exec.Command("bash", "-c", ffmpegCmd)
	cmdOutput, err := cmd.CombinedOutput()
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
			`convert %s -font Helvetica -antialias -gravity NorthWest `+
				`-fill '#aaaaaa' -pointsize 50 `+
				`-annotate +20+5 '%v' %s`,
			absPath, timeStr, currentPath)
		cmd = exec.Command("bash", "-c", addTimeCmd)
		cmdOutput, innerErr := cmd.CombinedOutput()
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
		`convert %s -font Helvetica -antialias -gravity NorthWest `+
			`-fill '#aaaaaa' -pointsize 50 `+
			`-annotate +20+5 '%v' `+
			`-annotate +20+%d 'Temp: %.0fF (feels-like %.0fF)' `+
			`-annotate +20+%d 'Wind: %.0f mph (Gusts: %.0f mph)' `+
			`-annotate +20+%d 'Humidity: %.0f%%  Rain: %.2f in/hr' `+
			`-annotate +20+%d 'Pressure: %.2f inHg' %s`,
		absPath, timeStr,
		5+lineHeight, temp, feelsLike,
		5+2*lineHeight, wind, windGust,
		5+3*lineHeight, humidity, precip,
		5+4*lineHeight, pressure,
		currentPath)
	cmd = exec.Command("bash", "-c", imageMagickCmd)
	cmdOutput, err = cmd.CombinedOutput()
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

	return nil
}
