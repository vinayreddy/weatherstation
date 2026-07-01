package main

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestDBPoolMaxConns locks in the concurrent-read pool size (previously pinned
// to 1, which serialized every request behind any slow DB holder).
func TestDBPoolMaxConns(t *testing.T) {
	db := InitDB(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()
	if got := db.Stats().MaxOpenConnections; got != maxDBConns {
		t.Errorf("MaxOpenConnections = %d, want %d", got, maxDBConns)
	}
}

// TestDBConcurrentAccessNoBusy exercises many goroutines reading and writing at
// once. With the pool >1 this only stays SQLITE_BUSY-free because busy_timeout
// is set on every connection via the DSN (a post-open PRAGMA would configure
// just one connection). A regression there would surface as errors here.
func TestDBConcurrentAccessNoBusy(t *testing.T) {
	db := InitDB(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()

	base := time.Now().Unix()
	var wg sync.WaitGroup
	errs := make(chan error, 256)
	for g := range 16 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 20 {
				ts := base + int64(g*1000+i)
				if err := InsertObservation(db, &Observation{Timestamp: ts, Temp: float64(i)}); err != nil {
					errs <- err
					return
				}
				if _, err := QueryObservations(db, base, base+1_000_000); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent DB access errored (SQLITE_BUSY regression?): %v", err)
	}
}
