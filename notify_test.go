package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// unixgramListener creates a datagram socket under a short /tmp path (macOS caps
// unix socket paths at ~104 bytes, so t.TempDir() can be too long) and points
// NOTIFY_SOCKET at it.
func unixgramListener(t *testing.T) *net.UnixConn {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wsn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	t.Setenv("NOTIFY_SOCKET", sock)
	return conn
}

func TestSdNotify(t *testing.T) {
	// No socket configured → no-op, no error.
	os.Unsetenv("NOTIFY_SOCKET")
	if err := sdNotify("READY=1"); err != nil {
		t.Errorf("sdNotify with no socket should be a no-op, got %v", err)
	}

	conn := unixgramListener(t)
	if err := sdNotify("READY=1"); err != nil {
		t.Fatalf("sdNotify: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "READY=1" {
		t.Errorf("received %q, want READY=1", got)
	}
}

func TestWatchdogInterval(t *testing.T) {
	os.Unsetenv("WATCHDOG_PID")
	t.Setenv("WATCHDOG_USEC", "900000") // 0.9s → ping every 300ms
	if d, ok := watchdogInterval(); !ok || d != 300*time.Millisecond {
		t.Errorf("watchdogInterval = %v, %v; want 300ms, true", d, ok)
	}

	// WATCHDOG_PID naming another process → not ours.
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()+1))
	if _, ok := watchdogInterval(); ok {
		t.Error("foreign WATCHDOG_PID should yield ok=false")
	}
	// WATCHDOG_PID naming us → ours.
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()))
	if _, ok := watchdogInterval(); !ok {
		t.Error("our WATCHDOG_PID should yield ok=true")
	}
	// No WATCHDOG_USEC → disabled.
	os.Unsetenv("WATCHDOG_USEC")
	if _, ok := watchdogInterval(); ok {
		t.Error("missing WATCHDOG_USEC should yield ok=false")
	}
}

func TestWatchdogLoopPingsWhenHealthy(t *testing.T) {
	conn := unixgramListener(t)
	t.Setenv("WATCHDOG_USEC", "150000") // ping every 50ms
	os.Unsetenv("WATCHDOG_PID")

	go watchdogLoop(t.Context(), func() bool { return true })

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil {
		t.Fatalf("expected a watchdog ping: %v", err)
	}
	if got := string(buf[:n]); got != "WATCHDOG=1" {
		t.Errorf("ping = %q, want WATCHDOG=1", got)
	}
}

func TestWatchdogLoopSilentWhenUnhealthy(t *testing.T) {
	conn := unixgramListener(t)
	t.Setenv("WATCHDOG_USEC", "60000") // would ping every 20ms if healthy
	os.Unsetenv("WATCHDOG_PID")

	go watchdogLoop(t.Context(), func() bool { return false })

	// Over several ping intervals, no datagram should arrive.
	conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 64)
	if _, _, err := conn.ReadFromUnix(buf); err == nil {
		t.Error("watchdog pinged while unhealthy; it must withhold the ping")
	}
}
