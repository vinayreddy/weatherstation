package main

import (
	"math"
	"time"
)

// Solar position via the NOAA solar calculations (the same equations behind
// NOAA's online solar calculator). Accurate to well within a degree, which is
// all the highlights scorer needs to tell night from day, spot golden hour, and
// gate aurora candidates on darkness. Pure arithmetic — no external module.

const (
	deg2rad = math.Pi / 180
	rad2deg = 180 / math.Pi
)

// sunElevationDeg returns the sun's elevation angle (degrees above the horizon,
// negative below) for the given latitude/longitude (degrees, east positive) at
// time t. Longitude west of Greenwich — e.g. Seattle — is negative.
func sunElevationDeg(latDeg, lonDeg float64, t time.Time) float64 {
	utc := t.UTC()
	secs := float64(utc.Hour())*3600 + float64(utc.Minute())*60 + float64(utc.Second())

	jc := (julianDay(utc) - 2451545.0) / 36525.0 // Julian centuries since J2000.0

	// Geometric mean longitude and anomaly of the sun (degrees).
	L0 := math.Mod(280.46646+jc*(36000.76983+jc*0.0003032), 360)
	M := 357.52911 + jc*(35999.05029-0.0001537*jc)
	e := 0.016708634 - jc*(0.000042037+0.0000001267*jc) // orbital eccentricity

	mRad := M * deg2rad
	// Sun's equation of the center (degrees).
	c := math.Sin(mRad)*(1.914602-jc*(0.004817+0.000014*jc)) +
		math.Sin(2*mRad)*(0.019993-0.000101*jc) +
		math.Sin(3*mRad)*0.000289

	trueLong := L0 + c
	omega := 125.04 - 1934.136*jc
	lambda := trueLong - 0.00569 - 0.00478*math.Sin(omega*deg2rad) // apparent longitude

	// Mean obliquity of the ecliptic, with the standard correction.
	obliq := 23 + (26+(21.448-jc*(46.815+jc*(0.00059-jc*0.001813)))/60)/60
	obliqCorr := obliq + 0.00256*math.Cos(omega*deg2rad)

	declin := math.Asin(math.Sin(obliqCorr*deg2rad) * math.Sin(lambda*deg2rad)) // radians

	// Equation of time (minutes).
	y := math.Tan(obliqCorr / 2 * deg2rad)
	y *= y
	l0Rad := L0 * deg2rad
	eqTime := 4 * rad2deg * (y*math.Sin(2*l0Rad) - 2*e*math.Sin(mRad) +
		4*e*y*math.Sin(mRad)*math.Cos(2*l0Rad) -
		0.5*y*y*math.Sin(4*l0Rad) -
		1.25*e*e*math.Sin(2*mRad))

	// True solar time (minutes) working entirely in UTC, so longitude carries the
	// whole offset and there is no separate timezone term.
	trueSolarMin := math.Mod(secs/60+eqTime+4*lonDeg, 1440)
	hourAngle := trueSolarMin/4 - 180 // degrees
	if hourAngle < -180 {
		hourAngle += 360
	}

	latRad := latDeg * deg2rad
	haRad := hourAngle * deg2rad
	cosZenith := math.Sin(latRad)*math.Sin(declin) +
		math.Cos(latRad)*math.Cos(declin)*math.Cos(haRad)
	cosZenith = math.Max(-1, math.Min(1, cosZenith))
	return 90 - math.Acos(cosZenith)*rad2deg
}

// julianDay returns the Julian Day Number (including fractional day) for a UTC time.
func julianDay(t time.Time) float64 {
	y, mInt, d := t.Year(), int(t.Month()), t.Day()
	if mInt <= 2 {
		y--
		mInt += 12
	}
	a := y / 100
	b := 2 - a + a/4
	dayFrac := (float64(t.Hour())*3600 + float64(t.Minute())*60 + float64(t.Second())) / 86400.0
	return math.Floor(365.25*float64(y+4716)) +
		math.Floor(30.6001*float64(mInt+1)) +
		float64(d) + float64(b) - 1524.5 + dayFrac
}
