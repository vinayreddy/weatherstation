package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"5GB", 5 << 30, false},
		{"500MB", 500 << 20, false},
		{"1.5G", int64(1.5 * (1 << 30)), false},
		{"1024", 1024, false},
		{"2k", 2 << 10, false},
		{"  10 MB ", 10 << 20, false},
		{"1TB", 1 << 40, false},
		{"", 0, true},
		{"abc", 0, true},
		{"5XB", 0, true},
	}
	for _, c := range cases {
		got, err := parseSize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSize(%q) expected error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSize(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// newTestWSS builds a WeatherStationServer backed by a temp ImageDir + SQLite db.
func newTestWSS(t *testing.T, clk *FakeClock) (*WeatherStationServer, string) {
	t.Helper()
	root := t.TempDir()
	db := InitDB(filepath.Join(root, "test.db"))
	t.Cleanup(func() { db.Close() })
	imgDir := filepath.Join(root, "images")
	return &WeatherStationServer{
		config: &Config{ImageDir: imgDir},
		clock:  clk,
		db:     db,
		al:     &LogAlerter{},
	}, imgDir
}

// writeImg writes a fake capture (size bytes) at ts and registers its DB row,
// exactly as captureAndOverlay would (path via imagePath).
func writeImg(t *testing.T, ws *WeatherStationServer, ts time.Time, size int) {
	t.Helper()
	rel := imagePath(ts)
	abs := filepath.Join(ws.config.ImageDir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, make([]byte, size), 0644); err != nil {
		t.Fatal(err)
	}
	if err := InsertImage(ws.db, &ImageRecord{Timestamp: ts.Unix(), Path: rel}); err != nil {
		t.Fatal(err)
	}
}

func listJPGs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jpg") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func assertPresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected %s present: %v", path, err)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s absent, err=%v", path, err)
	}
}

func TestThinPriorDays(t *testing.T) {
	clk := NewFakeClock()
	clk.Set(time.Date(2026, 3, 15, 12, 0, 0, 0, ptLocation))
	ws, imgDir := newTestWSS(t, clk)
	at := func(d, h, m, s int) time.Time { return time.Date(2026, 3, d, h, m, s, 0, ptLocation) }

	// Prior day 03-14: hour 08 has three frames, hour 09 has two.
	for _, ts := range []time.Time{at(14, 8, 0, 0), at(14, 8, 15, 0), at(14, 8, 45, 0), at(14, 9, 5, 0), at(14, 9, 30, 0)} {
		writeImg(t, ws, ts, 10)
	}
	// Today 03-15: three frames — must be left fully intact.
	for _, ts := range []time.Time{at(15, 10, 0, 0), at(15, 10, 1, 0), at(15, 10, 2, 0)} {
		writeImg(t, ws, ts, 10)
	}

	liveRoot := filepath.Join(imgDir, "live")
	todayMid := time.Date(2026, 3, 15, 0, 0, 0, 0, ptLocation)
	if err := ws.thinPriorDays(liveRoot, todayMid); err != nil {
		t.Fatal(err)
	}

	// Prior day thinned to the earliest frame per hour.
	gotPrior := listJPGs(t, filepath.Join(liveRoot, "2026/03/14"))
	wantPrior := []string{"080000.jpg", "090500.jpg"}
	if !slices.Equal(gotPrior, wantPrior) {
		t.Errorf("prior-day files = %v, want %v", gotPrior, wantPrior)
	}
	// Today untouched.
	if got := listJPGs(t, filepath.Join(liveRoot, "2026/03/15")); len(got) != 3 {
		t.Errorf("today files = %v, want 3", got)
	}
	// DB rows for the prior day reduced to the two kept frames.
	d14 := time.Date(2026, 3, 14, 0, 0, 0, 0, ptLocation)
	rows, err := QueryImages(ws.db, d14.Unix(), d14.AddDate(0, 0, 1).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("prior-day rows = %d, want 2", len(rows))
	}

	// Idempotent: a second pass changes nothing.
	if err := ws.thinPriorDays(liveRoot, todayMid); err != nil {
		t.Fatal(err)
	}
	if got := listJPGs(t, filepath.Join(liveRoot, "2026/03/14")); !slices.Equal(got, wantPrior) {
		t.Errorf("after 2nd thin = %v, want %v", got, wantPrior)
	}
}

func TestEnforceDiskCapNoArchive(t *testing.T) {
	clk := NewFakeClock()
	clk.Set(time.Date(2026, 3, 15, 12, 0, 0, 0, ptLocation))
	ws, imgDir := newTestWSS(t, clk)
	ws.config.ImageDiskLimit = 250
	ws.config.ImageArchiveDest = "" // delete-without-archive mode

	at := func(d int) time.Time { return time.Date(2026, 3, d, 10, 0, 0, 0, ptLocation) }
	for _, d := range []int{12, 13, 14} { // three 100-byte prior days
		writeImg(t, ws, at(d), 100)
	}
	writeImg(t, ws, at(15), 100) // today

	liveRoot := filepath.Join(imgDir, "live")
	todayMid := time.Date(2026, 3, 15, 0, 0, 0, 0, ptLocation)
	if err := ws.enforceDiskCap(liveRoot, todayMid); err != nil {
		t.Fatal(err)
	}

	// 400B total, limit 250: evict 03-12 (->300) then 03-13 (->200<=250, stop).
	assertAbsent(t, filepath.Join(liveRoot, "2026/03/12"))
	assertAbsent(t, filepath.Join(liveRoot, "2026/03/13"))
	assertPresent(t, filepath.Join(liveRoot, "2026/03/14"))
	assertPresent(t, filepath.Join(liveRoot, "2026/03/15"))

	if total, _ := dirSize(imgDir); total > 250 {
		t.Errorf("total %d still over limit 250", total)
	}
	// Evicted rows hard-deleted; survivors remain.
	d12 := time.Date(2026, 3, 12, 0, 0, 0, 0, ptLocation)
	if rows, _ := QueryImages(ws.db, d12.Unix(), d12.AddDate(0, 0, 1).Unix()); len(rows) != 0 {
		t.Errorf("evicted-day rows = %d, want 0", len(rows))
	}
}

func TestArchiveAndDeleteDayRsyncFailKeepsData(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not available")
	}
	clk := NewFakeClock()
	clk.Set(time.Date(2026, 3, 15, 12, 0, 0, 0, ptLocation))
	ws, imgDir := newTestWSS(t, clk)
	// Unresolvable .invalid host => rsync/ssh fails fast (BatchMode, no prompt).
	ws.config.ImageArchiveDest = "ws-test@host.invalid:/tmp/archive"

	writeImg(t, ws, time.Date(2026, 3, 14, 8, 0, 0, 0, ptLocation), 10)
	liveRoot := filepath.Join(imgDir, "live")
	d := dayDir{
		path: filepath.Join(liveRoot, "2026/03/14"),
		date: time.Date(2026, 3, 14, 0, 0, 0, 0, ptLocation),
	}

	if err := ws.archiveAndDeleteDay(liveRoot, d); err == nil {
		t.Fatal("expected error when archive push fails")
	}
	// Invariant: never delete un-archived data.
	assertPresent(t, filepath.Join(liveRoot, "2026/03/14", "080000.jpg"))
	rows, _ := QueryImages(ws.db, d.date.Unix(), d.date.AddDate(0, 0, 1).Unix())
	if len(rows) != 1 {
		t.Errorf("rows after failed archive = %d, want 1 (kept)", len(rows))
	}
}
