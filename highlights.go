package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"time"
)

// The highlights scorer rates each captured frame for how "interesting" it is,
// writing a numeric interest_score plus a category tag and human caption to the
// images row. The Highlights page surfaces the top frames. Scoring is
// forward-only: only frames captured at or after the scoring epoch are rated, so
// the existing back-catalogue is left untouched (per the feature's design).

const (
	scoreLoopInterval = 5 * time.Minute
	rescoreWindow     = 48 * time.Hour   // re-score this recent window each pass
	nearestObsWindow  = 30 * time.Minute // ± window to bind a frame to an observation
	highlightEventGap = int64(3 * 3600)  // collapse same-category frames within this gap
	kvScoreSince      = "score_since"    // kv: unix epoch when forward scoring began

	// Sun-elevation gates (degrees).
	auroraDarkElevation = -12.0 // sun must be this far below the horizon (nautical dark)
	goldenLowElevation  = -6.0  // civil twilight
	goldenHighElevation = 8.0

	// Weather thresholds (imperial — the units WU returns).
	snowTempMaxF     = 34.0 // precip at/below this reads as snow/wintry
	heavyRainInHr    = 0.30 // in/hr
	windGustMinMph   = 35.0
	fogSpreadMaxF    = 2.0  // temp − dew point
	fogHumidityMin   = 96.0 // %
	fogWindMaxMph    = 3.0
	clearHumidityMax = 60.0 // %
	clearSkyFraction = 0.85 // solar ≥ this × clear-sky max ⇒ cloudless

	// catUninteresting marks a frame that has been scored and found unremarkable.
	// It is distinct from the empty string (an as-yet-unscored frame) so the
	// scorer doesn't re-evaluate every boring frame on every pass. Score stays 0,
	// so these never surface in Highlights (which filters interest_score > 0).
	catUninteresting = "none"

	// Category base weights, ranked by rarity/drama. Per-category intensity
	// multipliers (≥1) let an extreme event outrank a merely-present rarer one.
	wAurora    = 100.0
	wSnow      = 90.0
	wWindstorm = 70.0
	wExtreme   = 60.0
	wFog       = 45.0
	wHeavyRain = 40.0
	wGolden    = 35.0
	wClear     = 30.0
)

// scoreCtx carries the per-pass inputs shared across every frame in a batch.
type scoreCtx struct {
	lat, lon    float64
	kpThreshold float64
	tempHigh    float64 // p99 temp; NaN when history is too thin to judge extremes
	tempLow     float64 // p01 temp; NaN when unknown
}

// scoreImage rates a single frame and returns (score, category, detail). A score
// of 0 with an empty category means "not interesting" (won't appear in
// Highlights). Pure function: all inputs are passed in, so it is unit-testable.
func scoreImage(ts int64, obs *Observation, kp float64, kpOK bool, sc scoreCtx) (float64, string, string) {
	elev := sunElevationDeg(sc.lat, sc.lon, time.Unix(ts, 0))

	type cand struct {
		score  float64
		cat    string
		detail string
	}
	var cands []cand
	add := func(score float64, cat, detail string) {
		if score > 0 {
			cands = append(cands, cand{score, cat, detail})
		}
	}

	// Aurora — Weather Underground has no space-weather data, so this is driven
	// by NOAA SWPC's Kp index. Requires a dark sky and Kp at/above the
	// Seattle-visibility threshold; score scales with how far above.
	if kpOK && kp >= sc.kpThreshold && elev < auroraDarkElevation {
		intensity := 1 + (kp-sc.kpThreshold)/3
		add(wAurora*intensity, "aurora", fmt.Sprintf("Kp %.1f — aurora possible", kp))
	}

	if obs != nil {
		daytime := elev > 0
		spread := obs.Temp - obs.DewPoint

		// Snow / wintry precip — rare and beautiful in Seattle.
		if obs.PrecipRate > 0 && obs.Temp <= snowTempMaxF {
			intensity := 1 + clamp((snowTempMaxF-obs.Temp)/10, 0, 1)
			add(wSnow*intensity, "snow", fmt.Sprintf("Snow at %.0f°F", obs.Temp))
		}

		// Windstorm — the classic Pacific NW event, keyed on gusts.
		if obs.WindGust >= windGustMinMph {
			add(wWindstorm*(obs.WindGust/windGustMinMph), "windstorm",
				fmt.Sprintf("Gusts %.0f mph", obs.WindGust))
		}

		// Heavy rain (when it's not cold enough to be snow).
		if obs.PrecipRate >= heavyRainInHr && obs.Temp > snowTempMaxF {
			add(wHeavyRain*(obs.PrecipRate/heavyRainInHr), "rain",
				fmt.Sprintf("%.2f in/hr rain", obs.PrecipRate))
		}

		// Fog — small temp/dew-point spread, saturated, calm. Daytime so it's visible.
		if daytime && obs.Humidity >= fogHumidityMin && spread <= fogSpreadMaxF && obs.WindSpeed <= fogWindMaxMph {
			add(wFog, "fog", "Fog")
		}

		// Golden hour — sun near the horizon (sunrise/sunset light). Elevation
		// alone can't tell sunrise from sunset, so the label covers both.
		if elev >= goldenLowElevation && elev <= goldenHighElevation {
			add(wGolden, "golden", "Golden hour")
		}

		// Crystal-clear day — solar near the clear-sky ceiling and dry air. These
		// are the "Mt. Rainier is out" days.
		if daytime {
			if maxSolar := clearSkyMax(elev); maxSolar > 50 &&
				obs.SolarRadiation >= clearSkyFraction*maxSolar && obs.Humidity <= clearHumidityMax {
				add(wClear, "clear", "Crystal clear")
			}
		}

		// Temperature extremes, relative to this station's own history.
		if !math.IsNaN(sc.tempHigh) && obs.Temp >= sc.tempHigh {
			add(wExtreme, "extreme", fmt.Sprintf("Heat %.0f°F", obs.Temp))
		}
		if !math.IsNaN(sc.tempLow) && obs.Temp <= sc.tempLow {
			add(wExtreme, "extreme", fmt.Sprintf("Cold %.0f°F", obs.Temp))
		}
	}

	best := cand{}
	for _, c := range cands {
		if c.score > best.score {
			best = c
		}
	}
	return best.score, best.cat, best.detail
}

