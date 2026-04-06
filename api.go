package main

import (
	"encoding/json"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
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

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
