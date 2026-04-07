package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FakeClock is a test Clock with a settable time.
type FakeClock struct {
	now time.Time
}

func NewFakeClock() *FakeClock                  { return &FakeClock{now: time.Now()} }
func (c *FakeClock) Set(t time.Time)            { c.now = t }
func (c *FakeClock) Now() time.Time             { return c.now }
func (c *FakeClock) NowPacific() time.Time      { return c.now.In(ptLocation) }
func (c *FakeClock) Increment(d time.Duration)  { c.now = c.now.Add(d) }

func TestLoadConfig(t *testing.T) {
	t.Setenv("WS_WU_API_KEY", "test-wu-key")
	t.Setenv("WS_WU_STATION_ID", "KWATEST1")
	t.Setenv("WS_RTSP_STREAM", "rtsp://cam:554/stream")
	t.Setenv("WS_IMAGE_DIR", "/tmp/images")
	t.Setenv("WS_DB_PATH", "/tmp/test.db")
	t.Setenv("WS_HTTP_PORT", "9090")
	t.Setenv("WS_REFRESH_SECS", "60")
	t.Setenv("WS_MAILTRAP_API_TOKEN", "mt-token")
	t.Setenv("WS_ALERT_EMAIL_TO", "alert@example.com")
	t.Setenv("WS_ALERT_EMAIL_FROM", "ws@example.com")

	cfg := LoadConfig()

	if cfg.WUApiKey != "test-wu-key" {
		t.Errorf("WUApiKey = %q", cfg.WUApiKey)
	}
	if cfg.WUStationID != "KWATEST1" {
		t.Errorf("WUStationID = %q", cfg.WUStationID)
	}
	if cfg.RTSPStream != "rtsp://cam:554/stream" {
		t.Errorf("RTSPStream = %q", cfg.RTSPStream)
	}
	if cfg.ImageDir != "/tmp/images" {
		t.Errorf("ImageDir = %q", cfg.ImageDir)
	}
	if cfg.DBPath != "/tmp/test.db" {
		t.Errorf("DBPath = %q", cfg.DBPath)
	}
	if cfg.HTTPPort != "9090" {
		t.Errorf("HTTPPort = %q", cfg.HTTPPort)
	}
	if cfg.RefreshSecs != 60 {
		t.Errorf("RefreshSecs = %d, want 60", cfg.RefreshSecs)
	}
	if cfg.MailtrapAPIToken != "mt-token" {
		t.Errorf("MailtrapAPIToken = %q", cfg.MailtrapAPIToken)
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	for k := range knownEnvVars {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	cfg := LoadConfig()

	if cfg.WUStationID != "KWASEATT3003" {
		t.Errorf("WUStationID default = %q", cfg.WUStationID)
	}
	if cfg.ImageDir != "./data/images" {
		t.Errorf("ImageDir default = %q", cfg.ImageDir)
	}
	if cfg.DBPath != "./data/weather.db" {
		t.Errorf("DBPath default = %q", cfg.DBPath)
	}
	if cfg.HTTPPort != "8080" {
		t.Errorf("HTTPPort default = %q", cfg.HTTPPort)
	}
	if cfg.RefreshSecs != 30 {
		t.Errorf("RefreshSecs default = %d, want 30", cfg.RefreshSecs)
	}
	if cfg.AlertEmailFrom != "weatherstation@localhost" {
		t.Errorf("AlertEmailFrom default = %q", cfg.AlertEmailFrom)
	}
}

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := `# Comment line
WS_TEST_VAR1=hello

WS_TEST_VAR2="quoted value"
WS_TEST_VAR3='single quoted'
`
	if err := os.WriteFile(envPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	os.Unsetenv("WS_TEST_VAR1")
	os.Unsetenv("WS_TEST_VAR2")
	os.Unsetenv("WS_TEST_VAR3")

	loadEnvFile(envPath)

	tests := []struct {
		key, want string
	}{
		{"WS_TEST_VAR1", "hello"},
		{"WS_TEST_VAR2", "quoted value"},
		{"WS_TEST_VAR3", "single quoted"},
	}
	for _, tt := range tests {
		if got := os.Getenv(tt.key); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.key, got, tt.want)
		}
	}

	os.Unsetenv("WS_TEST_VAR1")
	os.Unsetenv("WS_TEST_VAR2")
	os.Unsetenv("WS_TEST_VAR3")
}

func TestVersionInfo(t *testing.T) {
	BuildDate = "2026-04-07 15:47:17 UTC"
	BuildUser = "testuser"
	UnameInfo = "Linux testhost 6.1.0"
	GitCommit = "abc1234"
	GitBranch = "main"

	got := versionInfo()

	lines := strings.Split(got, "\n")
	if len(lines) != 5 {
		t.Fatalf("versionInfo() has %d lines, want 5:\n%s", len(lines), got)
	}

	want := []string{
		"Build Date: 2026-04-07 15:47:17 UTC",
		"Build User: testuser",
		"Uname Info: Linux testhost 6.1.0",
		"Git Commit: abc1234",
		"Git Branch: main",
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d = %q, want %q", i, lines[i], w)
		}
	}
}

func TestLoadEnvFile_NoOverride(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := `WS_TEST_EXISTING=new_value`
	if err := os.WriteFile(envPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WS_TEST_EXISTING", "original")
	loadEnvFile(envPath)

	if got := os.Getenv("WS_TEST_EXISTING"); got != "original" {
		t.Errorf("env var was overridden: got %q, want %q", got, "original")
	}
}
