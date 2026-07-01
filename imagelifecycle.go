package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	lifecycleTick    = time.Hour
	rsyncTimeout     = 30 * time.Minute
	kvLastArchiveRun = "image_archive_last_run" // unix seconds, stored in the kv table
)

// dayDir is a single live/YYYY/MM/DD image directory and its Pacific-midnight date.
type dayDir struct {
	path string    // absolute, e.g. <ImageDir>/live/2026/03/01
	date time.Time // midnight PT of that day
}

// imageLifecycleLoop maintains the image store: it thins prior days to ~1 image
// per hour, archives+removes days older than the retention window, and enforces
// a hard disk cap. It runs once at startup, then every hour. The filesystem
// under <ImageDir>/live is the source of truth; DB rows are kept in sync.
func (ws *WeatherStationServer) imageLifecycleLoop(ctx context.Context) {
	ws.runImageLifecycleOnce()
	ticker := time.NewTicker(lifecycleTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ws.runImageLifecycleOnce()
		}
	}
}

func (ws *WeatherStationServer) runImageLifecycleOnce() {
	// A sweep error must never crash the supervised child.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("image lifecycle panicked", "recover", r)
		}
	}()

	liveRoot := filepath.Join(ws.config.ImageDir, "live")
	n := ws.clock.NowPacific()
	today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, ptLocation)

	// (a) Thin every prior day to ~1/hour — cheap, biggest win; runs hourly.
	if err := ws.thinPriorDays(liveRoot, today); err != nil {
		slog.Error("image thinning failed", "err", err)
	}
	// (b) Retention + archival, gated to the configured cadence.
	if ws.archiveDue() {
		if err := ws.enforceRetention(liveRoot, today); err != nil {
			slog.Error("image retention/archival failed", "err", err)
			// Leave the cursor unset so we retry next cycle.
		} else {
			kvSet(ws.db, kvLastArchiveRun, strconv.FormatInt(ws.clock.Now().Unix(), 10))
		}
	}
	// (c) Hard disk-cap safety net — runs hourly.
	if err := ws.enforceDiskCap(liveRoot, today); err != nil {
		slog.Error("image disk-cap enforcement failed", "err", err)
	}
}

// archiveDue reports whether the retention/archival sweep is due per the
// configured cadence (WS_IMAGE_ARCHIVE_EVERY_HOURS).
func (ws *WeatherStationServer) archiveDue() bool {
	if ws.config.ImageArchiveEveryHours <= 0 {
		return true
	}
	last, _ := strconv.ParseInt(kvGet(ws.db, kvLastArchiveRun), 10, 64)
	if last == 0 {
		return true
	}
	every := time.Duration(ws.config.ImageArchiveEveryHours) * time.Hour
	return ws.clock.Now().Sub(time.Unix(last, 0)) >= every
}

// thinPriorDays reduces every day before today to ~1 image per hour.
func (ws *WeatherStationServer) thinPriorDays(liveRoot string, today time.Time) error {
	days, err := listDayDirs(liveRoot)
	if err != nil {
		return err
	}
	for _, d := range days {
		if !d.date.Before(today) {
			continue // never touch today (or any future) — the capture loop owns it
		}
		if err := ws.thinDay(d); err != nil {
			slog.Error("thin day failed", "day", d.date.Format("2006-01-02"), "err", err)
		}
	}
	return nil
}

// thinDay keeps, in each hour, the highest-interest image (ties broken toward the
// earliest) plus any pinned frames, deleting the rest (file + row). Keeping the
// best frame rather than the earliest means a striking moment — an aurora, a
// lightning-lit sky — survives thinning instead of being dropped for whatever
// happened to capture first. Unscored frames all score 0, so this degrades to
// "keep earliest". Idempotent: a day already at <=1 file per hour is a no-op.
func (ws *WeatherStationServer) thinDay(d dayDir) error {
	entries, err := os.ReadDir(d.path)
	if err != nil {
		return err
	}
	meta, err := ImageMetaInRange(ws.db, d.date.Unix(), d.date.AddDate(0, 0, 1).Unix())
	if err != nil {
		return err
	}
	byHour := map[string][]string{} // "HH" -> filenames
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jpg") || len(name) < 6 {
			continue
		}
		byHour[name[:2]] = append(byHour[name[:2]], name)
	}
	for _, names := range byHour {
		if len(names) <= 1 {
			continue
		}
		sort.Strings(names) // earliest HHMMSS first, so ties keep the earliest

		// Find the hour's best (highest score) frame.
		best := 0
		scores := make([]imgMeta, len(names))
		for i, name := range names {
			if ts, ok := fileTimestamp(d, name); ok {
				scores[i] = meta[ts]
			}
			if scores[i].Score > scores[best].Score {
				best = i
			}
		}
		for i, name := range names {
			if i == best || scores[i].Pinned {
				continue // keep the best frame and every pinned frame
			}
			rel := "live/" + d.date.Format("2006/01/02") + "/" + name // matches imagePath()
			if err := os.Remove(filepath.Join(d.path, name)); err != nil && !os.IsNotExist(err) {
				slog.Error("thin remove failed", "file", name, "err", err)
				continue
			}
			if err := DeleteImageByPath(ws.db, rel); err != nil {
				slog.Error("thin db delete failed", "path", rel, "err", err)
			}
			// Drop the cached thumbnail alongside its frame so the cache stays 1:1.
			if err := os.Remove(thumbPath(ws.config.ImageDir, rel)); err != nil && !os.IsNotExist(err) {
				slog.Error("thin thumb remove failed", "path", rel, "err", err)
			}
		}
	}
	return nil
}

