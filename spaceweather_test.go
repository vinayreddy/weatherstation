package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestSpaceWeather_FetchKp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
			{"time_tag":"2026-06-22T00:00:00","Kp":1.33,"a_running":5,"station_count":8},
			{"time_tag":"2026-06-22T03:00:00","Kp":7.00,"a_running":48,"station_count":8}
		]`))
	}))
	defer srv.Close()

	old := swpcBaseURL
	swpcBaseURL = srv.URL
	defer func() { swpcBaseURL = old }()

	readings, err := NewSpaceWeatherClient().FetchKp()
	if err != nil {
		t.Fatalf("FetchKp: %v", err)
	}
	if len(readings) != 2 {
		t.Fatalf("got %d readings, want 2", len(readings))
	}
	wantBucket := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC).Unix()
	if readings[0].Bucket != wantBucket {
		t.Errorf("bucket0 = %d, want %d", readings[0].Bucket, wantBucket)
	}
	if readings[1].Kp != 7.0 {
		t.Errorf("kp1 = %v, want 7.0", readings[1].Kp)
	}
}

func TestKpAt(t *testing.T) {
	db := InitDB(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()

	bucket := time.Date(2026, 6, 22, 3, 0, 0, 0, time.UTC).Unix()
	if err := UpsertKp(db, bucket, 7.0); err != nil {
		t.Fatal(err)
	}

	// A time one hour into the bucket resolves to its Kp.
	if kp, ok, _ := KpAt(db, bucket+3600); !ok || kp != 7.0 {
		t.Errorf("KpAt(in-bucket) = %v ok=%v, want 7.0 true", kp, ok)
	}
	// A time well past the bucket (no fresher reading) is unknown, not stale.
	if _, ok, _ := KpAt(db, bucket+6*3600); ok {
		t.Errorf("KpAt(6h later) ok=true, want false (too stale)")
	}
	// A time before any reading is unknown.
	if _, ok, _ := KpAt(db, bucket-3600); ok {
		t.Errorf("KpAt(before any) ok=true, want false")
	}
}
