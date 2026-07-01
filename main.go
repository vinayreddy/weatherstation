package main

import (
	"bufio"
	"context"
	"embed"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // embed timezone data for raspi

	"golang.org/x/crypto/acme/autocert"
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

	// HTTPS / TLS via Let's Encrypt (autocert). When TLSEnable is false the
	// server stays plain-HTTP on HTTPPort (local dev + fallback) — unchanged.
	TLSEnable bool   // WS_TLS_ENABLE  — terminate TLS in-process
	Domain    string // WS_DOMAIN      — FQDN the cert is issued for
	TLSPort   string // WS_TLS_PORT    — in-process HTTPS listen port
	CertDir   string // WS_CERT_DIR    — autocert DirCache dir (must persist + be writable)
	ACMEEmail string // WS_ACME_EMAIL  — optional LE account email (expiry notices)

	// Image storage lifecycle.
	ImageDiskLimit         int64  // hard cap on ImageDir size in bytes; <=0 disables
	ImageRetentionDays     int    // keep this many days locally; older is archived+deleted
	ImageArchiveDest       string // rsync dest e.g. user@host:/path; "" => delete without archiving
	ImageArchiveEveryHours int    // cadence (hours) for the retention/archival sweep

	// Highlights / aurora.
	Latitude          float64 // camera latitude, degrees north (for sun position)
	Longitude         float64 // camera longitude, degrees east (Seattle is negative)
	AuroraKpThreshold float64 // min NOAA Kp index to flag an aurora candidate
	HighlightPinCount int     // top-N highlight frames kept on local disk indefinitely

	// Resource hardening.
	MaxInflightRequests int // cap on concurrent in-flight HTTP requests; <=0 disables
}

