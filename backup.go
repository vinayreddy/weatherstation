package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	kvLastBackupRun   = "backup_last_run"   // unix seconds, stored in the kv table
	kvLastBackupAlert = "backup_last_alert" // unix seconds of the last failure alert; "" after a success
	backupAlertEvery  = 24 * time.Hour      // re-alert at most this often while backups keep failing
	backupStagingDir  = "backup-staging"    // created next to the DB, removed after every run
)

// backupDue reports whether backup is enabled (WS_BACKUP_DEST set) and due per
// the configured cadence (WS_BACKUP_EVERY_HOURS).
func (ws *WeatherStationServer) backupDue() bool {
	if ws.config.BackupDest == "" {
		return false
	}
	return ws.cadenceDue(kvLastBackupRun, ws.config.BackupEveryHours)
}

// runBackup copies off-box everything a dead SD card would take with it: a
// consistent DB snapshot and the env/cert config (to BackupDest), and every kept
// prior day of images (to ImageArchiveDest, so the archive host holds a single
// image tree). It is additive only — nothing is ever deleted remotely — and on
// failure the cursor stays put so the next hourly tick retries.
func (ws *WeatherStationServer) runBackup(liveRoot string, today time.Time) {
	dbBytes, dbErr := ws.backupDBAndConfig()
	days, imgErr := ws.backupImages(liveRoot, today)
	if err := errors.Join(dbErr, imgErr); err != nil {
		slog.Error("backup failed", "err", err)
		ws.alertBackupFailure(err)
		return
	}
	kvSet(ws.db, kvLastBackupRun, strconv.FormatInt(ws.clock.Now().Unix(), 10))
	kvSet(ws.db, kvLastBackupAlert, "") // a new failure streak alerts immediately
	slog.Info("backup complete", "db_bytes", dbBytes, "image_days", days)
}

// backupDBAndConfig stages a DB snapshot and the config next to the DB, then
// pushes both in one rsync. Remote layout under BackupDest:
//
//	db/weather-Mon.db … weather-Sun.db  7 rolling daily snapshots
//	db/weather-2026-09.db               last snapshot of each month, kept
//	config/<env file>, config/certs/…
func (ws *WeatherStationServer) backupDBAndConfig() (int64, error) {
	stage := filepath.Join(filepath.Dir(ws.config.DBPath), backupStagingDir)
	if err := os.RemoveAll(stage); err != nil {
		return 0, Wrap(err, "clear backup staging")
	}
	defer os.RemoveAll(stage)

	dbBytes, err := ws.stageDBSnapshot(filepath.Join(stage, "db"), ws.clock.NowPacific())
	if err != nil {
		return 0, Wrap(err, "stage db snapshot")
	}
	if err := ws.stageConfig(filepath.Join(stage, "config")); err != nil {
		return 0, Wrap(err, "stage config")
	}
	if err := rsyncRelative(ws.config.BackupDest, stage, "db", "config"); err != nil {
		return 0, Wrap(err, "db/config backup push")
	}
	return dbBytes, nil
}

// stageDBSnapshot writes weather-<Weekday>.db into dir and hard-links it as
// weather-<YYYY-MM>.db, returning the snapshot size.
func (ws *WeatherStationServer) stageDBSnapshot(dir string, now time.Time) (int64, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, err
	}
	daily := filepath.Join(dir, "weather-"+now.Format("Mon")+".db")
	// VACUUM INTO writes a consistent, compacted copy from a single read
	// transaction, so it is safe while the capture loop keeps writing (WAL mode).
	if _, err := ws.db.Exec("VACUUM INTO ?", daily); err != nil {
		return 0, Wrap(err, "vacuum into")
	}
	if err := os.Link(daily, filepath.Join(dir, "weather-"+now.Format("2006-01")+".db")); err != nil {
		return 0, Wrap(err, "link monthly snapshot")
	}
	info, err := os.Stat(daily)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// stageConfig copies the env file and (when TLS is on) the autocert cache into
// dir, preserving modes so the 0600 keys stay 0600 remotely. Either one missing
// is not an error; dir is always created so the rsync source exists.
func (ws *WeatherStationServer) stageConfig(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if f := ws.config.EnvFile; f != "" {
		if err := copyFile(f, filepath.Join(dir, filepath.Base(f))); err != nil && !os.IsNotExist(err) {
			return Wrap(err, "copy env file")
		}
	}
	if !ws.config.TLSEnable || ws.config.CertDir == "" {
		return nil
	}
	entries, err := os.ReadDir(ws.config.CertDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return Wrap(err, "read cert dir")
	}
	for _, e := range entries {
		if e.IsDir() {
			continue // autocert's DirCache is flat
		}
		src := filepath.Join(ws.config.CertDir, e.Name())
		if err := copyFile(src, filepath.Join(dir, "certs", e.Name())); err != nil && !os.IsNotExist(err) {
			return Wrapf(err, "copy cert %s", e.Name())
		}
	}
	return nil
}

// backupImages pushes every prior day (already thinned) to the image archive in
// one rsync. Today is skipped: it is still full-res and is picked up tomorrow.
func (ws *WeatherStationServer) backupImages(liveRoot string, today time.Time) (int, error) {
	if ws.config.ImageArchiveDest == "" {
		return 0, nil
	}
	days, err := listDayDirs(liveRoot)
	if err != nil {
		return 0, Wrap(err, "list day dirs")
	}
	var rels []string
	for _, d := range days {
		if d.date.Before(today) {
			rels = append(rels, "live/"+d.date.Format("2006/01/02"))
		}
	}
	if len(rels) == 0 {
		return 0, nil
	}
	if err := rsyncRelative(ws.config.ImageArchiveDest, ws.config.ImageDir, rels...); err != nil {
		return 0, Wrap(err, "image backup push")
	}
	return len(rels), nil
}

// alertBackupFailure fires at most once per backupAlertEvery during a failure
// streak, so an archive host that is down for a day doesn't page every hour.
func (ws *WeatherStationServer) alertBackupFailure(err error) {
	now := ws.clock.Now()
	last, _ := strconv.ParseInt(kvGet(ws.db, kvLastBackupAlert), 10, 64)
	if last != 0 && now.Sub(time.Unix(last, 0)) < backupAlertEvery {
		return
	}
	ws.al.Fire("Backup failed", fmt.Sprintf("%v\n\nRetrying hourly; re-alerting every %v while it keeps failing.",
		err, backupAlertEvery))
	kvSet(ws.db, kvLastBackupAlert, strconv.FormatInt(now.Unix(), 10))
}

// copyFile copies src to dst (creating dst's parent), preserving src's mode bits.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
