package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestHealthy(t *testing.T) {
	wss, _ := newTestServer(t)

	// Startup grace: no capture stamp yet, DB reachable → healthy.
	if !wss.healthy() {
		t.Error("fresh server should be healthy (startup grace + DB up)")
	}
	// Recent capture progress → healthy.
	wss.lastCaptureProgress.Store(wss.clock.Now().Unix())
	if !wss.healthy() {
		t.Error("recent capture progress should be healthy")
	}
	// Stale capture progress → unhealthy (wedged loop).
	wss.lastCaptureProgress.Store(wss.clock.Now().Unix() - 100000)
	if wss.healthy() {
		t.Error("stale capture progress should be unhealthy")
	}
	// DB unreachable → unhealthy even with a fresh stamp.
	wss.lastCaptureProgress.Store(wss.clock.Now().Unix())
	wss.db.Close()
	if wss.healthy() {
		t.Error("closed DB should be unhealthy")
	}
}

func TestHandleHealthz(t *testing.T) {
	wss, _ := newTestServer(t)
	wss.startTime = wss.clock.Now()

	rec := httptest.NewRecorder()
	wss.handleHealthz(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthy status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["healthy"] != true {
		t.Errorf("healthy = %v, want true", body["healthy"])
	}
	for _, k := range []string{"goroutines", "heapAllocMB", "dbOpenConns", "inFlight", "externalProcs", "uptimeSecs"} {
		if _, ok := body[k]; !ok {
			t.Errorf("missing key %q in /healthz body", k)
		}
	}

	// Wedged capture loop → 503 so external probes see it.
	wss.lastCaptureProgress.Store(wss.clock.Now().Unix() - 100000)
	rec = httptest.NewRecorder()
	wss.handleHealthz(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unhealthy status = %d, want 503", rec.Code)
	}
}

func TestRunMarker(t *testing.T) {
	cfg := &Config{DBPath: filepath.Join(t.TempDir(), "weather.db")}
	if priorUncleanExit(cfg) {
		t.Error("no marker should exist yet")
	}
	writeRunMarker(cfg)
	if !priorUncleanExit(cfg) {
		t.Error("marker should exist after writeRunMarker")
	}
	removeRunMarker(cfg)
	if priorUncleanExit(cfg) {
		t.Error("marker should be gone after removeRunMarker (clean shutdown)")
	}
}

func TestLimitInFlight(t *testing.T) {
	// nil semaphore → passthrough.
	pass := limitInFlight(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	pass.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("nil sem should pass through, got %d", rec.Code)
	}

	// Cap of 1: while one request holds the slot, a concurrent one gets 503.
	sem := make(chan struct{}, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	h := limitInFlight(sem, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	<-started // first request now holds the only slot

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("over-cap request = %d, want 503", rec.Code)
	}
	close(release)
}
