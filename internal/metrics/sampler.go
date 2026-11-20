package metrics

import (
	"log/slog"
	rtmetrics "runtime/metrics"
	"sync"
	"syscall"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// SampleEvery is the time between two samples.
const SampleEvery = time.Minute

// Probe reads the CPU time the process used since it started and the
// memory it holds from the system now.
type Probe func() (cpu time.Duration, memoryBytes int64)

// ProcessProbe is the probe of this process: user plus system time of
// getrusage, and the total of runtime/metrics /memory/classes/total:bytes.
func ProcessProbe() (time.Duration, int64) {
	var cpu time.Duration
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		cpu = time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}
	sample := []rtmetrics.Sample{{Name: "/memory/classes/total:bytes"}}
	rtmetrics.Read(sample)
	var memory int64
	if sample[0].Value.Kind() == rtmetrics.KindUint64 {
		memory = int64(min(sample[0].Value.Uint64(), 1<<62))
	}
	return cpu, memory
}

// Sampler writes one sample of 1 minute every SampleEvery from the totals
// of a Recorder and a Probe, and compacts the file after each.
type Sampler struct {
	rec   *Recorder
	store *Store
	clock tasks.Clock
	probe Probe
	log   *slog.Logger

	mu      sync.Mutex
	timer   tasks.Timer
	stopped bool
	last    Totals
	lastCPU time.Duration
	start   time.Time
	errors  int64
}

// StartSampler starts the samples; nil clock and probe are those of the
// machine and the process.
func StartSampler(rec *Recorder, store *Store, clock tasks.Clock, probe Probe, log *slog.Logger) *Sampler {
	if clock == nil {
		clock = tasks.RealClock()
	}
	if probe == nil {
		probe = ProcessProbe
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Sampler{rec: rec, store: store, clock: clock, probe: probe, log: log.With(slog.String("logger", Logger))}
	s.last = rec.Totals()
	s.lastCPU, _ = probe()
	s.start = clock.Now().UTC().Truncate(SampleEvery)
	s.mu.Lock()
	s.timer = clock.AfterFunc(SampleEvery, s.tick)
	s.mu.Unlock()
	return s
}

func (s *Sampler) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	now := s.clock.Now().UTC()
	totals := s.rec.Totals()
	cpu, memory := s.probe()
	x := Sample{Resolution: Minute, At: s.start, Requests: totals.Requests - s.last.Requests,
		BytesSent: totals.BytesSent - s.last.BytesSent, BytesReceived: totals.BytesReceived - s.last.BytesReceived,
		CPU: max(cpu-s.lastCPU, 0), MemoryBytes: memory}
	s.last, s.lastCPU, s.start = totals, cpu, now.Truncate(SampleEvery)
	if err := s.store.Add(x); err != nil {
		s.fail(err)
	} else if err := s.store.Compact(now); err != nil {
		s.fail(err)
	}
	s.timer = s.clock.AfterFunc(SampleEvery, s.tick)
}

// fail counts a sample that did not reach the file; s.mu is held.
func (s *Sampler) fail(err error) {
	s.errors++
	s.log.Warn("a sample of the figures is not written", slog.String("error", err.Error()))
}

// Errors are the samples that did not reach the file.
func (s *Sampler) Errors() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errors
}

// Stop ends the samples; the interval in progress is not written.
func (s *Sampler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	if s.timer != nil {
		s.timer.Stop()
	}
}
