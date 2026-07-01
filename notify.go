// systemd sd_notify + watchdog support. Hand-rolled over NOTIFY_SOCKET so there's
// no external dependency and it cross-compiles cleanly to arm64. All functions
// are no-ops when the process isn't running under a Type=notify systemd unit
// (NOTIFY_SOCKET / WATCHDOG_USEC unset), so local/dev runs are unaffected.
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strconv"
	"time"
)

// sdNotify sends a status line to systemd's notification socket. Returns nil
// (no-op) when NOTIFY_SOCKET is unset.
func sdNotify(state string) error {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil
	}
	// Abstract-namespace sockets are advertised with a leading '@' which stands
	// in for the NUL byte.
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

// notifyReady tells systemd startup is complete (required for Type=notify so the
// unit reaches "active" instead of timing out).
func notifyReady() {
	if err := sdNotify("READY=1"); err != nil {
		slog.Warn("sd_notify READY failed", "err", err)
	}
}

// watchdogInterval derives the heartbeat period from WATCHDOG_USEC (set by systemd
// when WatchdogSec= is configured). It pings at a third of the deadline for margin.
// ok is false when no watchdog is configured or WATCHDOG_PID names another process.
func watchdogInterval() (d time.Duration, ok bool) {
	usec := os.Getenv("WATCHDOG_USEC")
	if usec == "" {
		return 0, false
	}
	// systemd sets WATCHDOG_PID to the process it expects pings from; if it's set
	// and isn't us (e.g. inherited by a child), don't pretend to own the watchdog.
	if pidStr := os.Getenv("WATCHDOG_PID"); pidStr != "" {
		if pid, err := strconv.Atoi(pidStr); err == nil && pid != os.Getpid() {
			return 0, false
		}
	}
	us, err := strconv.ParseInt(usec, 10, 64)
	if err != nil || us <= 0 {
		return 0, false
	}
	return time.Duration(us) * time.Microsecond / 3, true
}

// watchdogLoop pings the systemd watchdog on an interval, but only while healthy()
// reports true. If the app wedges (capture loop stalled or DB pool unreachable),
// the pings stop and systemd SIGABRTs + restarts the service after WatchdogSec —
// recovering from *hangs* the crash-only supervisor never could. No-op when no
// watchdog is configured. Returns when ctx is cancelled.
func watchdogLoop(ctx context.Context, healthy func() bool) {
	interval, ok := watchdogInterval()
	if !ok {
		slog.Info("systemd watchdog not configured; heartbeat disabled")
		return
	}
	slog.Info("starting systemd watchdog heartbeat", "interval", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !healthy() {
				// Withhold the ping on purpose: systemd will restart us.
				slog.Error("unhealthy — withholding watchdog ping (systemd will restart)")
				continue
			}
			if err := sdNotify("WATCHDOG=1"); err != nil {
				slog.Warn("sd_notify WATCHDOG failed", "err", err)
			}
		}
	}
}