var knownEnvVars = map[string]bool{
	"WS_TLS_ENABLE":                true,
	"WS_DOMAIN":                    true,
	"WS_TLS_PORT":                  true,
	"WS_CERT_DIR":                  true,
	"WS_ACME_EMAIL":                true,
	"WS_WU_API_KEY":                true,
	"WS_WU_STATION_ID":             true,
	"WS_RTSP_STREAM":               true,
	"WS_IMAGE_DIR":                 true,
	"WS_DB_PATH":                   true,
	"WS_HTTP_PORT":                 true,
	"WS_REFRESH_SECS":              true,
	"WS_MAILTRAP_API_TOKEN":        true,
	"WS_ALERT_EMAIL_TO":            true,
	"WS_ALERT_EMAIL_FROM":          true,
	"WS_IMAGE_DISK_LIMIT":          true,
	"WS_IMAGE_RETENTION_DAYS":      true,
	"WS_IMAGE_ARCHIVE_DEST":        true,
	"WS_IMAGE_ARCHIVE_EVERY_HOURS": true,
	"WS_LATITUDE":                  true,
	"WS_LONGITUDE":                 true,
	"WS_AURORA_KP_THRESHOLD":       true,
	"WS_HIGHLIGHT_PIN_COUNT":       true,
	"WS_MAX_INFLIGHT_REQUESTS":     true,
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

		TLSEnable: getEnvBool("WS_TLS_ENABLE", false),
		Domain:    os.Getenv("WS_DOMAIN"),
		TLSPort:   getEnv("WS_TLS_PORT", "8443"),
		CertDir:   getEnv("WS_CERT_DIR", "/var/lib/weatherstation/certs"),
		ACMEEmail: os.Getenv("WS_ACME_EMAIL"),

		ImageDiskLimit:         getEnvSize("WS_IMAGE_DISK_LIMIT", "5GB"),
		ImageRetentionDays:     getEnvInt("WS_IMAGE_RETENTION_DAYS", 90),
		ImageArchiveDest:       os.Getenv("WS_IMAGE_ARCHIVE_DEST"),
		ImageArchiveEveryHours: getEnvInt("WS_IMAGE_ARCHIVE_EVERY_HOURS", 24),

		// Defaults point at the camera's location (Seattle, Capitol Hill).
		Latitude:          getEnvFloat("WS_LATITUDE", 47.62),
		Longitude:         getEnvFloat("WS_LONGITUDE", -122.32),
		AuroraKpThreshold: getEnvFloat("WS_AURORA_KP_THRESHOLD", 6),
		HighlightPinCount: getEnvInt("WS_HIGHLIGHT_PIN_COUNT", 300),

		MaxInflightRequests: getEnvInt("WS_MAX_INFLIGHT_REQUESTS", 64),
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

func getEnvBool(key string, fallback bool) bool {
	if val := os.Getenv(key); val != "" {
		if b, err := strconv.ParseBool(val); err == nil {
			return b
		}
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	if val := os.Getenv(key); val != "" {
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f
		}
	}
	return fallback
}

// parseSize parses a human-readable byte size: "5GB", "500MB", "1.5G", or a
// plain byte count "5368709120". Binary units (1 GB == 1024 MB),
// case-insensitive, optional space. Suffixes: B, K/KB/KIB, M/MB/MIB, G/GB/GIB,
// T/TB/TIB.
func parseSize(s string) (int64, error) {
	u := strings.ToUpper(strings.TrimSpace(s))
	if u == "" {
		return 0, fmt.Errorf("empty size")
	}
	i := 0
	for i < len(u) && (u[i] >= '0' && u[i] <= '9' || u[i] == '.') {
		i++
	}
	num, err := strconv.ParseFloat(strings.TrimSpace(u[:i]), 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q: %w", s, err)
	}
	var mult float64
	switch strings.TrimSpace(u[i:]) {
	case "", "B":
		mult = 1
	case "K", "KB", "KIB":
		mult = 1 << 10
	case "M", "MB", "MIB":
		mult = 1 << 20
	case "G", "GB", "GIB":
		mult = 1 << 30
	case "T", "TB", "TIB":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("unknown size unit in %q", s)
	}
	return int64(num * mult), nil
}

// getEnvSize reads a human-readable byte size from the environment, falling back
// to the given default. It fatals on an unparseable value (mirrors the existing
// fail-fast config style).
func getEnvSize(key, fallback string) int64 {
	raw := os.Getenv(key)
	if raw == "" {
		raw = fallback
	}
	n, err := parseSize(raw)
	if err != nil {
		log.Fatalf("invalid %s=%q: %v", key, raw, err)
	}
	return n
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

	log.Println("starting weatherstation\n" + versionInfo())

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
		config:    cfg,
		clock:     cl,
		al:        alerter,
		db:        db,
		wu:        wu,
		sw:        NewSpaceWeatherClient(), // NOAA SWPC needs no API key
		thumbSem:  make(chan struct{}, maxConcurrentThumbs),
		startTime: cl.Now(),
	}
	if cfg.MaxInflightRequests > 0 {
		wss.inflightSem = make(chan struct{}, cfg.MaxInflightRequests)
	}

	// A leftover run marker means the previous run was killed (crash/OOM/watchdog)
	// rather than shut down cleanly — alert once, now that systemd (not the
	// fork-monitor) supervises. Then claim the marker for this run.
	if priorUncleanExit(cfg) {
		if err := alerter.Fire("weatherstation recovered",
			"previous run exited uncleanly (crash, OOM-kill, or watchdog) and was auto-restarted"); err != nil {
			slog.Error("recovery alert failed", "err", err)
		}
	}
	writeRunMarker(cfg)

	// Root context, cancelled on SIGINT/SIGTERM, drives graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// App handler: /healthz stays outside the in-flight limiter so it always
	// answers, even while the app sheds load with 503s.
	handler := buildHandler(wss, NewAPIMux(wss))

	var servers []*http.Server
	if cfg.TLSEnable {
		if cfg.Domain == "" {
			log.Fatal("WS_DOMAIN is required when WS_TLS_ENABLE=true")
		}
		// TLS terminated in-process via Let's Encrypt. Certs are obtained lazily
		// on the first TLS handshake and cached under cfg.CertDir.
		m := &autocert.Manager{
			Cache:      autocert.DirCache(cfg.CertDir),
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.Domain),
			Email:      cfg.ACMEEmail, // "" is allowed
		}
		// HTTP listener: answers ACME HTTP-01 challenges and 302-redirects
		// everything else to HTTPS. External:80 -> Pi:WS_HTTP_PORT.
		acme := &http.Server{
			Addr:              ":" + cfg.HTTPPort,
			Handler:           m.HTTPHandler(nil),
			ReadHeaderTimeout: 10 * time.Second, // slowloris guard
		}
		// HTTPS listener: serves the app. External:443 -> Pi:WS_TLS_PORT.
		https := newHTTPServer(":"+cfg.TLSPort, handler)
		https.TLSConfig = m.TLSConfig()
		servers = append(servers, acme, https)
		go serve(acme, "ACME/redirect HTTP", acme.ListenAndServe)
		go serve(https, "HTTPS", func() error { return https.ListenAndServeTLS("", "") })
	} else {
		// Plain HTTP — local dev and the fallback case.
		plain := newHTTPServer(":"+cfg.HTTPPort, handler)
		servers = append(servers, plain)
		go serve(plain, "HTTP", plain.ListenAndServe)
	}

	// systemd integration + observability.
	notifyReady()
	go watchdogLoop(ctx, wss.healthy)
	go wss.metricsLoop(ctx)

	// Background loops (capture/poll/score/lifecycle) run until ctx is cancelled.
	bgDone := make(chan struct{})
	go func() { wss.backgroundLoop(ctx); close(bgDone) }()

	<-ctx.Done()
	stop() // restore default handling so a second signal force-quits
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil {
			slog.Warn("server shutdown", "addr", s.Addr, "err", err)
		}
	}
	select {
	case <-bgDone:
	case <-time.After(10 * time.Second):
		slog.Warn("background loops did not stop in time")
	}
	removeRunMarker(cfg)
	slog.Info("shutdown complete")
}

// newHTTPServer builds an http.Server with conservative timeouts so slow or stuck
// clients can't tie up goroutines/connections indefinitely. WriteTimeout sits
// above the /thumb worst case (a cold 15s convert) with margin.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// serve runs a listener, treating the post-Shutdown ErrServerClosed as clean.
func serve(srv *http.Server, name string, listen func() error) {
	slog.Info("starting "+name+" server", "addr", srv.Addr)
	if err := listen(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("%s server failed: %v", name, err)
	}
}
