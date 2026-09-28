package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TestCaptureArgsVideoOnly guards the flags that keep a Pi capture under the 30s
// timeout: without video-only SETUP, probing the camera's audio tracks pushed
// every capture past it (see captureArgs).
func TestCaptureArgsVideoOnly(t *testing.T) {
	args := captureArgs("rtsps://cam/stream", "/tmp/out.jpg")
	in := slices.Index(args, "-i")
	amt := slices.Index(args, "-allowed_media_types")
	if in < 0 || amt < 0 || amt > in || args[amt+1] != "video" {
		t.Errorf("want input option -allowed_media_types video before -i, got %v", args)
	}
	if args[in+1] != "rtsps://cam/stream" || args[len(args)-1] != "/tmp/out.jpg" {
		t.Errorf("stream/output misplaced: %v", args)
	}
	for _, want := range []string{"-an", "-hide_banner"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %s: %v", want, args)
		}
	}
}

func TestTail(t *testing.T) {
	if got := tail("a\n  b\tc", 10); got != "a b c" {
		t.Errorf("tail short = %q, want %q", got, "a b c")
	}
	long := strings.Repeat("x", 50) + "\n: signal: killed"
	if got := tail(long, 16); got != "...: signal: killed" {
		t.Errorf("tail long = %q, want %q", got, "...: signal: killed")
	}
}

func TestImagePath(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	ts := time.Date(2026, 4, 6, 14, 30, 45, 0, loc)
	got := imagePath(ts)
	want := "live/2026/04/06/143045.jpg"
	if got != want {
		t.Errorf("imagePath = %q, want %q", got, want)
	}
}
