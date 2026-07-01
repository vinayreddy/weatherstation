package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestThumbCacheHitSkipsConvert proves a cached thumbnail is served as-is, so
// `convert` runs at most once per image. We pre-seed the cache with sentinel
// bytes (not a real JPEG); if the handler regenerated, it would replace them.
func TestThumbCacheHitSkipsConvert(t *testing.T) {
	wss, mux := newTestServer(t)
	ts := time.Now().In(ptLocation).Unix()
	rel := imagePath(time.Unix(ts, 0).In(ptLocation))

	dst := thumbPath(wss.config.ImageDir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := []byte("CACHED-THUMB-NOT-A-REAL-JPEG")
	if err := os.WriteFile(dst, sentinel, 0644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/thumb/"+rel, nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); got != string(sentinel) {
		t.Errorf("served %q; a cache hit must serve the cached bytes, not regenerate", got)
	}
}

// TestGenThumbRespectsSemaphore verifies the concurrency limiter blocks on a
// full semaphore and bails when the context is done rather than spawning an
// unbounded `convert`.
func TestGenThumbRespectsSemaphore(t *testing.T) {
	wss, _ := newTestServer(t)
	wss.thumbSem = make(chan struct{}, 1)
	wss.thumbSem <- struct{}{} // occupy the only slot

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done, so acquisition can't succeed
	err := wss.genThumb(ctx, "src.jpg", "dst.jpg")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("genThumb err = %v, want context.Canceled", err)
	}
}

// TestThinDayRemovesThumbs checks thinning drops the thumbnails of the frames it
// deletes while keeping the survivor's thumbnail (cache stays 1:1 with frames).
func TestThinDayRemovesThumbs(t *testing.T) {
	clk := NewFakeClock()
	clk.Set(time.Date(2026, 3, 15, 12, 0, 0, 0, ptLocation))
	ws, imgDir := newTestWSS(t, clk)

	frames := []time.Time{
		time.Date(2026, 3, 14, 8, 0, 0, 0, ptLocation),  // earliest -> kept
		time.Date(2026, 3, 14, 8, 15, 0, 0, ptLocation), // thinned
		time.Date(2026, 3, 14, 8, 45, 0, 0, ptLocation), // thinned
	}
	for _, ts := range frames {
		writeImg(t, ws, ts, 10)
		dst := thumbPath(imgDir, imagePath(ts))
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte("thumb"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	d := dayDir{
		path: filepath.Join(imgDir, "live", "2026/03/14"),
		date: time.Date(2026, 3, 14, 0, 0, 0, 0, ptLocation),
	}
	if err := ws.thinDay(d); err != nil {
		t.Fatal(err)
	}
	assertPresent(t, thumbPath(imgDir, imagePath(frames[0])))
	assertAbsent(t, thumbPath(imgDir, imagePath(frames[1])))
	assertAbsent(t, thumbPath(imgDir, imagePath(frames[2])))
}

// TestArchiveAndDeleteDayRemovesThumbDir checks retiring a day also prunes its
// whole cached-thumbnail directory.
func TestArchiveAndDeleteDayRemovesThumbDir(t *testing.T) {
	clk := NewFakeClock()
	clk.Set(time.Date(2026, 3, 15, 12, 0, 0, 0, ptLocation))
	ws, imgDir := newTestWSS(t, clk)
	ws.config.ImageArchiveDest = "" // delete-without-archive mode

	ts := time.Date(2026, 3, 14, 8, 0, 0, 0, ptLocation)
	writeImg(t, ws, ts, 10)
	dst := thumbPath(imgDir, imagePath(ts))
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("thumb"), 0644); err != nil {
		t.Fatal(err)
	}

	liveRoot := filepath.Join(imgDir, "live")
	d := dayDir{
		path: filepath.Join(liveRoot, "2026/03/14"),
		date: time.Date(2026, 3, 14, 0, 0, 0, 0, ptLocation),
	}
	if err := ws.archiveAndDeleteDay(liveRoot, d); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, thumbDayDir(imgDir, d))
	assertAbsent(t, dst)
}
