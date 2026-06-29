package main

import (
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// writeRealJPEG writes a small valid JPEG at <ImageDir>/<rel> and registers a
// scored image row, so both /thumb (which shells out to ImageMagick) and the
// highlights API have something real to work with.
func writeRealJPEG(t *testing.T, wss *WeatherStationServer, ts int64, rel, category, detail string, score float64) {
	t.Helper()
	abs := filepath.Join(wss.config.ImageDir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(abs)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	if err := jpeg.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := InsertImage(wss.db, &ImageRecord{Timestamp: ts, Path: rel}); err != nil {
		t.Fatal(err)
	}
	if score > 0 {
		if err := SetImageScore(wss.db, ts, score, category, detail); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPagesRender(t *testing.T) {
	_, mux := newTestServer(t)
	for _, path := range []string{"/", "/viewer", "/history", "/highlights"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

func TestAPIHighlights_DedupAndOrder(t *testing.T) {
	wss, mux := newTestServer(t)
	day := time.Now().In(ptLocation)
	mk := func(offsetMin int, cat, detail string, score float64) {
		ts := day.Add(time.Duration(offsetMin) * time.Minute).Unix()
		rel := imagePath(time.Unix(ts, 0).In(ptLocation))
		writeRealJPEG(t, wss, ts, rel, cat, detail, score)
	}
	// A two-frame windstorm run (collapses to its peak) and one snow frame.
	mk(-120, "windstorm", "Gusts 40 mph", 70)
	mk(-119, "windstorm", "Gusts 55 mph", 110) // peak
	mk(-60, "snow", "Snow at 30F", 95)

	req := httptest.NewRequest("GET", "/api/highlights", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Highlights []ImageRecord `json:"highlights"`
	}
	json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Highlights) != 2 {
		t.Fatalf("got %d highlight events, want 2 (windstorm run collapsed)", len(resp.Highlights))
	}
	if resp.Highlights[0].InterestScore != 110 || resp.Highlights[0].Category != "windstorm" {
		t.Errorf("top = %+v, want windstorm peak 110", resp.Highlights[0])
	}
	if resp.Highlights[1].Category != "snow" {
		t.Errorf("second = %q, want snow", resp.Highlights[1].Category)
	}
}

func TestAPIImagesStepDownsample(t *testing.T) {
	wss, mux := newTestServer(t)
	day := time.Now().In(ptLocation)
	// Five frames one minute apart.
	for i := range 5 {
		ts := day.Add(time.Duration(-i) * time.Minute).Unix()
		InsertImage(wss.db, &ImageRecord{Timestamp: ts, Path: imagePath(time.Unix(ts, 0).In(ptLocation))})
	}
	dateStr := day.Format("2006-01-02")
	req := httptest.NewRequest("GET", "/api/images?date="+dateStr+"&step=180", nil) // 3-min spacing
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var resp struct {
		Images []ImageRecord `json:"images"`
	}
	json.NewDecoder(rec.Body).Decode(&resp)
	// 5 frames over 4 minutes at >=180s spacing → first + one ~3min later = 2.
	if len(resp.Images) != 2 {
		t.Errorf("downsampled to %d, want 2", len(resp.Images))
	}
}

func TestAPINearestObservation(t *testing.T) {
	wss, mux := newTestServer(t)
	now := time.Now().Unix()
	InsertObservation(wss.db, &Observation{Timestamp: now - 600, Temp: 50, WindSpeed: 5})
	InsertObservation(wss.db, &Observation{Timestamp: now, Temp: 58, WindSpeed: 9})

	// Within the ±30min window: nearest to now-120 is the `now` observation.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/nearest-observation?ts="+itoa(now-120), nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var o Observation
	json.NewDecoder(rec.Body).Decode(&o)
	if o.Temp != 58 {
		t.Errorf("nearest temp = %v, want 58", o.Temp)
	}

	// Far from any observation (>30min): 404, not a stale match.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/nearest-observation?ts="+itoa(now+99999), nil))
	if rec.Code != 404 {
		t.Errorf("far-away status = %d, want 404", rec.Code)
	}
}

func TestThumb(t *testing.T) {
	wss, mux := newTestServer(t)
	ts := time.Now().In(ptLocation).Unix()
	rel := imagePath(time.Unix(ts, 0).In(ptLocation))
	writeRealJPEG(t, wss, ts, rel, "", "", 0)

	// Traversal / non-live paths are rejected (call handler directly to bypass
	// the mux's own path cleaning).
	rec := httptest.NewRecorder()
	wss.handleThumb(rec, httptest.NewRequest("GET", "/thumb/live/../../etc/passwd", nil))
	if rec.Code != 404 {
		t.Errorf("traversal status = %d, want 404", rec.Code)
	}

	if _, err := exec.LookPath("convert"); err != nil {
		t.Skip("ImageMagick 'convert' not available; skipping thumbnail generation check")
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/thumb/"+rel, nil))
	if rec.Code != 200 {
		t.Fatalf("thumb status = %d, want 200", rec.Code)
	}
	// Cache file should now exist under cache/thumb/.
	if _, err := os.Stat(filepath.Join(wss.config.ImageDir, "cache", "thumb", rel)); err != nil {
		t.Errorf("thumb not cached: %v", err)
	}
}
