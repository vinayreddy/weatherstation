package main

import (
	"testing"
	"time"
)

// Seattle, Capitol Hill — the camera's location.
const testLat, testLon = 47.62, -122.32

// scanDay walks a UTC day at one-minute steps and returns the peak elevation,
// the minute (UTC) at which it occurs, and the minimum elevation.
func scanDay(date time.Time) (maxElev float64, maxAtUTC time.Time, minElev float64) {
	maxElev, minElev = -1e9, 1e9
	for m := range 24 * 60 {
		t := date.Add(time.Duration(m) * time.Minute)
		e := sunElevationDeg(testLat, testLon, t)
		if e > maxElev {
			maxElev, maxAtUTC = e, t
		}
		if e < minElev {
			minElev = e
		}
	}
	return
}

func TestSunElevation_SeattleSolstices(t *testing.T) {
	// Summer solstice: noon sun ≈ 90 − (lat − 23.44) ≈ 65.8°, deep night well below.
	summer := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	maxS, atS, minS := scanDay(summer)
	if maxS < 64 || maxS > 68 {
		t.Errorf("summer noon elevation = %.2f, want ≈65.8", maxS)
	}
	if minS > -18 {
		t.Errorf("summer night min elevation = %.2f, want well below horizon", minS)
	}
	// Solar noon in Seattle is ≈13:10 PDT ≈ 20:10 UTC. A flipped-longitude sign
	// would push this ~8h away, so this also guards the east-positive convention.
	if h := atS.UTC().Hour(); h < 19 || h > 21 {
		t.Errorf("summer solar-noon hour = %02d UTC, want ≈20", h)
	}

	// Winter solstice: noon sun ≈ 90 − (lat + 23.44) ≈ 18.9°.
	winter := time.Date(2026, 12, 21, 0, 0, 0, 0, time.UTC)
	maxW, _, _ := scanDay(winter)
	if maxW < 17 || maxW > 21 {
		t.Errorf("winter noon elevation = %.2f, want ≈18.9", maxW)
	}
	if maxS <= maxW {
		t.Errorf("summer noon (%.1f) should exceed winter noon (%.1f)", maxS, maxW)
	}
}

func TestSunElevation_NightIsNegative(t *testing.T) {
	// 2 AM PDT on the solstice = 09:00 UTC — unambiguously night in Seattle.
	night := time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)
	if e := sunElevationDeg(testLat, testLon, night); e > -12 {
		t.Errorf("2am PDT elevation = %.2f, want < -12 (dark)", e)
	}
}
