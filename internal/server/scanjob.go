package server

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/scan"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// scanStopTimeout is SCAN_STOP_SECONDS: how long Close waits for a scan
// to stop before it leaves the index to the process exit.
var scanStopTimeout = 10 * time.Second

func nowMS() int64 { return time.Now().UnixMilli() }

// scanRunner is scan.Run; a test gives a job another one.
type scanRunner func(s stores.Store, ix *index.Index, progress func(live int)) (scan.Result, error)

// scanCancelled is ScanCancelled: the server stops while a scan runs. The
// index keeps the rows of the previous scan.
type scanCancelled struct{ message string }

func (e *scanCancelled) Error() string { return e.message }

// cancellableStore is _CancellableStore: its walk stops with scanCancelled
// once cancel is set.
type cancellableStore struct {
	stores.Store
	cancel *atomic.Bool
}

func (c *cancellableStore) Entries(counts *stores.ScanCounts, yield func(stores.Entry) error) error {
	return c.Store.Entries(counts, func(e stores.Entry) error {
		if c.cancel.Load() {
			return &scanCancelled{"scan of store " + c.Name() + " cancelled"}
		}
		return yield(e)
	})
}

// scanJob is ScanJob: at most one scan at a time, in a goroutine of its
// own, beside the requests.
type scanJob struct {
	index     *index.Index
	takeOver  func() error // takes the new rows over into the read path
	cancelled atomic.Bool

	mu        sync.Mutex
	run       scanRunner
	running   bool
	store     string
	progress  int
	startedMS int64
	done      chan struct{} // closed when the goroutine of the last scan ends
	last      *pyjson.Object
}

func newScanJob(ix *index.Index, takeOver func() error) *scanJob {
	return &scanJob{index: ix, takeOver: takeOver, run: scan.Run, last: pyjson.NewObject()}
}

func (j *scanJob) setRun(run scanRunner) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.run = run
}

// start is false while a scan runs or the server stops.
func (j *scanJob) start(s stores.Store) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running || j.cancelled.Load() {
		return false
	}
	j.running, j.store, j.progress, j.startedMS = true, s.Name(), 0, nowMS()
	done := make(chan struct{})
	j.done = done
	go j.work(s, j.run, done)
	return true
}

func (j *scanJob) onProgress(live int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.progress = live
}

// work is _run: the outcome keeps the key order of the Python dict, which
// takes the fields of the result over in place and adds hints and error.
func (j *scanJob) work(s stores.Store, run scanRunner, done chan struct{}) {
	defer close(done)
	outcome := pyjson.NewObject()
	for _, key := range []string{"live", "deleted", "copies", "no_bytes", "unparsable", "seconds"} {
		outcome.Set(key, nil)
	}
	outcome.Set("store", s.Name())
	result, err := run(&cancellableStore{Store: s, cancel: &j.cancelled}, j.index, j.onProgress)
	var cancelled *scanCancelled
	switch {
	case err == nil:
		hints := result.Hints
		if hints == nil {
			hints = map[string]string{}
		}
		outcome.Set("store", result.Store).Set("live", result.Live).Set("deleted", result.Deleted).Set("copies", result.Copies).
			Set("no_bytes", result.NoBytes).Set("unparsable", result.Unparsable).Set("seconds", result.Seconds).
			Set("hints", hints).Set("error", nil)
	case errors.As(err, &cancelled):
		outcome.Set("error", "cancelled")
	default:
		logAt(slog.LevelError, serverLogger, "scan of store %s failed\n%s: %s", s.Name(), PyClass(err), PyText(err))
		text := PyText(err)
		if text == "" {
			text = PyClass(err)
		}
		outcome.Set("error", text)
	}
	outcome.Set("finished_ms", nowMS())
	if cancelled == nil {
		// Python lets a failure here end the thread and the job stays
		// running for good; the Go job logs it and records the outcome.
		if err := j.takeOver(); err != nil {
			logAt(slog.LevelError, serverLogger, "the scan of store %s is not taken over: %s: %s", s.Name(), PyClass(err), PyText(err))
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.last.Set(s.Name(), outcome)
	j.running, j.store = false, ""
}

// snapshot is the scan part of /api/status. An outcome is not changed once
// it is in last, so the copy shares them.
func (j *scanJob) snapshot() *pyjson.Object {
	j.mu.Lock()
	defer j.mu.Unlock()
	last := pyjson.NewObject()
	for name, outcome := range j.last.All() {
		last.Set(name, outcome)
	}
	state := pyjson.NewObject().Set("running", j.running)
	if j.running {
		state.Set("store", j.store).Set("progress", j.progress).Set("started_ms", j.startedMS)
	} else {
		state.Set("store", nil).Set("progress", nil).Set("started_ms", nil)
	}
	return state.Set("last", last)
}

// stop cancels a running scan and waits for it, at most timeout. It is true
// when no scan goroutine is left. After it no scan starts.
func (j *scanJob) stop(timeout time.Duration) bool {
	j.cancelled.Store(true)
	j.mu.Lock()
	done := j.done
	j.mu.Unlock()
	if done == nil {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
