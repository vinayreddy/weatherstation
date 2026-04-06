package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*WeatherStationServer, *http.ServeMux) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db := InitDB(dbPath)
	t.Cleanup(func() { db.Close() })

	wss := &WeatherStationServer{
		config: &Config{
			ImageDir:    t.TempDir(),
			WUStationID: "KTEST",
			HTTPPort:    "0",
		},
		clock: NewFakeClock(),
		al:    &LogAlerter{},
		db:    db,
	}
	mux := NewAPIMux(wss).(*http.ServeMux)
	return wss, mux
}

func TestAPICurrent(t *testing.T) {
	wss, mux := newTestServer(t)

	// Insert an observation
	now := time.Now().Unix()
	InsertObservation(wss.db, &Observation{
		Timestamp: now, Temp: 55, Humidity: 70, WindSpeed: 8, Pressure: 30.1,
	})

	req := httptest.NewRequest("GET", "/api/current", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	obs := resp["observation"].(map[string]any)
	if obs["temp"].(float64) != 55 {
		t.Errorf("temp = %v", obs["temp"])
	}
}

func TestAPIObservations(t *testing.T) {
	wss, mux := newTestServer(t)

	now := time.Now().Unix()
	InsertObservation(wss.db, &Observation{Timestamp: now - 120, Temp: 50})
	InsertObservation(wss.db, &Observation{Timestamp: now - 60, Temp: 52})
	InsertObservation(wss.db, &Observation{Timestamp: now, Temp: 55})

	req := httptest.NewRequest("GET", "/api/observations?from="+
		itoa(now-200)+"&to="+itoa(now+1), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	obs := resp["observations"].([]any)
	if len(obs) != 3 {
		t.Fatalf("got %d observations, want 3", len(obs))
	}
}

func TestAPIImages(t *testing.T) {
	wss, mux := newTestServer(t)

	// Insert images for today
	now := time.Now().In(ptLocation)
	ts := now.Unix()
	InsertImage(wss.db, &ImageRecord{Timestamp: ts - 60, Path: "live/a.jpg"})
	InsertImage(wss.db, &ImageRecord{Timestamp: ts, Path: "live/b.jpg"})

	dateStr := now.Format("2006-01-02")
	req := httptest.NewRequest("GET", "/api/images?date="+dateStr, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp)
	images := resp["images"].([]any)
	if len(images) != 2 {
		t.Fatalf("got %d images, want 2", len(images))
	}
}

func TestAPINearestImage(t *testing.T) {
	wss, mux := newTestServer(t)

	now := time.Now().Unix()
	InsertImage(wss.db, &ImageRecord{Timestamp: now - 120, Path: "a.jpg"})
	InsertImage(wss.db, &ImageRecord{Timestamp: now, Path: "b.jpg"})

	req := httptest.NewRequest("GET", "/api/nearest-image?ts="+itoa(now-10), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var img ImageRecord
	json.NewDecoder(rec.Body).Decode(&img)
	if img.Path != "b.jpg" {
		t.Errorf("nearest = %q, want b.jpg", img.Path)
	}
}

func itoa(n int64) string {
	return fmt.Sprintf("%d", n)
}
