package main

import (
	"context"
	"encoding/json"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// NewAPIMux creates the HTTP handler with all routes.
func NewAPIMux(wss *WeatherStationServer) http.Handler {
	mux := http.NewServeMux()

	// Embedded frontend templates
	tmplFS, _ := fs.Sub(webFS, "web/templates")
	templates := template.Must(template.ParseFS(tmplFS, "*.html"))

	// Static files (JS, CSS)
	staticFS, _ := fs.Sub(webFS, "web/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Serve camera images from disk
	mux.Handle("GET /images/", http.StripPrefix("/images/", http.FileServer(http.Dir(wss.config.ImageDir))))

	// Serve (and lazily generate) downscaled thumbnails for galleries.
	mux.HandleFunc("GET /thumb/", wss.handleThumb)

	// Pages
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		templates.ExecuteTemplate(w, "dashboard.html", nil)
	})
	mux.HandleFunc("GET /history", func(w http.ResponseWriter, r *http.Request) {
		templates.ExecuteTemplate(w, "history.html", nil)
	})
	mux.HandleFunc("GET /highlights", func(w http.ResponseWriter, r *http.Request) {
		templates.ExecuteTemplate(w, "highlights.html", nil)
	})
	mux.HandleFunc("GET /viewer", func(w http.ResponseWriter, r *http.Request) {
		templates.ExecuteTemplate(w, "viewer.html", nil)
	})

	// JSON API
	mux.HandleFunc("GET /api/current", func(w http.ResponseWriter, r *http.Request) {
		obs, err := LatestObservation(wss.db)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]any{
			"observation": obs,
			"imageURL":    "/images/current.jpg",
			"stationID":   wss.config.WUStationID,
		})
	})

	mux.HandleFunc("GET /api/observations", func(w http.ResponseWriter, r *http.Request) {
		from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)

		// Default: last 24 hours
		if from == 0 || to == 0 {
			now := time.Now().Unix()
			from = now - 86400
			to = now
		}

		obs, err := QueryObservations(wss.db, from, to)
		if err != nil {
			slog.Error("query observations failed", "err", err)
			http.Error(w, err.Error(), 500)
			return
		}
		// Cap the number of points so the payload/DOM/render cost is independent
		// of the range length (a 30-day range is ~8.6k readings; a chart only has
		// a few hundred px of width).
		obs = downsampleObservations(obs, maxChartPoints)
		writeJSON(w, map[string]any{
			"observations": obs,
			"from":         from,
			"to":           to,
		})
	})

	mux.HandleFunc("GET /api/images", func(w http.ResponseWriter, r *http.Request) {
		dateStr := r.URL.Query().Get("date")
		if dateStr == "" {
			dateStr = time.Now().In(ptLocation).Format("2006-01-02")
		}
		t, err := time.ParseInLocation("2006-01-02", dateStr, ptLocation)
		if err != nil {
			http.Error(w, "invalid date format, use YYYY-MM-DD", 400)
			return
		}
		dayStart := t.Unix()
		dayEnd := t.Add(24 * time.Hour).Unix()

		images, err := QueryImages(wss.db, dayStart, dayEnd)
		if err != nil {
			slog.Error("query images failed", "err", err)
			http.Error(w, err.Error(), 500)
			return
		}
		// Optional user-requested downsampling by time step, then a hard cap on the
		// number of frames so a dense day (a frame every 30s) never returns
		// thousands of cells — and thus thousands of thumbnail requests —
		// regardless of the capture rate.
		if step, _ := strconv.ParseInt(r.URL.Query().Get("step"), 10, 64); step > 0 {
			images = downsampleImages(images, step)
		}
		images = capImages(images, maxViewerFrames)

		// Attach each frame's nearest weather so the viewer's lightbox/timelapse
		// never makes a per-frame /api/nearest-observation call. Load the day's
		// observations once and match by a two-pointer merge (both time-sorted);
		// the page thus issues a constant 2 queries regardless of frame count.
		dayObs, err := QueryObservations(wss.db, dayStart, dayEnd)
		if err != nil {
			// Non-fatal: serve frames without weather rather than failing the page.
			slog.Error("query observations for image weather failed", "err", err)
			dayObs = nil
		}
		writeJSON(w, map[string]any{
			"images": attachWeather(images, dayObs),
			"date":   dateStr,
		})
	})

	mux.HandleFunc("GET /api/nearest-image", func(w http.ResponseWriter, r *http.Request) {
		ts, _ := strconv.ParseInt(r.URL.Query().Get("ts"), 10, 64)
		if ts == 0 {
			http.Error(w, "ts parameter required", 400)
			return
		}
		img, err := NearestImage(wss.db, ts)
		if err != nil {
			slog.Error("nearest image failed", "err", err)
			http.Error(w, err.Error(), 500)
			return
		}
		if img == nil {
			http.Error(w, "no images found", 404)
			return
		}
		writeJSON(w, img)
	})

	// Weather observation nearest a timestamp, for captions on historical images
	// (stored frames are raw — no burned-in overlay). Within ±30 min.
	mux.HandleFunc("GET /api/nearest-observation", func(w http.ResponseWriter, r *http.Request) {
		ts, _ := strconv.ParseInt(r.URL.Query().Get("ts"), 10, 64)
		if ts == 0 {
			http.Error(w, "ts parameter required", 400)
			return
		}
		obs, err := NearestObservation(wss.db, ts, 1800)
		if err != nil {
			slog.Error("nearest observation failed", "err", err)
			http.Error(w, err.Error(), 500)
			return
		}
		if obs == nil {
			http.Error(w, "no observation found", 404)
			return
		}
		writeJSON(w, obs)
	})

	// Top "most interesting" frames, deduplicated into events. Params: from, to
	// (unix), category (optional filter), limit (default 60).
	mux.HandleFunc("GET /api/highlights", func(w http.ResponseWriter, r *http.Request) {
		to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
		from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		if to == 0 {
			to = time.Now().Unix()
		}
		if from == 0 {
			from = to - 365*86400 // default: trailing year of local history
		}
		category := r.URL.Query().Get("category")
		limit := 60
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
			limit = n
		}

		// Pull a generous pool of scored frames, collapse runs into events, then trim.
		pool, err := QueryHighlights(wss.db, from, to, category, 5000)
		if err != nil {
			slog.Error("query highlights failed", "err", err)
			http.Error(w, err.Error(), 500)
			return
		}
		events := dedupeHighlights(pool, highlightEventGap)
		if len(events) > limit {
			events = events[:limit]
		}
		writeJSON(w, map[string]any{
			"highlights": events,
			"from":       from,
			"to":         to,
		})
	})

	return mux
}

