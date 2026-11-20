package logship

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The rules of a sender.
const (
	maxAttempts = 3                // attempts for one batch
	maxPause    = 30 * time.Second // the cap of a Retry-After
	stopGrace   = 2 * time.Second  // real time a stopped sender gets to send what is queued
	warnEvery   = time.Minute      // at most one warning per target in this time of the clock
)

// pauses are the waits before the second and the third attempt of a batch.
var pauses = [maxAttempts - 1]time.Duration{500 * time.Millisecond, 2 * time.Second}

// Stats are the counters of one running target.
type Stats struct {
	Target   string
	Kind     string
	Queued   int64 // lines the tap put in the queue
	Sent     int64 // lines the receiver took
	Dropped  int64 // lines the tap dropped because the queue was full
	Failed   int64 // lines given up: after 3 attempts, at a final answer, at the stop
	Rejected int64 // lines the receiver refused inside an accepted answer
	Cut      int64 // lines sent shortened (syslog over UDP)
	Attempts int64 // attempts to send a batch
	Errors   int64 // attempts that failed
}

// counters are the counts of one sender, read without a lock.
type counters struct {
	queued, sent, dropped, failed, rejected, cut, attempts, errors atomic.Int64
}

// sender is the queue of one target and the goroutine that sends it.
type sender struct {
	target  Target
	secret  string
	mask    uint8
	queue   *queue
	adapter Adapter
	clock   clock
	log     *slog.Logger
	counts  counters

	wake   chan struct{} // a line came; capacity 1
	halt   chan struct{} // closed by stop
	ctx    context.Context
	cancel context.CancelFunc // ends every attempt and pause; called 2 s after stop
	done   chan struct{}

	mu     sync.Mutex // halted and grace
	halted bool
	grace  *time.Timer

	next atomic.Pointer[sender] // the sender that took over a changed row; set before this one stops

	// The warning state, used by the goroutine of the sender only.
	warned  time.Time
	atWarn  [4]int64 // errors, failed, dropped and rejected at the last warning
	failure string   // the class of the last failure since the last warning
}

func newSender(t Target, secret string, a Adapter, clk clock, log *slog.Logger) *sender {
	ctx, cancel := context.WithCancel(context.Background())
	return &sender{target: t, secret: secret, mask: t.fileMask(), queue: newQueue(t.queueLines(), t.queueBytes()),
		adapter: a, clock: clk, log: log, wake: make(chan struct{}, 1), halt: make(chan struct{}), ctx: ctx,
		cancel: cancel, done: make(chan struct{})}
}

// offer is the part of the tap for one target: it queues the line, hands it
// over or drops it, and returns at once.
func (s *sender) offer(line logs.Line) {
	if !s.queue.push(line) {
		if !s.handOver(line) {
			s.counts.dropped.Add(1)
		}
		return
	}
	s.counts.queued.Add(1)
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// handOver gives a line that came after the queue of a changed row closed to
// the sender that took the row over, which queues it when its row takes the
// kind of the line. It is false while the queue is open, so the line found it
// full, and when no sender took over.
func (s *sender) handOver(line logs.Line) bool {
	next := s.next.Load()
	if next == nil || !s.queue.isClosed() {
		return false
	}
	if next.mask&fileBits[line.Kind] != 0 {
		next.offer(line)
	}
	return true
}

// stop makes the sender send what is queued and end; 2 s later its attempts
// are cancelled.
func (s *sender) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.halted {
		return
	}
	s.halted = true
	close(s.halt)
	s.grace = time.AfterFunc(stopGrace, s.cancel)
}

func (s *sender) stats() Stats {
	return Stats{Target: s.target.Name, Kind: s.target.Kind, Queued: s.counts.queued.Load(), Sent: s.counts.sent.Load(),
		Dropped: s.counts.dropped.Load(), Failed: s.counts.failed.Load(), Rejected: s.counts.rejected.Load(),
		Cut: s.counts.cut.Load(), Attempts: s.counts.attempts.Load(), Errors: s.counts.errors.Load()}
}

// run sends batches until the sender is stopped and its queue is empty, or
// its 2 s after the stop passed.
func (s *sender) run() {
	defer close(s.done)
	defer s.finish()
	stopping := false
	for {
		if !stopping {
			stopping = s.wait()
		}
		batch := s.queue.take(s.target.batchLines())
		if len(batch) == 0 {
			if stopping && s.queue.closeIfEmpty() {
				return
			}
			continue
		}
		s.deliver(batch)
		if s.ctx.Err() != nil {
			return
		}
	}
}

