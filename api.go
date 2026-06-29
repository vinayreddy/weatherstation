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
		// Optional downsampling: keep at most one image per `step` seconds. Lets
		// dense days (a frame every 30s) load a manageable grid.
		if step, _ := strconv.ParseInt(r.URL.Query().Get("step"), 10, 64); step > 0 {
			images = downsampleImages(images, step)
		}
		writeJSON(w, map[string]any{
			"images": images,
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

// handleThumb serves a downscaled JPEG for /thumb/<live/...jpg>, generating and
// caching it on first request under <ImageDir>/cache/thumb/. The cache sits
// outside live/, so the image lifecycle never archives it and it can be safely
// regenerated or wiped.
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
	dst := filepath.Join(wss.config.ImageDir, "cache", "thumb", relClean)

	if _, err := os.Stat(dst); err != nil {
		if _, err := os.Stat(src); err != nil {
			http.Error(w, "image not found", 404)
			return
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			http.Error(w, "thumb error", 500)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "convert", src, "-thumbnail", "400x300", dst)
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Error("thumbnail generation failed", "src", src, "err", err, "out", string(out))
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