// Per-page work is bounded to these budgets so the amount of stored data never
// dictates how much a page fetches or renders.
const (
	maxChartPoints  = 1500 // max points returned by /api/observations, any range
	maxViewerFrames = 500  // max frames returned by /api/images, any day density
)

// weatherMatchWindow bounds how far (seconds) a frame may be from an observation
// for that reading to be shown as its weather. Matches the ±30 min window the
// old per-frame /api/nearest-observation used.
const weatherMatchWindow = 1800

// imageWithWeather is an image frame annotated with the observation nearest its
// capture time. Embedding ImageRecord keeps all its fields at the top level of
// the JSON (what viewer.js already reads) and adds a "weather" field, so the
// viewer needs no per-frame weather lookup.
type imageWithWeather struct {
	ImageRecord
	Weather *Observation `json:"weather"`
}

// attachWeather annotates each frame with the observation nearest its timestamp
// (within weatherMatchWindow). Both inputs must be sorted ascending by
// timestamp; it runs in O(len(images)+len(obs)) via a two-pointer merge instead
// of a query per frame. A frame with no observation in range gets Weather=nil.
func attachWeather(images []ImageRecord, obs []Observation) []imageWithWeather {
	out := make([]imageWithWeather, len(images))
	j := 0
	for i, img := range images {
		// Advance while the next observation is at least as close as the current;
		// safe to only move forward because images are sorted ascending too.
		for j+1 < len(obs) &&
			absInt64(obs[j+1].Timestamp-img.Timestamp) <= absInt64(obs[j].Timestamp-img.Timestamp) {
			j++
		}
		out[i] = imageWithWeather{ImageRecord: img}
		if j < len(obs) && absInt64(obs[j].Timestamp-img.Timestamp) <= weatherMatchWindow {
			o := obs[j]
			out[i].Weather = &o
		}
	}
	return out
}

// capImages limits images to roughly maxN frames by keeping every stride-th
// frame, always retaining pinned highlights (images must be sorted ascending).
// Bounds the grid size — and thus the number of thumbnail requests — regardless
// of how many frames a day holds. The result may exceed maxN only by however
// many pinned frames exist off the stride, which is small.
func capImages(images []ImageRecord, maxN int) []ImageRecord {
	if maxN <= 0 || len(images) <= maxN {
		return images
	}
	stride := (len(images) + maxN - 1) / maxN // ceil(len/maxN)
	out := make([]ImageRecord, 0, maxN+1)
	for i, img := range images {
		if img.Pinned || i%stride == 0 {
			out = append(out, img)
		}
	}
	return out
}

// downsampleObservations limits obs to roughly maxN points by keeping every
// stride-th reading (obs must be sorted ascending). Makes a history chart's
// payload and render cost independent of the range length.
func downsampleObservations(obs []Observation, maxN int) []Observation {
	if maxN <= 0 || len(obs) <= maxN {
		return obs
	}
	stride := (len(obs) + maxN - 1) / maxN // ceil(len/maxN)
	out := make([]Observation, 0, maxN+1)
	for i := 0; i < len(obs); i += stride {
		out = append(out, obs[i])
	}
	return out
}

