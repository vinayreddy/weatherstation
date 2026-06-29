package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Weather Underground exposes no space-weather data, so aurora detection uses
// NOAA's Space Weather Prediction Center (SWPC), whose products are free and
// need no API key. We track the planetary Kp index (0-9). Seattle (~47.6°N)
// needs roughly Kp >= 6 before aurora is visible on the northern horizon.

// swpcBaseURL is the SWPC products base. Overridden in tests.
var swpcBaseURL = "https://services.swpc.noaa.gov/products"

// SpaceWeatherClient fetches geomagnetic data from NOAA SWPC. Requests are
// rate-limited, mirroring WUClient, though SWPC is polled at most hourly.
type SpaceWeatherClient struct {
	client  *http.Client
	mu      sync.Mutex
	lastReq time.Time
}

const swpcMinRequestInterval = 5 * time.Second

func NewSpaceWeatherClient() *SpaceWeatherClient {
	return &SpaceWeatherClient{client: &http.Client{Timeout: 20 * time.Second}}
}

// KpReading is the planetary Kp index for one 3-hour bucket.
type KpReading struct {
	Bucket int64   // unix seconds at the start of the 3-hour bucket (UTC)
	Kp     float64 // 0-9
}

// swpcKpRow matches a row of noaa-planetary-k-index.json, e.g.
// {"time_tag":"2026-06-22T00:00:00","Kp":1.33,"a_running":5,"station_count":8}
type swpcKpRow struct {
	TimeTag string  `json:"time_tag"`
	Kp      float64 `json:"Kp"`
}

// FetchKp returns the observed planetary Kp index, one entry per 3-hour bucket,
// covering roughly the last 7 days. A single API call.
func (c *SpaceWeatherClient) FetchKp() ([]KpReading, error) {
	url := swpcBaseURL + "/noaa-planetary-k-index.json"
	var rows []swpcKpRow
	if err := c.fetch(url, &rows); err != nil {
		return nil, Wrap(err, "fetch planetary Kp")
	}
	out := make([]KpReading, 0, len(rows))
	for _, r := range rows {
		// time_tag is UTC with no zone suffix.
		t, err := time.ParseInLocation("2006-01-02T15:04:05", r.TimeTag, time.UTC)
		if err != nil {
			slog.Warn("skipping unparseable Kp time_tag", "time_tag", r.TimeTag)
			continue
		}
		out = append(out, KpReading{Bucket: t.Unix(), Kp: r.Kp})
	}
	return out, nil
}

func (c *SpaceWeatherClient) fetch(url string, dst any) error {
	c.mu.Lock()
	if wait := swpcMinRequestInterval - time.Since(c.lastReq); wait > 0 {
		time.Sleep(wait)
	}
	c.lastReq = time.Now()
	c.mu.Unlock()

	resp, err := c.client.Get(url)
	if err != nil {
		return Wrap(err, "http request")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Wrap(err, "reading response")
	}
	if resp.StatusCode != 200 {
		return Errorf("SWPC returned status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return Wrap(err, "parsing SWPC response")
	}
	return nil
}

// spaceWeatherLoop seeds the recent Kp history on startup (one call returns ~7
// days of 3-hour buckets) and refreshes hourly. Kp itself only updates every 3
// hours, so hourly polling is conservative.
func (ws *WeatherStationServer) spaceWeatherLoop() {
	ws.refreshSpaceWeather()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		ws.refreshSpaceWeather()
	}
}

func (ws *WeatherStationServer) refreshSpaceWeather() {
	readings, err := ws.sw.FetchKp()
	if err != nil {
		slog.Error("failed to fetch space weather", "err", err)
		return
	}
	stored, maxKp := 0, 0.0
	for _, r := range readings {
		if err := UpsertKp(ws.db, r.Bucket, r.Kp); err != nil {
			slog.Error("failed to store Kp", "err", err)
			continue
		}
		stored++
		if r.Kp > maxKp {
			maxKp = r.Kp
		}
	}
	slog.Info("space weather updated", "buckets", stored, "max_kp", maxKp)
}
