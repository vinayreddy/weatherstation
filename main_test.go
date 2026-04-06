package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FakeClock is a test Clock with a settable time.
type FakeClock struct {
	now time.Time
}

func NewFakeClock() *FakeClock          { return &FakeClock{now: time.Now()} }
func (c *FakeClock) Set(t time.Time)    { c.now = t }
func (c *FakeClock) Now() time.Time     { return c.now }
func (c *FakeClock) NowPacific() time.Time { return c.now.In(ptLocation) }
func (c *FakeClock) Increment(d time.Duration) { c.now = c.now.Add(d) }

func TestLoadConfig(t *testing.T) {
	t.Setenv("WS_OPENWEATHER_API_KEY", "test-key-123")
	t.Setenv("WS_RTSP_STREAM", "rtsp://cam:554/stream")
	t.Setenv("WS_IMAGE_DIR", "/tmp/images")
	t.Setenv("WS_REFRESH_SECS", "60")
	t.Setenv("WS_MAILTRAP_API_TOKEN", "mt-token")
	t.Setenv("WS_ALERT_EMAIL_TO", "alert@example.com")
	t.Setenv("WS_ALERT_EMAIL_FROM", "ws@example.com")

	cfg := LoadConfig()

	if cfg.OpenWeatherAPIKey != "test-key-123" {
		t.Errorf("OpenWeatherAPIKey = %q, want %q", cfg.OpenWeatherAPIKey, "test-key-123")
	}
	if cfg.RTSPStream != "rtsp://cam:554/stream" {
		t.Errorf("RTSPStream = %q", cfg.RTSPStream)
	}
	if cfg.ImageDir != "/tmp/images" {
		t.Errorf("ImageDir = %q", cfg.ImageDir)
	}
	if cfg.RefreshSecs != 60 {
		t.Errorf("RefreshSecs = %d, want 60", cfg.RefreshSecs)
	}
	if cfg.MailtrapAPIToken != "mt-token" {
		t.Errorf("MailtrapAPIToken = %q", cfg.MailtrapAPIToken)
	}
	if cfg.AlertEmailTo != "alert@example.com" {
		t.Errorf("AlertEmailTo = %q", cfg.AlertEmailTo)
	}
	if cfg.AlertEmailFrom != "ws@example.com" {
		t.Errorf("AlertEmailFrom = %q", cfg.AlertEmailFrom)
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	// Clear all WS_ vars to test defaults
	for k := range knownEnvVars {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	cfg := LoadConfig()

	if cfg.RefreshSecs != 30 {
		t.Errorf("RefreshSecs default = %d, want 30", cfg.RefreshSecs)
	}
	if cfg.AlertEmailFrom != "weatherstation@localhost" {
		t.Errorf("AlertEmailFrom default = %q, want %q", cfg.AlertEmailFrom, "weatherstation@localhost")
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

	// Ensure vars don't exist
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

	// Cleanup
	os.Unsetenv("WS_TEST_VAR1")
	os.Unsetenv("WS_TEST_VAR2")
	os.Unsetenv("WS_TEST_VAR3")
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