// clearSkyMax is a rough clear-sky horizontal irradiance ceiling (W/m²) for a sun
// elevation, used only to separate clear skies from overcast. Crude but adequate.
func clearSkyMax(elevationDeg float64) float64 {
	if elevationDeg <= 0 {
		return 0
	}
	return 1000 * math.Sin(elevationDeg*deg2rad)
}

func clamp(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// scoreLoop scores newly-captured frames forward-only and maintains the pinned
// set of top highlights. It runs once at startup, then every scoreLoopInterval.
func (ws *WeatherStationServer) scoreLoop() {
	// Establish the forward-only scoring epoch once, on first ever run.
	if kvGet(ws.db, kvScoreSince) == "" {
		kvSet(ws.db, kvScoreSince, strconv.FormatInt(ws.clock.Now().Unix(), 10))
	}
	ws.runScoringOnce()
	ticker := time.NewTicker(scoreLoopInterval)
	defer ticker.Stop()
	for range ticker.C {
		ws.runScoringOnce()
	}
}

func (ws *WeatherStationServer) runScoringOnce() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("highlights scoring panicked", "recover", r)
		}
	}()

	scoreSince, _ := strconv.ParseInt(kvGet(ws.db, kvScoreSince), 10, 64)
	rescoreFrom := ws.clock.Now().Add(-rescoreWindow).Unix()
	imgs, err := ImagesToScore(ws.db, scoreSince, rescoreFrom)
	if err != nil {
		slog.Error("scoring: load images failed", "err", err)
		return
	}

	sc := scoreCtx{
		lat:         ws.config.Latitude,
		lon:         ws.config.Longitude,
		kpThreshold: ws.config.AuroraKpThreshold,
		tempHigh:    math.NaN(),
		tempLow:     math.NaN(),
	}
	if high, low, ok := TempExtremes(ws.db); ok {
		sc.tempHigh, sc.tempLow = high, low
	}

	scored := 0
	for _, img := range imgs {
		obs, err := NearestObservation(ws.db, img.Timestamp, int64(nearestObsWindow/time.Second))
		if err != nil {
			slog.Error("scoring: nearest observation failed", "ts", img.Timestamp, "err", err)
			continue
		}
		kp, kpOK, err := KpAt(ws.db, img.Timestamp)
		if err != nil {
			slog.Error("scoring: Kp lookup failed", "ts", img.Timestamp, "err", err)
			continue
		}
		score, cat, detail := scoreImage(img.Timestamp, obs, kp, kpOK, sc)
		if cat == "" {
			cat = catUninteresting // mark as scored so it isn't re-evaluated every pass
		}
		if err := SetImageScore(ws.db, img.Timestamp, score, cat, detail); err != nil {
			slog.Error("scoring: store score failed", "ts", img.Timestamp, "err", err)
			continue
		}
		scored++
	}

	if err := UpdatePins(ws.db, ws.config.HighlightPinCount); err != nil {
		slog.Error("scoring: update pins failed", "err", err)
	}
	if scored > 0 {
		slog.Info("highlights scored", "frames", scored)
	}
}

// TempExtremes returns the p99 (high) and p01 (low) temperatures from the
// station's observation history, used to flag record-breaking frames. ok is
// false when there aren't enough samples to judge extremes meaningfully.
func TempExtremes(db *sql.DB) (high, low float64, ok bool) {
	const sane = `temp > -60 AND temp < 140` // drop sensor sentinels
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM observations WHERE ` + sane).Scan(&n); err != nil || n < 2000 {
		return 0, 0, false
	}
	hiOff := int(float64(n) * 0.99)
	loOff := int(float64(n) * 0.01)
	q := `SELECT temp FROM observations WHERE ` + sane + ` ORDER BY temp LIMIT 1 OFFSET ?`
	if err := db.QueryRow(q, hiOff).Scan(&high); err != nil {
		return 0, 0, false
	}
	if err := db.QueryRow(q, loOff).Scan(&low); err != nil {
		return 0, 0, false
	}
	return high, low, true
}

// dedupeHighlights collapses runs of same-category frames that fall within gapSecs
// into a single representative "event" — the highest-scoring frame in the run.
// This turns a multi-hour windstorm of identical frames into one card. The result
// is sorted by score, highest first.
func dedupeHighlights(imgs []ImageRecord, gapSecs int64) []ImageRecord {
	byCat := map[string][]ImageRecord{}
	for _, im := range imgs {
		byCat[im.Category] = append(byCat[im.Category], im)
	}
	var events []ImageRecord
	for _, list := range byCat {
		sort.Slice(list, func(i, j int) bool { return list[i].Timestamp < list[j].Timestamp })
		var cur ImageRecord
		var lastTs int64
		have := false
		flush := func() {
			if have {
				events = append(events, cur)
				have = false
			}
		}
		for _, im := range list {
			if have && im.Timestamp-lastTs <= gapSecs {
				if im.InterestScore > cur.InterestScore {
					cur = im
				}
			} else {
				flush()
				cur, have = im, true
			}
			lastTs = im.Timestamp
		}
		flush()
	}
	sort.Slice(events, func(i, j int) bool { return events[i].InterestScore > events[j].InterestScore })
	return events
}
