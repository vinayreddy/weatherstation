package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"time"
)

// wuBaseURL is the Weather Underground API base. Overridden in tests.
var wuBaseURL = "https://api.weather.com/v2/pws"

// WUClient fetches weather data from the Weather Underground PWS API.
type WUClient struct {
	apiKey    string
	stationID string
	client    *http.Client
}

func NewWUClient(apiKey, stationID string) *WUClient {
	return &WUClient{
		apiKey:    apiKey,
		stationID: stationID,
		client:    &http.Client{Timeout: 15 * time.Second},
	}
}

// FetchCurrent returns the latest observation from the weather station.
func (c *WUClient) FetchCurrent() (*Observation, error) {
	url := fmt.Sprintf("%s/observations/current?stationId=%s&format=json&units=e&numericPrecision=decimal&apiKey=%s",
		wuBaseURL, c.stationID, c.apiKey)

	var resp wuResponse
	if err := c.fetch(url, &resp); err != nil {
		return nil, Wrap(err, "fetch current observation")
	}
	if len(resp.Observations) == 0 {
		return nil, Errorf("no observations returned for station %s", c.stationID)
	}
	return resp.Observations[0].toObservation(), nil
}

// FetchHistory returns all observations for a specific date (YYYYMMDD format).
// Uses 1 API call.
func (c *WUClient) FetchHistory(date string) ([]Observation, error) {
	url := fmt.Sprintf("%s/history/all?stationId=%s&format=json&units=e&numericPrecision=decimal&date=%s&apiKey=%s",
		wuBaseURL, c.stationID, date, c.apiKey)

	var resp wuResponse
	if err := c.fetch(url, &resp); err != nil {
		return nil, Wrap(err, "fetch history")
	}

	obs := make([]Observation, 0, len(resp.Observations))
	for _, o := range resp.Observations {
		obs = append(obs, *o.toObservation())
	}
	return obs, nil
}

func (c *WUClient) fetch(url string, dst any) error {
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
		return Errorf("WU API returned status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return Wrap(err, "parsing WU response")
	}
	return nil
}

// ---------------------------------------------------------------------------
// WU API response types
// ---------------------------------------------------------------------------

type wuResponse struct {
	Observations []wuObservation `json:"observations"`
}

type wuObservation struct {
	StationID      string     `json:"stationID"`
	ObsTimeUtc     string     `json:"obsTimeUtc"`
	ObsTimeLocal   string     `json:"obsTimeLocal"`
	Epoch          *int64     `json:"epoch"`
	Humidity       *float64   `json:"humidity"`
	WindDir        *int       `json:"winddir"`
	SolarRadiation *float64   `json:"solarRadiation"`
	UV             *float64   `json:"uv"`
	Imperial       *wuImperial `json:"imperial"`
}

type wuImperial struct {
	Temp        *float64 `json:"temp"`
	HeatIndex   *float64 `json:"heatIndex"`
	DewPt       *float64 `json:"dewpt"`
	WindChill   *float64 `json:"windChill"`
	WindSpeed   *float64 `json:"windSpeed"`
	WindGust    *float64 `json:"windGust"`
	Pressure    *float64 `json:"pressure"`
	PrecipRate  *float64 `json:"precipRate"`
	PrecipTotal *float64 `json:"precipTotal"`
}