// absInt64 returns the absolute value of x.
func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// downsampleImages keeps at most one image per stepSecs window (images must be
// sorted ascending by timestamp). Pinned highlights are always kept so the best
// frames never vanish from a downsampled grid.
func downsampleImages(images []ImageRecord, stepSecs int64) []ImageRecord {
	out := make([]ImageRecord, 0, len(images))
	var last int64
	for i, img := range images {
		if i == 0 || img.Pinned || img.Timestamp-last >= stepSecs {
			out = append(out, img)
			last = img.Timestamp
		}
	}
	return out
}

// maxConcurrentThumbs bounds how many ImageMagick `convert` processes run at
// once for thumbnail generation. A cold Browse grid can request many thumbnails
// at once; without a cap that spawns dozens of `convert` processes and can
// saturate a small host (the Raspberry Pi). Thumbnails are normally
// pre-generated at capture time (see captureAndOverlay), so this limiter mostly
// engages for cold caches or old pinned frames.
const maxConcurrentThumbs = 2

// thumbPath returns the cache location for the thumbnail of the live frame at
// rel (e.g. "live/2026/03/01/143000.jpg"). The cache mirrors the live/ tree
// under <ImageDir>/cache/thumb/, outside live/ so the image lifecycle never
// archives it; it is pruned alongside its source frame (see thinDay /
// archiveAndDeleteDay) so it stays 1:1 with the frames on disk.
func thumbPath(imageDir, rel string) string {
	return filepath.Join(imageDir, "cache", "thumb", rel)
}

// ensureThumb writes a 400x300 thumbnail of src to dst if dst does not already
// exist. A cache hit is a no-op, so `convert` runs at most once per source
// image, ever. It writes to a temp file then renames, so a killed or timed-out
// convert never leaves a truncated file that later looks like a valid cache hit.
func ensureThumb(ctx context.Context, src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil // already cached
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := runExternal(exec.CommandContext(ctx, "convert", src, "-thumbnail", "400x300", tmp))
	if err != nil {
		os.Remove(tmp)
		return Wrapf(err, "convert %s: %s", src, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, dst)
}

// genThumb runs ensureThumb under the thumbnail concurrency limiter, so no more
// than maxConcurrentThumbs `convert` processes run at once. Acquisition respects
// ctx, so a disconnected client (or a timeout) doesn't leave a goroutine parked
// on the semaphore. A nil semaphore (unset in some tests) means no limit.
func (wss *WeatherStationServer) genThumb(ctx context.Context, src, dst string) error {
	if wss.thumbSem != nil {
		select {
		case wss.thumbSem <- struct{}{}:
			defer func() { <-wss.thumbSem }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ensureThumb(ctx, src, dst)
}

// handleThumb serves a downscaled JPEG for /thumb/<live/...jpg>. Thumbnails are
// normally pre-generated at capture; on a cache miss this regenerates one
// (rate-limited via genThumb) and caches it. A cache hit skips the semaphore
// entirely and just serves the file.
func (wss *WeatherStationServer) handleThumb(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/thumb/")
	clean := path.Clean("/" + rel) // collapse any ../ and normalise
	// Only serve live-capture JPEGs; reject traversal and anything else.
	if !strings.HasPrefix(clean, "/live/") || !strings.HasSuffix(clean, ".jpg") {
		http.Error(w, "not found", 404)
		return
	}
	relClean := strings.TrimPrefix(clean, "/")
	src := filepath.Join(wss.config.ImageDir, relClean)
	dst := thumbPath(wss.config.ImageDir, relClean)

	if _, err := os.Stat(dst); err != nil {
		if _, err := os.Stat(src); err != nil {
			http.Error(w, "image not found", 404)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := wss.genThumb(ctx, src, dst); err != nil {
			slog.Error("thumbnail generation failed", "src", src, "err", err)
			http.Error(w, "thumb error", 500)
			return
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, dst)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// buildHandler wraps the app mux with the in-flight limiter, but registers
// /healthz on an outer mux *outside* the limiter so health probes always answer,
// even while the app is shedding load with 503s.
func buildHandler(wss *WeatherStationServer, app http.Handler) http.Handler {
	root := http.NewServeMux()
	root.HandleFunc("/healthz", wss.handleHealthz)
	root.Handle("/", limitInFlight(wss.inflightSem, app))
	return root
}

// limitInFlight caps concurrent in-flight requests via a buffered semaphore.
// Beyond the cap it replies 503 immediately instead of queuing unbounded work
// (every request is a goroutine, and the expensive ones touch the DB or spawn
// subprocesses). The channel doubles as the gauge len(sem) that /healthz reports.
// A nil sem disables the limit (used in tests).
func limitInFlight(sem chan struct{}, next http.Handler) http.Handler {
	if sem == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "server busy", http.StatusServiceUnavailable)
		}
	})
}
