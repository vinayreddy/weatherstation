// Liveness, health reporting, and restart detection. healthy() is the single
// source of truth shared by the systemd watchdog (notify.go) and the /healthz
// endpoint, so an external monitor sees exactly what would trigger a restart.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"
)

// healthy reports whether the process is doing real work. It is deliberately
// cheap and side-effect-free so the watchdog can call it every ~20s. Two signals:
//   - the capture loop has iterated recently (a wedged loop goes stale), and
//   - the DB is reachable within 2s (a fully-wedged connection pool fails this).
//
// A zero capture stamp means "not started yet" — treated as healthy (startup grace).
func (ws *WeatherStationServer) healthy() bool {
	if last := ws.lastCaptureProgress.Load(); last != 0 {
		// Tolerate the error-path 60s backoff plus a full ffmpeg timeout.
		tol := max(int64(4*ws.config.RefreshSecs), 180)
		if ws.clock.Now().Unix()-last > tol {
			return false
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return ws.db.PingContext(ctx) == nil
}

// handleHealthz returns 200 when healthy() and 503 otherwise, with a diagnostic
// JSON body. systemd itself uses the watchdog (push), not this endpoint — this is
// for humans and external uptime monitors, and it reflects the same healthy().
func (ws *WeatherStationServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ok := ws.healthy()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	lastAge := int64(-1)
	if last := ws.lastCaptureProgress.Load(); last != 0 {
		lastAge = ws.clock.Now().Unix() - last
	}

	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(map[string]any{
		"healthy":            ok,
		"goroutines":         runtime.NumGoroutine(),
		"heapAllocMB":        m.HeapAlloc / (1 << 20),
		"lastCaptureAgeSecs": lastAge,
		"dbOpenConns":        ws.db.Stats().OpenConnections,
		"inFlight":           len(ws.inflightSem),
		"externalProcs":      externalProcs.Load(),
		"uptimeSecs":         int64(ws.clock.Now().Sub(ws.startTime).Seconds()),
	})
}

// metricsLoop logs a runtime snapshot every 5 minutes for postmortems. Returns
// when ctx is cancelled.
func (ws *WeatherStationServer) metricsLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			slog.Info("runtime metrics",
				"goroutines", runtime.NumGoroutine(),
				"heapAllocMB", m.HeapAlloc/(1<<20),
				"sysMB", m.Sys/(1<<20),
				"externalProcs", externalProcs.Load(),
				"inFlight", len(ws.inflightSem),
				"dbOpenConns", ws.db.Stats().OpenConnections,
			)
		}
	}
}

// --- restart marker --------------------------------------------------------
//
// A small file that exists while running and is removed on clean shutdown. If
// it's present at startup, the previous run exited uncleanly (crash, OOM-kill, or
// watchdog SIGABRT) — used to fire a one-shot recovery alert now that systemd,
// not the fork-monitor, supervises. It lives beside the DB (a persistent dir).

func runMarkerPath(cfg *Config) string { return cfg.DBPath + ".running" }

// priorUncleanExit reports whether the last run left its marker behind.
func priorUncleanExit(cfg *Config) bool {
	_, err := os.Stat(runMarkerPath(cfg))
	return err == nil
}

func writeRunMarker(cfg *Config) {
	p := runMarkerPath(cfg)
	if err := os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		slog.Warn("failed to write run marker", "path", p, "err", err)
	}
}

func removeRunMarker(cfg *Config) {
	if err := os.Remove(runMarkerPath(cfg)); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to remove run marker", "err", err)
	}
}
