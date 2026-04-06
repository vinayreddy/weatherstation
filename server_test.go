package main

import (
	"testing"
	"time"
)

func TestImagePath(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	ts := time.Date(2026, 4, 6, 14, 30, 45, 0, loc)
	got := imagePath(ts)
	want := "live/2026/04/06/143045.jpg"
	if got != want {
		t.Errorf("imagePath = %q, want %q", got, want)
	}
}
