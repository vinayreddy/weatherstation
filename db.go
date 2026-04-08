package main

import (
	"database/sql"
	"log"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS observations (
    timestamp       INTEGER PRIMARY KEY,
    temp            REAL,
    feels_like      REAL,
    dew_point       REAL,
    humidity        REAL,
    wind_speed      REAL,
    wind_gust       REAL,
    wind_dir        INTEGER,
    pressure        REAL,
    precip_rate     REAL,
    precip_total    REAL,
    solar_radiation REAL,
    uv              REAL
);

CREATE TABLE IF NOT EXISTS images (
    timestamp      INTEGER PRIMARY KEY,
    path           TEXT NOT NULL,
    is_archived    INTEGER DEFAULT 0,
    interest_score REAL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_images_archived ON images(is_archived, interest_score DESC);

CREATE TABLE IF NOT EXISTS kv (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// Observation represents a single weather reading.
type Observation struct {
	Timestamp      int64   `json:"timestamp"`
	Temp           float64 `json:"temp"`
	FeelsLike      float64 `json:"feelsLike"`
	DewPoint       float64 `json:"dewPoint"`
	Humidity       float64 `json:"humidity"`
	WindSpeed      float64 `json:"windSpeed"`
	WindGust       float64 `json:"windGust"`
	WindDir        int     `json:"windDir"`
	Pressure       float64 `json:"pressure"`
	PrecipRate     float64 `json:"precipRate"`
	PrecipTotal    float64 `json:"precipTotal"`
	SolarRadiation float64 `json:"solarRadiation"`
	UV             float64 `json:"uv"`
}

// ImageRecord represents a stored camera image.
type ImageRecord struct {
	Timestamp     int64   `json:"timestamp"`
	Path          string  `json:"path"`
	IsArchived    bool    `json:"isArchived"`
	InterestScore float64 `json:"interestScore"`
}

func InitDB(dbPath string) *sql.DB {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("Failed to open database %s: %v", dbPath, err)
	}
	// Enable WAL mode for better concurrent read/write performance.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		log.Fatalf("Failed to set WAL mode: %v", err)
	}
	// Wait up to 5s for locks to clear instead of failing immediately with SQLITE_BUSY.
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		log.Fatalf("Failed to set busy timeout: %v", err)
	}
	if _, err := db.Exec(schema); err != nil {
		log.Fatalf("Failed to create schema: %v", err)
	}
	return db
}

func InsertObservation(db *sql.DB, obs *Observation) error {
	_, err := db.Exec(`INSERT OR REPLACE INTO observations
		(timestamp, temp, feels_like, dew_point, humidity, wind_speed, wind_gust,
		 wind_dir, pressure, precip_rate, precip_total, solar_radiation, uv)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		obs.Timestamp, obs.Temp, obs.FeelsLike, obs.DewPoint, obs.Humidity,
		obs.WindSpeed, obs.WindGust, obs.WindDir, obs.Pressure,
		obs.PrecipRate, obs.PrecipTotal, obs.SolarRadiation, obs.UV)
	return err
}

func QueryObservations(db *sql.DB, from, to int64) ([]Observation, error) {
	rows, err := db.Query(`SELECT timestamp, temp, feels_like, dew_point, humidity,
		wind_speed, wind_gust, wind_dir, pressure, precip_rate, precip_total,
		solar_radiation, uv FROM observations WHERE timestamp >= ? AND timestamp <= ?
		ORDER BY timestamp`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var obs []Observation
	for rows.Next() {
		var o Observation
		if err := rows.Scan(&o.Timestamp, &o.Temp, &o.FeelsLike, &o.DewPoint,
			&o.Humidity, &o.WindSpeed, &o.WindGust, &o.WindDir, &o.Pressure,
			&o.PrecipRate, &o.PrecipTotal, &o.SolarRadiation, &o.UV); err != nil {
			return nil, err
		}
		obs = append(obs, o)
	}
	return obs, rows.Err()
}

// LatestObservation returns the most recent observation, or nil if none exist.
func LatestObservation(db *sql.DB) (*Observation, error) {
	row := db.QueryRow(`SELECT timestamp, temp, feels_like, dew_point, humidity,
		wind_speed, wind_gust, wind_dir, pressure, precip_rate, precip_total,
		solar_radiation, uv FROM observations ORDER BY timestamp DESC LIMIT 1`)
	var o Observation
	err := row.Scan(&o.Timestamp, &o.Temp, &o.FeelsLike, &o.DewPoint,
		&o.Humidity, &o.WindSpeed, &o.WindGust, &o.WindDir, &o.Pressure,
		&o.PrecipRate, &o.PrecipTotal, &o.SolarRadiation, &o.UV)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func InsertImage(db *sql.DB, img *ImageRecord) error {
	_, err := db.Exec(`INSERT OR REPLACE INTO images (timestamp, path, is_archived, interest_score)
		VALUES (?, ?, ?, ?)`, img.Timestamp, img.Path, boolToInt(img.IsArchived), img.InterestScore)
	return err
}

func QueryImages(db *sql.DB, dayStart, dayEnd int64) ([]ImageRecord, error) {
	rows, err := db.Query(`SELECT timestamp, path, is_archived, interest_score
		FROM images WHERE timestamp >= ? AND timestamp < ? ORDER BY timestamp`,
		dayStart, dayEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var images []ImageRecord
	for rows.Next() {
		var img ImageRecord
		var archived int
		if err := rows.Scan(&img.Timestamp, &img.Path, &archived, &img.InterestScore); err != nil {
			return nil, err
		}
		img.IsArchived = archived != 0
		images = append(images, img)
	}
	return images, rows.Err()
}

// NearestImage returns the image closest in time to the given timestamp.
func NearestImage(db *sql.DB, ts int64) (*ImageRecord, error) {
	row := db.QueryRow(`SELECT timestamp, path, is_archived, interest_score
		FROM images ORDER BY ABS(timestamp - ?) LIMIT 1`, ts)
	var img ImageRecord
	var archived int
	err := row.Scan(&img.Timestamp, &img.Path, &archived, &img.InterestScore)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	img.IsArchived = archived != 0
	return &img, nil
}

// imagePath returns the relative path for an image at the given time.
// Format: live/2026/04/06/143030.jpg
func imagePath(t time.Time) string {
	return t.Format("live/2006/01/02/150405") + ".jpg"
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func kvGet(db *sql.DB, key string) string {
	var val string
	err := db.QueryRow("SELECT value FROM kv WHERE key = ?", key).Scan(&val)
	if err != nil {
		return ""
	}
	return val
}

func kvSet(db *sql.DB, key, value string) {
	db.Exec("INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)", key, value)
}
