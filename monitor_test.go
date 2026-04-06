package main

import (
	"testing"
	"time"

	timeseries "github.com/codesuki/go-time-series"
)

// testAlerter records whether Fire was called and the last arguments.
type testAlerter struct {
	fired     bool
	lastTitle string
	lastMsg   string
}

func (a *testAlerter) Fire(title, msg string) error {
	a.fired = true
	a.lastTitle = title
	a.lastMsg = msg
	return nil
}

func TestGetSleepDuration(t *testing.T) {
	fc := NewFakeClock()
	ts, err := timeseries.NewTimeSeries(timeseries.WithClock(fc))
	if err != nil {
		t.Fatal(err)
	}

	// No crashes -> 10s (nc=0, falls through to default)
	fc.Increment(time.Second)
	if d := getSleepDuration(ts); d != 10*time.Second {
		t.Errorf("0 crashes: got %v, want 10s", d)
	}

	// 1 crash -> 1s
	fc.Increment(time.Second)
	ts.Increase(1)
	fc.Increment(time.Second)
	if d := getSleepDuration(ts); d != time.Second {
		t.Errorf("1 crash: got %v, want 1s", d)
	}

	// 2 crashes -> 5s
	fc.Increment(time.Second)
	ts.Increase(1)
	fc.Increment(time.Second)
	if d := getSleepDuration(ts); d != 5*time.Second {
		t.Errorf("2 crashes: got %v, want 5s", d)
	}

	// 3 crashes -> 10s
	fc.Increment(time.Second)
	ts.Increase(1)
	fc.Increment(time.Second)
	if d := getSleepDuration(ts); d != 10*time.Second {
		t.Errorf("3 crashes: got %v, want 10s", d)
	}
}

func TestCrashAlert_MaybeAlert(t *testing.T) {
	fc := NewFakeClock()
	al := &testAlerter{}
	ca := newCrashAlert(fc, al)

	// Fewer than 3 crashes should not fire
	fc.Increment(time.Second)
	ca.crashTs.Increase(1)
	fc.Increment(time.Second)
	ca.crashTs.Increase(1)
	fc.Increment(time.Second)
	if ca.maybeAlert("test", "err1") {
		t.Error("should not fire with only 2 crashes")
	}
	if al.fired {
		t.Error("alerter should not have been called")
	}

	// 3rd crash should fire
	fc.Increment(time.Second)
	ca.crashTs.Increase(1)
	fc.Increment(time.Second)
	if !ca.maybeAlert("test crashing", "err2") {
		t.Error("should fire after 3 crashes")
	}
	if !al.fired {
		t.Error("alerter should have been called")
	}
	if al.lastTitle != "test crashing" {
		t.Errorf("title = %q", al.lastTitle)
	}

	// Should not fire again within 1 hour
	al.fired = false
	fc.Increment(30 * time.Minute)
	ca.crashTs.Increase(1)
	fc.Increment(time.Second)
	if ca.maybeAlert("test", "err3") {
		t.Error("should not fire again within 1 hour")
	}

	// Should fire again after 1 hour, with enough recent crashes (>2 in 10 min)
	fc.Increment(31 * time.Minute) // total: 61 min from alert
	ca.crashTs.Increase(1)
	fc.Increment(time.Second)
	ca.crashTs.Increase(1)
	fc.Increment(time.Second)
	ca.crashTs.Increase(1)
	fc.Increment(time.Second)
	if !ca.maybeAlert("test", "err4") {
		t.Error("should fire again after 1 hour with >2 recent crashes")
	}
}

func TestCrashAlert_MaybeAlert_NoFire(t *testing.T) {
	fc := NewFakeClock()
	al := &testAlerter{}
	ca := newCrashAlert(fc, al)

	// Single crash should not trigger
	fc.Increment(time.Second)
	ca.crashTs.Increase(1)
	fc.Increment(time.Second)
	if ca.maybeAlert("test", "err") {
		t.Error("single crash should not trigger alert")
	}
	if al.fired {
		t.Error("alerter should not be called")
	}
}
