package proxy

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// Finding 5-2 of review 5: the slots limit the fetches that run at once,
// and the fetches that wait for a slot have a bound of their own, so a
// client that asks for many new paths and leaves cannot queue work without
// end. A HEAD waits for its slot only as long as its client does.

func TestFlightsAreBounded(t *testing.T) {
	g := &flights{limit: 3}
	release := make(chan struct{})
	var ran atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the callers leave at once; the fetches stay
	for i := range 3 {
		if _, err := g.do(ctx, flightKey{"demo-proxy", fmt.Sprintf("f-%d", i)}, func() result {
			ran.Add(1)
			<-release
			return result{kind: resultStored}
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("flight %d: %v", i, err)
		}
	}
	res, err := g.do(context.Background(), flightKey{"demo-proxy", "f-past"}, func() result {
		ran.Add(1)
		return result{kind: resultStored}
	})
	if err != nil || res.kind != resultFailed || !errors.Is(res.err, ErrBusy) {
		t.Fatalf("a flight past the bound: %+v %v", res, err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for g.count() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if res, err := g.do(context.Background(), flightKey{"demo-proxy", "f-after"}, func() result {
		return result{kind: resultStored}
	}); err != nil || res.kind != resultStored {
		t.Fatalf("a flight after the others ended: %+v %v", res, err)
	}
	if n := ran.Load(); n != 3 {
		t.Fatalf("%d fetches of the bounded keys ran, want 3", n)
	}
}

func TestHeadWaitsForItsSlotOnlyWhileItsClientDoes(t *testing.T) {
	g := newGate(t)
	p := New(Config{PerProcess: 1})
	defer p.Close()
	repo := rawProxy("demo-proxy", g.server.URL+"/")
	var stored atomic.Int64
	go func() { _, _ = p.Get(context.Background(), storing(repo, "files/held.bin", &stored)) }()
	deadline := time.Now().Add(5 * time.Second)
	for now, _, _ := g.counts(); now == 0 && time.Now().Before(deadline); now, _, _ = g.counts() {
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := p.Head(ctx, Query{Repo: repo, Path: "files/other.bin"})
	waited := time.Since(start)
	close(g.open)
	if !errors.Is(err, context.DeadlineExceeded) || waited > 2*time.Second {
		t.Fatalf("HEAD behind a full slot: %v after %v", err, waited)
	}
}
