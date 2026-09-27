package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// countingAlerter counts Fire calls.
type countingAlerter struct{ n int }

func (a *countingAlerter) Fire(title, msg string) error { a.n++; return nil }

// newBackupWSS returns a test server with backup enabled to local-path rsync
// destinations (rsync copies locally, no ssh). Returns the server, the image
// archive dest and the backup dest.
func newBackupWSS(t *testing.T, clk *FakeClock) (*WeatherStationServer, string, string) {
	t.Helper()
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not available")
	}
	ws, imgDir := newTestWSS(t, clk)
	root := filepath.Dir(imgDir)
	ws.config.DBPath = filepath.Join(root, "test.db") // newTestWSS's DB
	ws.config.ImageArchiveDest = filepath.Join(root, "archive")
	ws.config.BackupDest = filepath.Join(root, "backups")
	ws.config.BackupEveryHours = 24
	return ws, ws.config.ImageArchiveDest, ws.config.BackupDest
}

func TestBackupImagesPriorDaysOnlyAndAdditive(t *testing.T) {
	clk := NewFakeClock()
	clk.Set(time.Date(2026, 3, 15, 12, 0, 0, 0, ptLocation)) // Sunday
	ws, archive, _ := newBackupWSS(t, clk)

	writeImg(t, ws, time.Date(2026, 3, 13, 8, 0, 0, 0, ptLocation), 10)
	writeImg(t, ws, time.Date(2026, 3, 14, 9, 0, 0, 0, ptLocation), 10)
	writeImg(t, ws, time.Date(2026, 3, 15, 10, 0, 0, 0, ptLocation), 10) // today
	// A remote-only file (e.g. a frame since thinned on the Pi) must survive.
	remoteOnly := filepath.Join(archive, "live/2026/03/13", "070000.jpg")
	if err := os.MkdirAll(filepath.Dir(remoteOnly), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remoteOnly, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	ws.runImageLifecycleOnce()

	assertPresent(t, filepath.Join(archive, "live/2026/03/13", "080000.jpg"))
	assertPresent(t, filepath.Join(archive, "live/2026/03/14", "090000.jpg"))
	assertPresent(t, remoteOnly)
	if _, err := os.Stat(filepath.Join(archive, "live/2026/03/15")); !os.IsNotExist(err) {
		t.Errorf("today was backed up (stat err = %v); want skipped", err)
	}
	// Backup never removes local data.
	assertPresent(t, filepath.Join(ws.config.ImageDir, "live/2026/03/14", "090000.jpg"))
}

func TestBackupDBSnapshotAndConfig(t *testing.T) {
	clk := NewFakeClock()
	clk.Set(time.Date(2026, 3, 15, 12, 0, 0, 0, ptLocation)) // Sunday
	ws, _, backups := newBackupWSS(t, clk)
	writeImg(t, ws, time.Date(2026, 3, 14, 9, 0, 0, 0, ptLocation), 10)

	root := filepath.Dir(ws.config.DBPath)
	ws.config.EnvFile = filepath.Join(root, ".env.production")
	if err := os.WriteFile(ws.config.EnvFile, []byte("WS_WU_API_KEY=secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ws.config.TLSEnable = true
	ws.config.CertDir = filepath.Join(root, "certs")
	if err := os.MkdirAll(ws.config.CertDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.config.CertDir, "acme_account+key"), []byte("k"), 0600); err != nil {
		t.Fatal(err)
	}

	ws.runImageLifecycleOnce()

	for _, name := range []string{"weather-Sun.db", "weather-2026-03.db"} {
		path := filepath.Join(backups, "db", name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s missing: %v", name, err) // InitDB would log.Fatal on a missing dir
			continue
		}
		snap := InitDB(path)
		var n int
		if err := snap.QueryRow("SELECT COUNT(*) FROM images").Scan(&n); err != nil {
			t.Errorf("%s: query: %v", name, err)
		} else if n != 1 {
			t.Errorf("%s: images rows = %d, want 1", name, n)
		}
		snap.Close()
	}
	for _, rel := range []string{"config/.env.production", "config/certs/acme_account+key"} {
		info, err := os.Stat(filepath.Join(backups, rel))
		if err != nil {
			t.Errorf("%s missing: %v", rel, err)
			continue
		}
		if info.Mode().Perm() != 0600 {
			t.Errorf("%s mode = %v, want 0600", rel, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(root, backupStagingDir)); !os.IsNotExist(err) {
		t.Errorf("staging dir left behind (stat err = %v)", err)
	}
	if kvGet(ws.db, kvLastBackupRun) == "" {
		t.Error("cursor not advanced after successful backup")
	}
}

func TestBackupMissingConfigIsNotAnError(t *testing.T) {
	clk := NewFakeClock()
	clk.Set(time.Date(2026, 3, 15, 12, 0, 0, 0, ptLocation))
	ws, _, backups := newBackupWSS(t, clk)
	ws.config.EnvFile = filepath.Join(t.TempDir(), "missing.env")
	ws.config.TLSEnable = true
	ws.config.CertDir = filepath.Join(t.TempDir(), "no-certs")

	ws.runBackup(filepath.Join(ws.config.ImageDir, "live"), time.Date(2026, 3, 15, 0, 0, 0, 0, ptLocation))

	assertPresent(t, filepath.Join(backups, "db", "weather-Sun.db"))
	if kvGet(ws.db, kvLastBackupRun) == "" {
		t.Error("cursor not advanced; missing env/certs should not fail the backup")
	}
}

func TestBackupCadence(t *testing.T) {
	clk := NewFakeClock()
	clk.Set(time.Date(2026, 3, 15, 12, 0, 0, 0, ptLocation))
	ws, _, _ := newBackupWSS(t, clk)

	if !ws.backupDue() {
		t.Fatal("want due with no cursor")
	}
	ws.runBackup(filepath.Join(ws.config.ImageDir, "live"), time.Date(2026, 3, 15, 0, 0, 0, 0, ptLocation))
	if ws.backupDue() {
		t.Error("want not due right after a backup")
	}
	clk.Set(time.Date(2026, 3, 16, 11, 0, 0, 0, ptLocation))
	if ws.backupDue() {
		t.Error("want not due 23h later")
	}
	clk.Set(time.Date(2026, 3, 16, 12, 0, 0, 0, ptLocation))
	if !ws.backupDue() {
		t.Error("want due 24h later")
	}

	ws.config.BackupDest = ""
	if ws.backupDue() {
		t.Error("want never due when WS_BACKUP_DEST is empty")
	}
}

func TestBackupFailureKeepsCursorAndThrottlesAlerts(t *testing.T) {
	clk := NewFakeClock()
	clk.Set(time.Date(2026, 3, 15, 12, 0, 0, 0, ptLocation))
	ws, _, _ := newBackupWSS(t, clk)
	al := &countingAlerter{}
	ws.al = al
	// Unresolvable .invalid host => rsync/ssh fails fast (BatchMode, no prompt).
	ws.config.BackupDest = "ws-test@host.invalid:/tmp/backups"
	writeImg(t, ws, time.Date(2026, 3, 14, 9, 0, 0, 0, ptLocation), 10)
	liveRoot := filepath.Join(ws.config.ImageDir, "live")
	today := time.Date(2026, 3, 15, 0, 0, 0, 0, ptLocation)

	ws.runBackup(liveRoot, today)
	if kvGet(ws.db, kvLastBackupRun) != "" {
		t.Error("cursor advanced despite failure")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(ws.config.DBPath), backupStagingDir)); !os.IsNotExist(err) {
		t.Errorf("staging dir left behind after failure (stat err = %v)", err)
	}
	// The image half still ran even though the DB push failed.
	assertPresent(t, filepath.Join(ws.config.ImageArchiveDest, "live/2026/03/14", "090000.jpg"))

	clk.Set(clk.Now().Add(time.Hour))
	ws.runBackup(liveRoot, today)
	if al.n != 1 {
		t.Errorf("alerts after 2 failures within 24h = %d, want 1", al.n)
	}
	clk.Set(clk.Now().Add(backupAlertEvery))
	ws.runBackup(liveRoot, today)
	if al.n != 2 {
		t.Errorf("alerts after streak passed 24h = %d, want 2", al.n)
	}

	// Recovery clears the throttle, so the next failure alerts immediately.
	ws.config.BackupDest = filepath.Join(filepath.Dir(ws.config.DBPath), "backups")
	ws.runBackup(liveRoot, today)
	ws.config.BackupDest = "ws-test@host.invalid:/tmp/backups"
	clk.Set(clk.Now().Add(time.Hour))
	ws.runBackup(liveRoot, today)
	if al.n != 3 {
		t.Errorf("alerts after recover-then-fail = %d, want 3", al.n)
	}
}
