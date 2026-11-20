package logship

import (
	"context"
	"flag"
	"os"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// A receiver holds the lines of a batch before its answer reaches the
// sender, and the sender counts the batch only after that answer. A test
// that waits for the lines at a stub and then reads the counters must first
// wait until the sender counted every line (waitCounted).

// countDelay delays the return of every Send of every adapter of the
// package by its value, so each batch is counted that long after its stub
// holds it: go test ./internal/logship -args -count-delay=300ms
var countDelay = flag.Duration("count-delay", 0, "real time each Send of an adapter waits before it returns")

// sendLate delays the start of every Send of every adapter of the package
// by its value, as the work in front of a dial does on a loaded machine:
// go test ./internal/logship -args -send-late=300ms
var sendLate = flag.Duration("send-late", 0, "real time each Send of an adapter waits before it starts")

func TestMain(m *testing.M) {
	flag.Parse()
	if *countDelay > 0 {
		for kind := range openers {
			slowCount(kind, *countDelay)
		}
	}
	if *sendLate > 0 {
		for kind := range openers {
			lateSend(kind, *sendLate)
		}
	}
	os.Exit(m.Run())
}

// slowAdapter returns from Send delay after the adapter it wraps.
type slowAdapter struct {
	Adapter
	delay time.Duration
}

func (a slowAdapter) Send(ctx context.Context, batch []logs.Line) (Result, error) {
	res, err := a.Adapter.Send(ctx, batch)
	time.Sleep(a.delay)
	return res, err
}

// lateAdapter starts each Send of the adapter it wraps delay late, with the
// deadline of the attempt already running.
type lateAdapter struct {
	Adapter
	delay time.Duration
}

func (a lateAdapter) Send(ctx context.Context, batch []logs.Line) (Result, error) {
	time.Sleep(a.delay)
	return a.Adapter.Send(ctx, batch)
}

// slowCount wraps the opener of kind in slowAdapter and returns a function
// that puts the old opener back.
func slowCount(kind string, delay time.Duration) (restore func()) {
	return wrapOpener(kind, func(a Adapter) Adapter { return slowAdapter{Adapter: a, delay: delay} })
}

// lateSend wraps the opener of kind in lateAdapter and returns a function
// that puts the old opener back.
func lateSend(kind string, delay time.Duration) (restore func()) {
	return wrapOpener(kind, func(a Adapter) Adapter { return lateAdapter{Adapter: a, delay: delay} })
}

// wrapOpener makes the opener of kind wrap each adapter it opens and
// returns a function that puts the old opener back.
func wrapOpener(kind string, wrap func(Adapter) Adapter) (restore func()) {
	open := openers[kind]
	openers[kind] = func(t Target, secret string, e env) (Adapter, error) {
		a, err := open(t, secret, e)
		if err != nil {
			return nil, err
		}
		return wrap(a), nil
	}
	return func() { openers[kind] = open }
}

// waitCounted waits until the sender of target name counted n lines as
// sent, rejected or failed.
func waitCounted(t *testing.T, m *Manager, name string, n int) {
	t.Helper()
	waitFor(t, func() bool {
		st := statsOf(t, m, name)
		return st.Sent+st.Rejected+st.Failed >= int64(n)
	}, "the lines counted by the sender")
}

// The second batch of 500 reaches the stub 300 ms before the sender counts
// it; the counters read after shipThousand still hold all 1,000 lines.
func TestThousandCountedAfterTheStubHoldsThem(t *testing.T) {
	t.Cleanup(slowCount("loki", 300*time.Millisecond))
	lines := testLines("server", 0, 1000)
	stub := newLokiStub(t, "", false)
	m := shipThousand(t, "loki", stub, lines, nil)
	if st := statsOf(t, m, "demo"); st.Sent != 1000 || st.Failed != 0 || st.Errors != 0 || st.Attempts != 2 {
		t.Fatalf("%+v in %d requests", st, stub.count())
	}
}
