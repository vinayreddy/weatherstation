// Fork-and-monitor: the parent forks a child and restarts it on crashes.
// To kill the whole process tree (parent + child), use kill -9 -<PGID>.
// The PGID is the parent's PID.
package main

import (
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	timeseries "github.com/codesuki/go-time-series"
)

var (
	cpid        = -1              // child pid
	childEnvVar = "CHILD_PROCESS" // prevents the child from forking again
)

// MaybeForkAndMonitor forks the binary and monitors the child. Returns true if
// the parent forked (caller should return). Returns false if this is the child
// or monitoring is disabled.
func MaybeForkAndMonitor(c Clock, al Alerter, enabled bool, exitAfterAlert bool) bool {
	if !enabled || os.Getenv(childEnvVar) != "" {
		return false
	}

	pid := os.Getpid()
	// Forward signals to the child process
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGABRT, syscall.SIGHUP)
		for s := range sig {
			slog.Info("parent received signal", "pid", pid, "signal", s)
			if cpid > 0 {
				slog.Info("forwarding signal to child", "pid", pid, "signal", s, "cpid", cpid)
				syscall.Kill(cpid, s.(syscall.Signal))
			}
			cpid = -1
			slog.Info("parent exiting", "pid", pid)
			syscall.Exit(0)
		}
	}()

	ca := newCrashAlert(c, al)
	for {
		err := forkAndWait(c)
		if err == nil {
			break
		}
		ca.crashTs.Increase(1)
		alerted := ca.maybeAlert(fmt.Sprintf("%s crashing...", os.Args[0]), err.Error())
		if exitAfterAlert && alerted {
			break
		}
		time.Sleep(getSleepDuration(ca.crashTs))
	}
	return true
}

type crashAlert struct {
	crashTs  *timeseries.TimeSeries
	lastFire time.Time
	al       Alerter
	clock    Clock
}

func newCrashAlert(c Clock, al Alerter) *crashAlert {
	ts, err := timeseries.NewTimeSeries(timeseries.WithClock(c))
	if err != nil {
		log.Fatalf("Failed to create time series: %v", err)
	}
	return &crashAlert{
		crashTs: ts,
		al:      al,
		clock:   c,
	}
}

// maybeAlert fires an alert if there have been >2 crashes in the last 10
// minutes and the last alert was more than 1 hour ago. Returns true if fired.
func (a *crashAlert) maybeAlert(title, errMsg string) bool {
	d := 10 * time.Minute
	now := a.clock.Now()
	nc, err := a.crashTs.Recent(d)
	var text string
	if err != nil {
		text = fmt.Sprintf("Crash time-series broken, err: %v", err)
	} else if nc > 2 && now.Sub(a.lastFire) > time.Hour {
		text = fmt.Sprintf("%d crashes in the last 10 minutes, last crash err: %s", int(nc), errMsg)
	} else {
		return false
	}
	a.lastFire = now
	if err := a.al.Fire(title, text); err != nil {
		return false
	}
	return true
}

func getSleepDuration(crashTs *timeseries.TimeSeries) time.Duration {
	d := 30 * time.Second
	ncf, err := crashTs.Recent(d)
	nc := int(ncf)
	if err != nil {
		return 5 * time.Second
	} else if nc == 1 {
		return time.Second
	} else if nc == 2 {
		return 5 * time.Second
	}
	return 10 * time.Second
}

func forkAndWait(c Clock) error {
	startTs := c.Now()
	pid := os.Getpid()
	pwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get working directory: %v", err)
	}
	childEnv := []string{fmt.Sprintf("%s=1", childEnvVar)}
	args := append(os.Args, "-fork_and_monitor=false")
	cpid, err = syscall.ForkExec(args[0], args, &syscall.ProcAttr{
		Dir:   pwd,
		Env:   append(os.Environ(), childEnv...),
		Files: []uintptr{0, 1, 2},
	})
	if cpid == 0 || err != nil {
		log.Fatalf("Failed to fork child process, args: %v, pid: %d, cpid: %d, pwd: %v, err: %v",
			args, pid, cpid, pwd, err)
	}
	slog.Info("forked child", "parent_pid", pid, "child_pid", cpid)

	p, _ := os.FindProcess(cpid)
	s, err := p.Wait()
	ec := -1
	if s != nil {
		ec = s.ExitCode()
	}
	if ec != 0 || err != nil {
		endTs := c.Now()
		d := endTs.Sub(startTs)
		err = fmt.Errorf("child process (PID[%d]) exited, uptime: %v, process state: %v, err: %v",
			cpid, d, s, err)
		cpid = -1
		slog.Error("child crashed", "err", err)
	}
	return err
}
