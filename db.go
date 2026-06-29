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

CREATE TABLE IF NOT EXISTS backfill_log (
    date          TEXT PRIMARY KEY,
    backfilled_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS space_weather (
    timestamp INTEGER PRIMARY KEY, -- start of the 3-hour Kp bucket, unix seconds
    kp        REAL                 -- planetary Kp index (0-9) from NOAA SWPC
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
	// Category and Detail are filled by the highlights scoring pass (see
	// highlights.go). Category is a short tag ("aurora", "snow", ...) used for
	// grouping; Detail is a human caption ("Gusts 48 mph"). Empty until scored.
	Category string `json:"category"`
	Detail   string `json:"detail"`
	// Pinned marks a top highlight kept on local disk indefinitely (exempt from
	// thinning/archival eviction). Maintained by the scoring pass.
	Pinned bool `json:"pinned"`
}

// imageCols is the canonical column list for the images table, scanned by
// scanImageRecord. Keep the two in sync.
const imageCols = "timestamp, path, is_archived, interest_score, interest_category, interest_detail, pinned"

// scanImageRecord scans a row selecting imageCols into an ImageRecord.
func scanImageRecord(s interface{ Scan(...any) error }) (ImageRecord, error) {
	var img ImageRecord
	var archived, pinned int
	var cat, detail sql.NullString
	if err := s.Scan(&img.Timestamp, &img.Path, &archived, &img.InterestScore,
		&cat, &detail, &pinned); err != nil {
		return img, err
	}
	img.IsArchived = archived != 0
	img.Pinned = pinned != 0
	img.Category = cat.String
	img.Detail = detail.String
	return img, nil
}

func InitDB(dbPath string) *sql.DB {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("Failed to open database %s: %v", dbPath, err)
	}
	// SQLite only supports one concurrent writer. Limiting to a single
	// connection serialises all access and avoids SQLITE_BUSY errors from
	// the backfill, image-capture, and weather-poll goroutines competing
	// for the write lock. It also guarantees the PRAGMAs below stay in
	// effect (they are per-connection).
	db.SetMaxOpenConns(1)
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
	// Migrations for the highlights feature. The images table predates the
	// scoring columns, so add them in place if missing (idempotent).
	addColumnIfMissing(db, "images", "interest_category", "TEXT DEFAULT ''")
	addColumnIfMissing(db, "images", "interest_detail", "TEXT DEFAULT ''")
	addColumnIfMissing(db, "images", "pinned", "INTEGER DEFAULT 0")
	return db
}