// fileTimestamp maps an HHMMSS.jpg filename in a day directory to its unix time.
func fileTimestamp(d dayDir, name string) (int64, bool) {
	if len(name) < 6 {
		return 0, false
	}
	hh, e1 := strconv.Atoi(name[0:2])
	mm, e2 := strconv.Atoi(name[2:4])
	ss, e3 := strconv.Atoi(name[4:6])
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, false
	}
	return time.Date(d.date.Year(), d.date.Month(), d.date.Day(), hh, mm, ss, 0, ptLocation).Unix(), true
}

// enforceRetention archives+deletes every day older than the retention window.
func (ws *WeatherStationServer) enforceRetention(liveRoot string, today time.Time) error {
	if ws.config.ImageRetentionDays <= 0 {
		return nil
	}
	cutoff := today.AddDate(0, 0, -ws.config.ImageRetentionDays)
	days, err := listDayDirs(liveRoot)
	if err != nil {
		return err
	}
	sort.Slice(days, func(i, j int) bool { return days[i].date.Before(days[j].date) })
	for _, d := range days {
		if !d.date.Before(cutoff) {
			break // sorted oldest-first: nothing older remains
		}
		if err := ws.archiveAndDeleteDay(liveRoot, d); err != nil {
			return err // archive likely down; stop, retry next cycle, keep data
		}
	}
	return nil
}

// enforceDiskCap evicts oldest prior days until ImageDir is under the budget.
func (ws *WeatherStationServer) enforceDiskCap(liveRoot string, today time.Time) error {
	limit := ws.config.ImageDiskLimit
	if limit <= 0 {
		return nil
	}
	total, err := dirSize(ws.config.ImageDir)
	if err != nil {
		return err
	}
	if total <= limit {
		return nil
	}
	days, err := listDayDirs(liveRoot)
	if err != nil {
		return err
	}
	sort.Slice(days, func(i, j int) bool { return days[i].date.Before(days[j].date) })
	for _, d := range days {
		if total <= limit {
			break
		}
		if !d.date.Before(today) {
			break // never evict today
		}
		before, _ := dirSize(d.path)
		if err := ws.archiveAndDeleteDay(liveRoot, d); err != nil {
			return err // do not delete un-archived data on failure
		}
		after, _ := dirSize(d.path) // pinned frames may remain
		total -= before - after
	}
	if total > limit {
		slog.Warn("image dir still over budget after evicting all prior days",
			"total", total, "limit", limit)
	}
	return nil
}

