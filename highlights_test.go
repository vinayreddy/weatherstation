package main

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A daytime and a nighttime instant in Seattle, for category gating.
var (
	noonPDT  = time.Date(2026, 6, 21, 20, 0, 0, 0, time.UTC).Unix() // ~1pm PDT, sun high
	nightPDT = time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC).Unix()  // ~2am PDT, dark
)

func testScoreCtx() scoreCtx {
	return scoreCtx{lat: testLat, lon: testLon, kpThreshold: 6, tempHigh: 95, tempLow: 25}
}

func TestScoreImage_Categories(t *testing.T) {
	sc := testScoreCtx()
	cases := []struct {
		name    string
		ts      int64
		obs     *Observation
		kp      float64
		kpOK    bool
		wantCat string
	}{
		{"snow", noonPDT, &Observation{Temp: 30, PrecipRate: 0.05}, 0, false, "snow"},
		{"windstorm", noonPDT, &Observation{Temp: 50, WindGust: 50}, 0, false, "windstorm"},
		{"heavy-rain", noonPDT, &Observation{Temp: 50, PrecipRate: 0.5}, 0, false, "rain"},
		{"fog", noonPDT, &Observation{Temp: 50, DewPoint: 49, Humidity: 98, WindSpeed: 1}, 0, false, "fog"},
		{"clear", noonPDT, &Observation{Temp: 70, SolarRadiation: 900, Humidity: 40}, 0, false, "clear"},
		{"heat-extreme", noonPDT, &Observation{Temp: 99}, 0, false, "extreme"},
		{"aurora-at-night", nightPDT, &Observation{Temp: 55}, 7, true, "aurora"},
		{"aurora-suppressed-by-day", noonPDT, &Observation{Temp: 55}, 7, true, "clear"}, // daytime: no aurora; nothing else → see below
		{"boring", noonPDT, &Observation{Temp: 60, Humidity: 50}, 0, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			score, cat, _ := scoreImage(c.ts, c.obs, c.kp, c.kpOK, sc)
			// "aurora-suppressed-by-day" has no qualifying category at all.
			if c.name == "aurora-suppressed-by-day" {
				if cat == "aurora" {
					t.Errorf("aurora should not trigger in daylight")
				}
				return
			}
			if cat != c.wantCat {
				t.Errorf("category = %q (score %.1f), want %q", cat, score, c.wantCat)
			}
			if c.wantCat != "" && score <= 0 {
				t.Errorf("score = %.1f, want > 0", score)
			}
		})
	}
}

func TestScoreImage_AuroraScalesWithKp(t *testing.T) {
	sc := testScoreCtx()
	obs := &Observation{Temp: 50}
	s6, _, _ := scoreImage(nightPDT, obs, 6, true, sc)
	s9, _, _ := scoreImage(nightPDT, obs, 9, true, sc)
	if s9 <= s6 {
		t.Errorf("Kp9 score %.1f should exceed Kp6 score %.1f", s9, s6)
	}
}

func TestDedupeHighlights(t *testing.T) {
	base := time.Date(2026, 1, 10, 2, 0, 0, 0, time.UTC).Unix()
	imgs := []ImageRecord{
		// A windstorm run: three frames a few minutes apart → one event (the peak).
		{Timestamp: base, Category: "windstorm", InterestScore: 70},
		{Timestamp: base + 300, Category: "windstorm", InterestScore: 140}, // peak
		{Timestamp: base + 600, Category: "windstorm", InterestScore: 90},
		// Same category but a day later → a separate event.
		{Timestamp: base + 86400, Category: "windstorm", InterestScore: 80},
		// A different category overlapping in time → its own event.
		{Timestamp: base + 300, Category: "snow", InterestScore: 100},
	}
	events := dedupeHighlights(imgs, highlightEventGap)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	// Sorted by score desc: windstorm peak (140), snow (100), later windstorm (80).
	if events[0].InterestScore != 140 || events[0].Timestamp != base+300 {
		t.Errorf("top event = %+v, want the windstorm peak (140 @ base+300)", events[0])
	}
	if events[1].Category != "snow" {
		t.Errorf("second event = %q, want snow", events[1].Category)
	}
}

// TestThinDay_KeepsBestAndPinned verifies thinning keeps the highest-interest
// frame in each hour plus any pinned frame, dropping the rest.
func TestThinDay_KeepsBestAndPinned(t *testing.T) {
	clk := NewFakeClock()
	clk.Set(time.Date(2026, 3, 15, 12, 0, 0, 0, ptLocation))
	ws, imgDir := newTestWSS(t, clk)
	at := func(h, m int) time.Time { return time.Date(2026, 3, 14, h, m, 0, 0, ptLocation) }

	writeImg(t, ws, at(8, 0), 10)  // earliest, unscored
	writeImg(t, ws, at(8, 15), 10) // will be pinned
	writeImg(t, ws, at(8, 30), 10) // highest score → "best"

	// Score 08:30 highest, then pin 08:15 directly (a non-best pin).
	if err := SetImageScore(ws.db, at(8, 30).Unix(), 80, "windstorm", "Gusts 48 mph"); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.db.Exec(`UPDATE images SET pinned = 1 WHERE timestamp = ?`, at(8, 15).Unix()); err != nil {
		t.Fatal(err)
	}

	d := dayDir{path: filepath.Join(imgDir, "live/2026/03/14"), date: time.Date(2026, 3, 14, 0, 0, 0, 0, ptLocation)}
	if err := ws.thinDay(d); err != nil {
		t.Fatal(err)
	}

	got := listJPGs(t, d.path)
	want := []string{"081500.jpg", "083000.jpg"} // pinned + best; earliest dropped
	if !slices.Equal(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
}

// TestArchiveKeepsPinnedLocal verifies that archival retires the day but leaves
// pinned frames on local disk and directly viewable (is_archived = 0).
func TestArchiveKeepsPinnedLocal(t *testing.T) {
	clk := NewFakeClock()
	clk.Set(time.Date(2026, 3, 15, 12, 0, 0, 0, ptLocation))
	ws, imgDir := newTestWSS(t, clk)
	ws.config.ImageArchiveDest = "" // delete-without-archive: pinned still kept

	keep := time.Date(2026, 3, 14, 8, 0, 0, 0, ptLocation)
	drop := time.Date(2026, 3, 14, 9, 0, 0, 0, ptLocation)
	writeImg(t, ws, keep, 10)
	writeImg(t, ws, drop, 10)
	if _, err := ws.db.Exec(`UPDATE images SET pinned = 1 WHERE timestamp = ?`, keep.Unix()); err != nil {
		t.Fatal(err)
	}

	liveRoot := filepath.Join(imgDir, "live")
	d := dayDir{path: filepath.Join(liveRoot, "2026/03/14"), date: time.Date(2026, 3, 14, 0, 0, 0, 0, ptLocation)}
	if err := ws.archiveAndDeleteDay(liveRoot, d); err != nil {
		t.Fatal(err)
	}

	assertPresent(t, filepath.Join(d.path, "080000.jpg")) // pinned, kept local
	assertAbsent(t, filepath.Join(d.path, "090000.jpg"))  // retired

	// Pinned row survives and is still directly servable (not archived).
	rows, err := QueryImages(ws.db, d.date.Unix(), d.date.AddDate(0, 0, 1).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Timestamp != keep.Unix() || rows[0].IsArchived {
		t.Errorf("rows = %+v, want one un-archived pinned row at %d", rows, keep.Unix())
	}
}