func (o *wuObservation) toObservation() *Observation {
	obs := &Observation{}
	if o.Epoch != nil {
		obs.Timestamp = *o.Epoch
	}
	if o.Humidity != nil {
		obs.Humidity = *o.Humidity
	}
	if o.WindDir != nil {
		obs.WindDir = *o.WindDir
	}
	if o.SolarRadiation != nil {
		obs.SolarRadiation = *o.SolarRadiation
	}
	if o.UV != nil {
		obs.UV = *o.UV
	}
	if imp := o.Imperial; imp != nil {
		if imp.Temp != nil {
			obs.Temp = *imp.Temp
		}
		// Use heat index as "feels like" when available, fall back to wind chill
		if imp.HeatIndex != nil {
			obs.FeelsLike = *imp.HeatIndex
		} else if imp.WindChill != nil {
			obs.FeelsLike = *imp.WindChill
		}
		if imp.DewPt != nil {
			obs.DewPoint = *imp.DewPt
		}
		if imp.WindSpeed != nil {
			obs.WindSpeed = *imp.WindSpeed
		}
		if imp.WindGust != nil {
			obs.WindGust = *imp.WindGust
		}
		if imp.Pressure != nil {
			obs.Pressure = *imp.Pressure
		}
		if imp.PrecipRate != nil {
			obs.PrecipRate = *imp.PrecipRate
		}
		if imp.PrecipTotal != nil {
			obs.PrecipTotal = *imp.PrecipTotal
		}
	}
	return obs
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Backfill budget: live poller uses ~288 calls/day (every 5min). With a 1,200/day
// limit, that leaves ~900 for backfill. We use 800/day max to keep headroom.
const (
	backfillDailyBudget = 800
	backfillInterval    = 6 * time.Second // 10 req/min, well under 30/min limit
	kvBackfillCursor    = "backfill_cursor"
	kvBackfillOrigin    = "backfill_origin"
)

// startBackfill seeds the backfill origin date (only if not already set) and
// launches the background goroutine. Safe to call on every startup.
func startBackfill(wu *WUClient, db *sql.DB, origin string) {
	// Store origin on first invocation; ignore on subsequent runs.
	if existing := kvGet(db, kvBackfillOrigin); existing == "" {
		from, err := time.ParseInLocation("2006-01-02", origin, ptLocation)
		if err != nil {
			log.Fatalf("Invalid --backfill date %q (use YYYY-MM-DD): %v", origin, err)
		}
		kvSet(db, kvBackfillOrigin, from.Format("2006-01-02"))
		kvSet(db, kvBackfillCursor, from.Format("2006-01-02"))
	}
	go runBackfillLoop(wu, db)
}

// runBackfillLoop runs continuously, fetching up to backfillDailyBudget days per
// calendar day. When the budget is exhausted it sleeps until midnight PT, then
// continues. When all days are backfilled it exits.
func runBackfillLoop(wu *WUClient, db *sql.DB) {
	for {
		cursor := kvGet(db, kvBackfillCursor)
		if cursor == "" {
			return // no backfill configured
		}
		cursorDate, err := time.ParseInLocation("2006-01-02", cursor, ptLocation)
		if err != nil {
			slog.Error("bad backfill cursor, stopping", "cursor", cursor)
			return
		}

		yesterday := time.Now().In(ptLocation).AddDate(0, 0, -1).Truncate(24 * time.Hour)
		if cursorDate.After(yesterday) {
			slog.Info("backfill complete — caught up to yesterday")
			kvSet(db, kvBackfillCursor, "") // clear cursor
			return
		}

		remaining := int(yesterday.Sub(cursorDate).Hours()/24) + 1
		budget := backfillDailyBudget
		if budget > remaining {
			budget = remaining
		}

		slog.Info("backfill starting daily batch",
			"cursor", cursor,
			"batch_size", budget,
			"remaining_days", remaining)

		fetched := 0
		d := cursorDate
		for fetched < budget && !d.After(yesterday) {
			dateStr := d.Format("20060102")
			dayLabel := d.Format("2006-01-02")

			obs, err := wu.FetchHistory(dateStr)
			if err != nil {
				slog.Error("backfill fetch failed, skipping", "date", dayLabel, "err", err)
			} else {
				inserted := 0
				for i := range obs {
					if err := InsertObservation(db, &obs[i]); err == nil {
						inserted++
					}
				}
				slog.Info("backfill", "date", dayLabel, "observations", inserted,
					"progress", fmt.Sprintf("%d/%d", fetched+1, budget))
			}

			d = d.AddDate(0, 0, 1)
			kvSet(db, kvBackfillCursor, d.Format("2006-01-02"))
			fetched++
			time.Sleep(backfillInterval)
		}

		// Check if we're done
		newCursor := kvGet(db, kvBackfillCursor)
		nc, _ := time.ParseInLocation("2006-01-02", newCursor, ptLocation)
		if nc.After(yesterday) {
			slog.Info("backfill complete")
			kvSet(db, kvBackfillCursor, "")
			return
		}

		// Budget exhausted for today — sleep until midnight PT + 1min
		now := time.Now().In(ptLocation)
		midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 1, 0, 0, ptLocation)
		sleepDur := midnight.Sub(now)
		slog.Info("backfill daily budget exhausted, sleeping until midnight",
			"sleep", sleepDur.Round(time.Minute),
			"next_cursor", newCursor)
		time.Sleep(sleepDur)
	}
}