// archiveAndDeleteDay pushes a day to the archive (if configured) and only then
// removes it locally. It never deletes un-archived data: if the rsync fails it
// returns an error and leaves the local bytes in place for the next cycle.
//
// Pinned frames (top highlights) are exempt from local removal: the whole day is
// still rsync'd so their bytes are safely at the archive destination, but the
// pinned files stay on local disk with is_archived = 0 so they remain viewable. The
// rest of the day is marked archived (or row-deleted) and removed as usual.
func (ws *WeatherStationServer) archiveAndDeleteDay(liveRoot string, d dayDir) error {
	if ws.config.ImageArchiveDest != "" {
		if err := ws.rsyncDay(d); err != nil {
			ws.al.Fire("Image archive push failed", fmt.Sprintf(
				"day %s -> %s: %v", d.date.Format("2006-01-02"), ws.config.ImageArchiveDest, err))
			return Wrap(err, "archive push")
		}
	}

	dayStart := d.date.Unix()
	dayEnd := d.date.AddDate(0, 0, 1).Unix()
	if ws.config.ImageArchiveDest != "" {
		if _, err := MarkImagesArchived(ws.db, dayStart, dayEnd); err != nil {
			return Wrap(err, "mark archived")
		}
	} else {
		if _, err := DeleteUnpinnedInRange(ws.db, dayStart, dayEnd); err != nil {
			return Wrap(err, "delete rows")
		}
	}

	// Remove local files, preserving any pinned frames.
	pinned, err := PinnedInRange(ws.db, dayStart, dayEnd)
	if err != nil {
		return Wrap(err, "load pinned")
	}
	if len(pinned) == 0 {
		if err := os.RemoveAll(d.path); err != nil {
			return Wrap(err, "remove day dir")
		}
		pruneEmptyParents(filepath.Dir(d.path), liveRoot)
	} else {
		keep := make(map[string]bool, len(pinned))
		for _, p := range pinned {
			keep[filepath.Base(p.Path)] = true
		}
		if err := removeDayFilesExcept(d.path, keep); err != nil {
			return Wrap(err, "remove day files")
		}
	}
	// Prune this day's cached thumbnails too. Removing the whole thumb day dir is
	// fine even when pinned frames remain locally: their few thumbnails simply
	// regenerate on demand. This keeps the cache from accumulating orphans, which
	// would otherwise count against the disk cap (cache/ lives under ImageDir).
	if err := os.RemoveAll(thumbDayDir(ws.config.ImageDir, d)); err != nil {
		slog.Error("thumb day cleanup failed", "day", d.date.Format("2006-01-02"), "err", err)
	}
	slog.Info("image day retired", "day", d.date.Format("2006-01-02"),
		"archived", ws.config.ImageArchiveDest != "", "pinned_kept", len(pinned))
	return nil
}

// removeDayFilesExcept deletes every .jpg in dir whose basename is not in keep.
func removeDayFilesExcept(dir string, keep map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || keep[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// rsyncDay copies one day directory to the archive over ssh. Arguments are
// passed as a slice (no shell, so the operator-controlled dest can't inject),
// ssh runs in BatchMode so it fails fast instead of prompting, and we never use
// --remove-source-files: local files are deleted only after rsync exits 0.
func (ws *WeatherStationServer) rsyncDay(d dayDir) error {
	rel := "live/" + d.date.Format("2006/01/02")
	src := ws.config.ImageDir + "/./" + rel // "/./" marks the -R relative root
	dest := strings.TrimRight(ws.config.ImageArchiveDest, "/") + "/"

	ctx, cancel := context.WithTimeout(context.Background(), rsyncTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rsync",
		"-aR",       // archive mode + recreate the relative path under dest
		"--partial", // keep partial files remote so an interrupted transfer resumes
		"-e", "ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new",
		src, dest)
	cmd.WaitDelay = 5 * time.Second
	out, err := runExternal(cmd)
	if err != nil {
		return Wrapf(err, "rsync: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// thumbDayDir returns the cached-thumbnail directory mirroring a live day dir,
// e.g. <ImageDir>/cache/thumb/live/2026/03/01. Removed wholesale when a day is
// retired so the thumb cache stays in sync with the live frames.
func thumbDayDir(imageDir string, d dayDir) string {
	return filepath.Join(imageDir, "cache", "thumb", "live", d.date.Format("2006/01/02"))
}

// listDayDirs returns every live/YYYY/MM/DD directory with its Pacific date.
// A missing liveRoot yields an empty slice, not an error.
func listDayDirs(liveRoot string) ([]dayDir, error) {
	matches, err := filepath.Glob(filepath.Join(liveRoot,
		"[0-9][0-9][0-9][0-9]", "[0-9][0-9]", "[0-9][0-9]"))
	if err != nil {
		return nil, err
	}
	out := make([]dayDir, 0, len(matches))
	for _, m := range matches {
		rel, err := filepath.Rel(liveRoot, m)
		if err != nil {
			continue
		}
		t, err := time.ParseInLocation("2006/01/02", filepath.ToSlash(rel), ptLocation)
		if err != nil {
			continue
		}
		out = append(out, dayDir{path: m, date: t})
	}
	return out, nil
}

// dirSize sums the size of every regular file under root. A missing root (or a
// file that vanishes mid-walk) counts as zero rather than erroring.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, de fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if de.IsDir() {
			return nil
		}
		info, err := de.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}

// pruneEmptyParents removes dir and its now-empty ancestors, stopping at (and
// never removing) stopAt. os.Remove fails on a non-empty dir, which ends the walk.
func pruneEmptyParents(dir, stopAt string) {
	for len(dir) > len(stopAt) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
