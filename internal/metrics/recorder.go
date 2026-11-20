// Package metrics keeps the figures of a running server: counters of the
// requests since the start, the downloads per repository and day and the
// requests per day of catalog.sqlite (decisions L11 and L12 of the phase 7
// plan), and the samples of metrics.sqlite.
package metrics

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/logship"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// Logger names the lines of this package.
const Logger = "pipesmith_registry.metrics"

// FlushEvery is the time between two writes of the daily counts (L11).
const FlushEvery = 10 * time.Second

// Usage is where the daily counts go: the tables downloads and request_days
// of catalog.sqlite. *catalog.Catalog is one.
type Usage interface {
	AddUsage(downloads []catalog.Download, days []catalog.RequestDay) error
}

// Options configure a Recorder.
type Options struct {
	Clock  tasks.Clock  // nil is the clock of the machine
	Usage  Usage        // nil keeps no daily counts
	Logger *slog.Logger // nil logs nothing
	// Targets are the counters of the log targets; nil for none.
	Targets func() []logship.Stats
}

// Totals are the counters since the start of the process.
type Totals struct {
	Requests      int64            `json:"requests"`
	BytesReceived int64            `json:"bytesReceived"`
	BytesSent     int64            `json:"bytesSent"`
	Status        map[string]int64 `json:"status"`
	Downloads     int64            `json:"downloads"`
	DownloadBytes int64            `json:"downloadBytes"`
	FlushFailures int64            `json:"flushFailures"`
}

// statusClasses are the keys of Totals.Status, by the first digit.
var statusClasses = [...]string{"1xx", "2xx", "3xx", "4xx", "5xx"}

type downloadKey struct{ repo, day string }

type tally struct{ count, bytes int64 }

// Recorder counts the records of the requests and writes the daily counts
// every FlushEvery and at Close. Its methods are safe for use by several
// goroutines; the methods of a nil Recorder do nothing.
type Recorder struct {
	clock   tasks.Clock
	usage   Usage
	log     *slog.Logger
	targets func() []logship.Stats

	requests, received, sent, downloads, downloadBytes, failures atomic.Int64
	status                                                       [len(statusClasses)]atomic.Int64

	mu      sync.Mutex // the tallies, timer and closed
	pending map[downloadKey]tally
	days    map[string]tally
	timer   tasks.Timer
	samples *Store // the samples of Series; nil while they are off
	closed  bool
	flushMu sync.Mutex // one flush at a time, so counts reach the store in order
}

// NewRecorder starts a Recorder; with a Usage it writes every FlushEvery.
func NewRecorder(opts Options) *Recorder {
	r := &Recorder{clock: opts.Clock, usage: opts.Usage, log: opts.Logger, targets: opts.Targets,
		pending: map[downloadKey]tally{}, days: map[string]tally{}}
	if r.clock == nil {
		r.clock = tasks.RealClock()
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	r.log = r.log.With(slog.String("logger", Logger))
	if r.usage != nil {
		r.mu.Lock()
		r.schedule()
		r.mu.Unlock()
	}
	return r
}

// schedule arms the next flush; r.mu is held.
func (r *Recorder) schedule() {
	r.timer = r.clock.AfterFunc(FlushEvery, func() {
		_ = r.Flush() // a failure is counted and logged by Flush
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.closed {
			r.schedule()
		}
	})
}

// Request counts the record of one answered request. download is true for
// an answer that counts as a download by L11; it is counted for the
// repository of the record on the day of the clock.
func (r *Recorder) Request(rec logs.Request, download bool) {
	if r == nil {
		return
	}
	r.requests.Add(1)
	r.received.Add(rec.RequestBytes)
	r.sent.Add(rec.ResponseBytes)
	if class := rec.Status/100 - 1; class >= 0 && class < len(statusClasses) {
		r.status[class].Add(1)
	}
	counted := download && rec.Repository != ""
	if counted {
		r.downloads.Add(1)
		r.downloadBytes.Add(rec.ResponseBytes)
	}
	if r.usage == nil {
		return
	}
	day := r.clock.Now().UTC().Format(catalog.DayLayout)
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.days[day]
	d.count++
	d.bytes += rec.ResponseBytes
	r.days[day] = d
	if counted {
		k := downloadKey{rec.Repository, day}
		t := r.pending[k]
		t.count++
		t.bytes += rec.ResponseBytes
		r.pending[k] = t
	}
}

// Totals are the counters since the start.
func (r *Recorder) Totals() Totals {
	t := Totals{Status: map[string]int64{}}
	for _, class := range statusClasses {
		t.Status[class] = 0
	}
	if r == nil {
		return t
	}
	t.Requests, t.BytesReceived, t.BytesSent = r.requests.Load(), r.received.Load(), r.sent.Load()
	t.Downloads, t.DownloadBytes, t.FlushFailures = r.downloads.Load(), r.downloadBytes.Load(), r.failures.Load()
	for i, class := range statusClasses {
		t.Status[class] = r.status[i].Load()
	}
	return t
}

// Flush writes the counts that wait. Counts a write refuses wait for the
// next flush.
func (r *Recorder) Flush() error {
	if r == nil || r.usage == nil {
		return nil
	}
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	r.mu.Lock()
	pending, days := r.pending, r.days
	r.pending, r.days = map[downloadKey]tally{}, map[string]tally{}
	r.mu.Unlock()
	if len(pending) == 0 && len(days) == 0 {
		return nil
	}
	downloads := make([]catalog.Download, 0, len(pending))
	for k, t := range pending {
		downloads = append(downloads, catalog.Download{Repo: k.repo, Day: k.day, Count: t.count, Bytes: t.bytes})
	}
	rows := make([]catalog.RequestDay, 0, len(days))
	for day, t := range days {
		rows = append(rows, catalog.RequestDay{Day: day, Requests: t.count, BytesSent: t.bytes})
	}
	err := r.usage.AddUsage(downloads, rows)
	if err == nil {
		return nil
	}
	r.failures.Add(1)
	r.log.Warn("the download counts are not written; they wait for the next flush", slog.String("error", err.Error()))
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, t := range pending {
		have := r.pending[k]
		r.pending[k] = tally{have.count + t.count, have.bytes + t.bytes}
	}
	for day, t := range days {
		have := r.days[day]
		r.days[day] = tally{have.count + t.count, have.bytes + t.bytes}
	}
	return err
}

// Close stops the timer and writes what waits. A second Close does nothing.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	return r.Flush()
}