// addColumnIfMissing adds a column to a table if it does not already exist.
// SQLite has no "ADD COLUMN IF NOT EXISTS", so we inspect table_info first.
// Fatals on failure, matching the fail-fast style of InitDB.
func addColumnIfMissing(db *sql.DB, table, column, decl string) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		log.Fatalf("migrate: read %s columns: %v", table, err)
	}
	exists := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			log.Fatalf("migrate: scan %s columns: %v", table, err)
		}
		if name == column {
			exists = true
		}
	}
	rows.Close() // release the single connection before the ALTER below
	if exists {
		return
	}
	if _, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + decl); err != nil {
		log.Fatalf("migrate: add %s.%s: %v", table, column, err)
	}
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
	rows, err := db.Query(`SELECT `+imageCols+`
		FROM images WHERE timestamp >= ? AND timestamp < ? AND is_archived = 0 ORDER BY timestamp`,
		dayStart, dayEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var images []ImageRecord
	for rows.Next() {
		img, err := scanImageRecord(rows)
		if err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	return images, rows.Err()
}

// NearestImage returns the image closest in time to the given timestamp.
func NearestImage(db *sql.DB, ts int64) (*ImageRecord, error) {
	row := db.QueryRow(`SELECT `+imageCols+`
		FROM images WHERE is_archived = 0 ORDER BY ABS(timestamp - ?) LIMIT 1`, ts)
	img, err := scanImageRecord(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &img, nil
}

// GetBackfillTimestamp returns when a date was last backfilled, or 0 if never.
func GetBackfillTimestamp(db *sql.DB, date string) (int64, error) {
	var ts int64
	err := db.QueryRow(`SELECT backfilled_at FROM backfill_log WHERE date = ?`, date).Scan(&ts)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return ts, err
}

// SetBackfillTimestamp records when a date was backfilled.
func SetBackfillTimestamp(db *sql.DB, date string, ts int64) error {
	_, err := db.Exec(`INSERT OR REPLACE INTO backfill_log (date, backfilled_at) VALUES (?, ?)`, date, ts)
	return err
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

// DeleteImageByPath removes a single image row by its relative path. Used by
// thinning, which deletes the file and its row in lockstep.
func DeleteImageByPath(db *sql.DB, path string) error {
	_, err := db.Exec(`DELETE FROM images WHERE path = ?`, path)
	return err
}

// MarkImagesArchived flags non-pinned rows in [fromTs, toTs) as archived: their
// bytes have been pushed to the archive destination and the local files deleted,
// but the rows are kept for the historical timeline. Pinned rows are left untouched —
// their files stay on local disk and remain directly viewable (is_archived = 0).
func MarkImagesArchived(db *sql.DB, fromTs, toTs int64) (int64, error) {
	res, err := db.Exec(`UPDATE images SET is_archived = 1
		WHERE timestamp >= ? AND timestamp < ? AND pinned = 0`, fromTs, toTs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteUnpinnedInRange hard-deletes non-pinned rows in [fromTs, toTs). Used when
// no archive destination is configured — the bytes are gone, so the rows should
// be too. Pinned rows (and their local files) are kept.
func DeleteUnpinnedInRange(db *sql.DB, fromTs, toTs int64) (int64, error) {
	res, err := db.Exec(`DELETE FROM images WHERE timestamp >= ? AND timestamp < ? AND pinned = 0`, fromTs, toTs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// Highlights / scoring support
// ---------------------------------------------------------------------------

// NearestObservation returns the observation closest in time to ts, but only
// within ±windowSecs. Returns nil if no observation falls in that window. The
// bounded range keeps this an indexed PK scan rather than a full-table sort.
func NearestObservation(db *sql.DB, ts, windowSecs int64) (*Observation, error) {
	row := db.QueryRow(`SELECT timestamp, temp, feels_like, dew_point, humidity,
		wind_speed, wind_gust, wind_dir, pressure, precip_rate, precip_total,
		solar_radiation, uv FROM observations
		WHERE timestamp BETWEEN ? AND ? ORDER BY ABS(timestamp - ?) LIMIT 1`,
		ts-windowSecs, ts+windowSecs, ts)
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

// UpsertKp stores the planetary Kp value for a 3-hour bucket.
func UpsertKp(db *sql.DB, bucketTs int64, kp float64) error {
	_, err := db.Exec(`INSERT OR REPLACE INTO space_weather (timestamp, kp) VALUES (?, ?)`, bucketTs, kp)
	return err
}

// KpAt returns the Kp index for the 3-hour bucket containing ts: the most recent
// bucket at or before ts, provided ts is within ~3h+slack of it. ok is false when
// no nearby Kp reading is known (so callers can skip aurora scoring).
func KpAt(db *sql.DB, ts int64) (kp float64, ok bool, err error) {
	var bucket int64
	row := db.QueryRow(`SELECT timestamp, kp FROM space_weather
		WHERE timestamp <= ? ORDER BY timestamp DESC LIMIT 1`, ts)
	if err := row.Scan(&bucket, &kp); err != nil {
		if err == sql.ErrNoRows {
			return 0, false, nil
		}
		return 0, false, err
	}
	const maxAge = int64(4 * 3600) // a 3h bucket plus an hour of slack
	if ts-bucket > maxAge {
		return 0, false, nil
	}
	return kp, true, nil
}

// SetImageScore records the computed interestingness for an image.
func SetImageScore(db *sql.DB, ts int64, score float64, category, detail string) error {
	_, err := db.Exec(`UPDATE images
		SET interest_score = ?, interest_category = ?, interest_detail = ?
		WHERE timestamp = ?`, score, category, detail, ts)
	return err
}

// ImagesToScore returns local images that need (re)scoring: those at or after
// scoreSince that are either unscored or fall within the rolling rescore window
// (timestamp >= rescoreFrom). The rescore window lets late-arriving Kp and
// backfilled observations update recent scores without rescoring all history.
func ImagesToScore(db *sql.DB, scoreSince, rescoreFrom int64) ([]ImageRecord, error) {
	rows, err := db.Query(`SELECT `+imageCols+` FROM images
		WHERE is_archived = 0 AND timestamp >= ?
		  AND (interest_category IS NULL OR interest_category = '' OR timestamp >= ?)
		ORDER BY timestamp`, scoreSince, rescoreFrom)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImageRecord
	for rows.Next() {
		img, err := scanImageRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

// UpdatePins marks the top-k local images by interest_score as pinned and clears
// the pin on all others. Pinned images are kept on local disk indefinitely. k
// bounds local disk use; only positively-scored images are eligible.
func UpdatePins(db *sql.DB, k int) error {
	if k <= 0 {
		_, err := db.Exec(`UPDATE images SET pinned = 0 WHERE pinned = 1`)
		return err
	}
	_, err := db.Exec(`UPDATE images SET pinned = CASE WHEN timestamp IN (
			SELECT timestamp FROM images
			WHERE is_archived = 0 AND interest_score > 0
			ORDER BY interest_score DESC, timestamp DESC LIMIT ?
		) THEN 1 ELSE 0 END`, k)
	return err
}

// imgMeta is the per-image scoring metadata the lifecycle consults when thinning.
type imgMeta struct {
	Score  float64
	Pinned bool
}

// ImageMetaInRange returns score/pinned metadata keyed by timestamp for images in
// [fromTs, toTs). Used by thinning to keep the best (and never drop pinned) frames.
func ImageMetaInRange(db *sql.DB, fromTs, toTs int64) (map[int64]imgMeta, error) {
	rows, err := db.Query(`SELECT timestamp, interest_score, pinned FROM images
		WHERE timestamp >= ? AND timestamp < ?`, fromTs, toTs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]imgMeta{}
	for rows.Next() {
		var ts int64
		var score float64
		var pinned int
		if err := rows.Scan(&ts, &score, &pinned); err != nil {
			return nil, err
		}
		out[ts] = imgMeta{Score: score, Pinned: pinned != 0}
	}
	return out, rows.Err()
}

// PinnedInRange returns the pinned images in [fromTs, toTs). Archival keeps these
// frames on local disk (is_archived stays 0) while retiring the rest of the day.
func PinnedInRange(db *sql.DB, fromTs, toTs int64) ([]ImageRecord, error) {
	rows, err := db.Query(`SELECT `+imageCols+` FROM images
		WHERE timestamp >= ? AND timestamp < ? AND pinned = 1`, fromTs, toTs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImageRecord
	for rows.Next() {
		img, err := scanImageRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

// QueryHighlights returns the highest-scoring local images in [from, to],
// optionally filtered to a single category, ordered by score. limit<=0 means no
// limit. Temporal de-duplication into events is done by the caller.
func QueryHighlights(db *sql.DB, from, to int64, category string, limit int) ([]ImageRecord, error) {
	q := `SELECT ` + imageCols + ` FROM images
		WHERE is_archived = 0 AND interest_score > 0 AND timestamp >= ? AND timestamp <= ?`
	args := []any{from, to}
	if category != "" {
		q += ` AND interest_category = ?`
		args = append(args, category)
	}
	q += ` ORDER BY interest_score DESC, timestamp DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImageRecord
	for rows.Next() {
		img, err := scanImageRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}
