package main

import (
	"bufio"
	"embed"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // embed timezone data for raspi
)

//go:embed web/templates web/static
var webFS embed.FS

// Build-time variables injected via ldflags.
var (
	BuildDate = "unknown"
	BuildUser = "unknown"
	UnameInfo = "unknown"
	GitCommit = "unknown"
	GitBranch = "unknown"
)

// Flags
var (
	envFile             = flag.String("env", ".env", "Path to a .env file to load environment variables from")
	versionFlag         = flag.Bool("version", false, "Print version info and exit")
	forkAndMonitorFlag  = flag.Bool("fork_and_monitor", true, "Fork and monitor the child process, restarting on crashes")
	exitAfterCrashAlert = flag.Bool("exit_after_crash_alert", false, "Exit after crashing enough times to trigger an alert")
	backfillFrom        = flag.String("backfill", "", "Backfill weather data from this date (YYYY-MM-DD). Runs in background alongside live server, ~800 days/day, resumes across restarts.")
)

var ptLocation *time.Location

func init() {
	var err error
	ptLocation, err = time.LoadLocation("America/Los_Angeles")
	if err != nil {
		log.Fatalf("Failed to load America/Los_Angeles timezone: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Clock
// ---------------------------------------------------------------------------

type Clock interface {
	Now() time.Time
	NowPacific() time.Time
}

type RealClock struct{}

func (c *RealClock) Now() time.Time        { return time.Now() }
func (c *RealClock) NowPacific() time.Time { return time.Now().In(ptLocation) }

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type Config struct {
	WUApiKey         string
	WUStationID      string
	RTSPStream       string
	ImageDir         string
	DBPath           string
	HTTPPort         string
	RefreshSecs      int
	MailtrapAPIToken string
	AlertEmailTo     string
	AlertEmailFrom   string
}

var knownEnvVars = map[string]bool{
	"WS_WU_API_KEY":         true,
	"WS_WU_STATION_ID":      true,
	"WS_RTSP_STREAM":        true,
	"WS_IMAGE_DIR":          true,
	"WS_DB_PATH":            true,
	"WS_HTTP_PORT":          true,
	"WS_REFRESH_SECS":       true,
	"WS_MAILTRAP_API_TOKEN": true,
	"WS_ALERT_EMAIL_TO":     true,
	"WS_ALERT_EMAIL_FROM":   true,
}

func LoadConfig() *Config {
	return &Config{
		WUApiKey:         os.Getenv("WS_WU_API_KEY"),
		WUStationID:      getEnv("WS_WU_STATION_ID", "KWASEATT3003"),
		RTSPStream:       os.Getenv("WS_RTSP_STREAM"),
		ImageDir:         getEnv("WS_IMAGE_DIR", "./data/images"),
		DBPath:           getEnv("WS_DB_PATH", "./data/weather.db"),
		HTTPPort:         getEnv("WS_HTTP_PORT", "8080"),
		RefreshSecs:      getEnvInt("WS_REFRESH_SECS", 30),
		MailtrapAPIToken: os.Getenv("WS_MAILTRAP_API_TOKEN"),
		AlertEmailTo:     os.Getenv("WS_ALERT_EMAIL_TO"),
		AlertEmailFrom:   getEnv("WS_ALERT_EMAIL_FROM", "weatherstation@localhost"),
	}
}

func WarnUnknownEnvVars() {
	var unknown []string
	for _, entry := range os.Environ() {
		parts := strings.SplitN(entry, "=", 2)
		key := parts[0]
		if strings.HasPrefix(key, "WS_") && !knownEnvVars[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return
	}
	known := make([]string, 0, len(knownEnvVars))
	for k := range knownEnvVars {
		known = append(known, k)
	}
	sort.Strings(known)
	for _, u := range unknown {
		log.Fatalf("unknown env var %q — possible typo? Known WS_* vars: %s",
			u, strings.Join(known, ", "))
	}
}

// ---------------------------------------------------------------------------
// Env file loading
// ---------------------------------------------------------------------------

// loadEnvFile reads a .env file and sets environment variables.
// Lines starting with # are comments, empty lines are skipped.
// Only sets variables that are not already set in the environment.
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		// If using the default .env and it doesn't exist, silently skip.
		if *envFile == ".env" && os.IsNotExist(err) {
			return
		}
		log.Fatalf("failed to open env file %s: %v", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		// Strip surrounding quotes
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}

		// Don't override existing env vars
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if val := os.Getenv(key); val != "" {
		if n, err := strconv.Atoi(val); err == nil {
			return n
		}
	}
	return fallback
}

func versionInfo() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Build Date: %s\n", BuildDate)
	fmt.Fprintf(&b, "Build User: %s\n", BuildUser)
	fmt.Fprintf(&b, "Uname Info: %s\n", UnameInfo)
	fmt.Fprintf(&b, "Git Commit: %s\n", GitCommit)
	fmt.Fprintf(&b, "Git Branch: %s", GitBranch)
	return b.String()
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	flag.Parse()

	if *versionFlag {
		fmt.Println(versionInfo())
		return
	}

	loadEnvFile(*envFile)
	WarnUnknownEnvVars()
	cfg := LoadConfig()

	var alerter Alerter
	if cfg.MailtrapAPIToken != "" {
		alerter = NewMailtrapAlerter(cfg.MailtrapAPIToken, cfg.AlertEmailFrom, cfg.AlertEmailTo)
	} else {
		alerter = &LogAlerter{}
	}

	cl := &RealClock{}
	if MaybeForkAndMonitor(cl, alerter, *forkAndMonitorFlag, *exitAfterCrashAlert) {
		return
	}

	slog.Info("starting weatherstation", "version", versionInfo())

	db := InitDB(cfg.DBPath)
	defer db.Close()

	var wu *WUClient
	if cfg.WUApiKey != "" {
		wu = NewWUClient(cfg.WUApiKey, cfg.WUStationID)
	}

	// Start background backfill if requested (runs alongside the live server).
	// Only needs to be passed once — progress is saved in the DB.
	if *backfillFrom != "" {
		if wu == nil {
			log.Fatal("WS_WU_API_KEY is required for backfill")
		}
		startBackfill(wu, db, *backfillFrom)
	} else if cursor := kvGet(db, kvBackfillCursor); cursor != "" && wu != nil {
		// Resume a previously started backfill
		go runBackfillLoop(wu, db)
	}

	wss := &WeatherStationServer{
		config: cfg,
		clock:  cl,
		al:     alerter,
		db:     db,
		wu:     wu,
	}

	// Start HTTP server in background
	mux := NewAPIMux(wss)
	go func() {
		addr := ":" + cfg.HTTPPort
		slog.Info("starting HTTP server", "addr", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	wss.backgroundLoop()
}
