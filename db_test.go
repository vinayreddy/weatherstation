package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestInsertAndQueryObservations(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db := InitDB(dbPath)
	defer db.Close()

	now := time.Now().Unix()
	obs1 := &Observation{Timestamp: now - 300, Temp: 55.0, Humidity: 70, WindSpeed: 5}
	obs2 := &Observation{Timestamp: now, Temp: 57.0, Humidity: 68, WindSpeed: 8}

	if err := InsertObservation(db, obs1); err != nil {
		t.Fatal(err)
	}
	if err := InsertObservation(db, obs2); err != nil {
		t.Fatal(err)
	}

	// Query range
	results, err := QueryObservations(db, now-600, now+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d observations, want 2", len(results))
	}
	if results[0].Temp != 55.0 {
		t.Errorf("first obs temp = %v, want 55", results[0].Temp)
	}
	if results[1].Temp != 57.0 {
		t.Errorf("second obs temp = %v, want 57", results[1].Temp)
	}
}

func TestLatestObservation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db := InitDB(dbPath)
	defer db.Close()

	// No observations
	obs, err := LatestObservation(db)
	if err != nil {
		t.Fatal(err)
	}
	if obs != nil {
		t.Fatal("expected nil for empty DB")
	}

	// Insert and query
	now := time.Now().Unix()
	InsertObservation(db, &Observation{Timestamp: now - 60, Temp: 50})
	InsertObservation(db, &Observation{Timestamp: now, Temp: 55})

	obs, err = LatestObservation(db)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Temp != 55 {
		t.Errorf("latest temp = %v, want 55", obs.Temp)
	}
}

func TestInsertAndQueryImages(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db := InitDB(dbPath)
	defer db.Close()

	now := time.Now().Unix()
	dayStart := now - 3600
	dayEnd := now + 3600

	InsertImage(db, &ImageRecord{Timestamp: now - 60, Path: "live/2026/04/06/120000.jpg"})
	InsertImage(db, &ImageRecord{Timestamp: now, Path: "live/2026/04/06/120100.jpg"})

	images, err := QueryImages(db, dayStart, dayEnd)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 {
		t.Fatalf("got %d images, want 2", len(images))
	}
}

func TestNearestImage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db := InitDB(dbPath)
	defer db.Close()

	now := time.Now().Unix()
	InsertImage(db, &ImageRecord{Timestamp: now - 120, Path: "a.jpg"})
	InsertImage(db, &ImageRecord{Timestamp: now, Path: "b.jpg"})

	img, err := NearestImage(db, now-100)
	if err != nil {
		t.Fatal(err)
	}
	if img.Path != "a.jpg" {
		t.Errorf("nearest to now-100 should be a.jpg, got %s", img.Path)
	}

	img, err = NearestImage(db, now-10)
	if err != nil {
		t.Fatal(err)
	}
	if img.Path != "b.jpg" {
		t.Errorf("nearest to now-10 should be b.jpg, got %s", img.Path)
	}
}