// wait waits until a batch is full, or the first line waited the flush
// interval, or the sender is stopped; it reports whether it was stopped.
func (s *sender) wait() bool {
	select {
	case <-s.halt:
		return true
	default:
	}
	for s.queue.len() == 0 {
		select {
		case <-s.wake:
		case <-s.halt:
			return true
		}
	}
	if s.queue.len() >= s.target.batchLines() {
		return false
	}
	flush := s.clock.After(s.target.flushInterval())
	for s.queue.len() < s.target.batchLines() {
		select {
		case <-s.wake:
		case <-flush:
			return false
		case <-s.halt:
			return true
		}
	}
	return false
}

// deliver sends one batch in at most 3 attempts. A failure that is not
// final pauses 500 ms and then 2 s of the clock, or what Retry-After asks.
func (s *sender) deliver(batch []logs.Line) {
	pending := batch
	for attempt := 1; ; attempt++ {
		now := s.clock.Now() // the time of the attempt, read before its counts change
		res, err := s.attempt(pending)
		rest := res.Retry
		if err != nil && len(rest) == 0 {
			rest = pending
		}
		s.counts.sent.Add(max(0, int64(len(pending)-len(rest)-res.Rejected-res.Failed)))
		s.counts.rejected.Add(int64(res.Rejected))
		s.counts.failed.Add(int64(res.Failed))
		s.counts.cut.Add(int64(res.Cut))
		if err != nil {
			s.counts.errors.Add(1)
			s.failure = classOf(err)
		}
		again := len(rest) > 0 && !final(err) && attempt < maxAttempts && s.ctx.Err() == nil
		if len(rest) > 0 && !again {
			s.counts.failed.Add(int64(len(rest)))
		}
		s.maybeWarn(now)
		if !again {
			return
		}
		if !s.pause(pauseOf(attempt, err)) {
			s.counts.failed.Add(int64(len(rest)))
			return
		}
		pending = rest
	}
}

// attempt hands lines to the adapter with the timeout of the target.
func (s *sender) attempt(lines []logs.Line) (Result, error) {
	s.counts.attempts.Add(1)
	ctx, cancel := context.WithTimeout(s.ctx, s.target.timeout())
	defer cancel()
	return s.adapter.Send(ctx, lines)
}

// pause waits d of the clock; false when the sender was cancelled.
func (s *sender) pause(d time.Duration) bool {
	select {
	case <-s.clock.After(d):
		return s.ctx.Err() == nil
	case <-s.ctx.Done():
		return false
	}
}

// pauseOf is the wait after a failed attempt: Retry-After, at most 30 s,
// wins over the fixed pause.
func pauseOf(attempt int, err error) time.Duration {
	var f *Failure
	if errors.As(err, &f) && f.RetryAfter > 0 {
		return min(f.RetryAfter, maxPause)
	}
	return pauses[attempt-1]
}

// maybeWarn logs one warning with the counts since the last one, unless the
// last one is less than a minute of the clock before now or nothing went
// wrong.
func (s *sender) maybeWarn(now time.Time) {
	if !s.warned.IsZero() && now.Sub(s.warned) < warnEvery {
		return
	}
	at := [4]int64{s.counts.errors.Load(), s.counts.failed.Load(), s.counts.dropped.Load(), s.counts.rejected.Load()}
	if at == s.atWarn {
		return
	}
	attrs := []any{slog.String("target", s.target.Name)}
	if s.failure != "" {
		attrs = append(attrs, slog.String("error", s.failure))
	}
	attrs = append(attrs, slog.Int64("errors", at[0]-s.atWarn[0]), slog.Int64("failed", at[1]-s.atWarn[1]),
		slog.Int64("dropped", at[2]-s.atWarn[2]), slog.Int64("rejected", at[3]-s.atWarn[3]))
	s.log.Warn("log target failing", attrs...)
	s.warned, s.atWarn, s.failure = now, at, ""
}

// finish counts the lines left in the queue as failed, closes the adapter and
// logs the stop line with the counters.
func (s *sender) finish() {
	s.mu.Lock()
	if s.grace != nil {
		s.grace.Stop()
	}
	s.mu.Unlock()
	s.cancel()
	s.counts.failed.Add(int64(s.queue.close()))
	_ = s.adapter.Close() // what the adapter holds is released; its error changes no count
	st := s.stats()
	s.log.Info("log target stopped", slog.String("target", st.Target), slog.Int64("queued", st.Queued),
		slog.Int64("sent", st.Sent), slog.Int64("dropped", st.Dropped), slog.Int64("failed", st.Failed),
		slog.Int64("rejected", st.Rejected), slog.Int64("cut", st.Cut), slog.Int64("attempts", st.Attempts))
}
