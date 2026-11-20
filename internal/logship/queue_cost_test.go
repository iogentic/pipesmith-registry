package logship

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The queue holds what a plain list holds, across its blocks: pushes and
// takes of many sizes from a fixed seed, against a slice with the same
// bounds.
func TestQueueMatchesAList(t *testing.T) {
	const maxLines, maxBytes = 2000, 30_000
	rng := rand.New(rand.NewPCG(3, 2026))
	q := newQueue(maxLines, maxBytes)
	var list []logs.Line
	var held int64
	next := 0
	for step := 0; step < 4000; step++ {
		if rng.IntN(2) == 0 {
			for range rng.IntN(600) {
				l := logs.Line{Time: time.Unix(0, int64(next)), JSON: make([]byte, 1+rng.IntN(20))}
				next++
				fits := len(list) < maxLines && held+int64(len(l.JSON)) <= maxBytes
				if got := q.push(l); got != fits {
					t.Fatalf("step %d: push %v, the list says %v", step, got, fits)
				}
				if fits {
					list, held = append(list, l), held+int64(len(l.JSON))
				}
			}
		} else {
			k := rng.IntN(700)
			got := q.take(k)
			want := list[:min(k, len(list))]
			if len(got) != len(want) {
				t.Fatalf("step %d: took %d lines, the list holds %d of %d asked", step, len(got), len(want), k)
			}
			for i := range got {
				if !got[i].Time.Equal(want[i].Time) {
					t.Fatalf("step %d: line %d is %v, the list says %v", step, i, got[i].Time, want[i].Time)
				}
				held -= int64(len(want[i].JSON))
			}
			list = list[len(want):]
		}
		if q.len() != len(list) || q.bytes != held {
			t.Fatalf("step %d: %d lines and %d bytes, the list holds %d and %d", step, q.len(), q.bytes, len(list), held)
		}
	}
	if n := q.close(); n != len(list) || q.push(logs.Line{JSON: []byte("x")}) {
		t.Fatalf("close dropped %d of %d, or a closed queue took a line", n, len(list))
	}
}

// A queue of 1,000,000 short lines is drained in batches of 500 while a
// second goroutine keeps offering lines: the drain takes the 1,000,000 lines
// in their order. A push costs the same at any length of the queue: the
// same allocations at 1,024 and at 1,000,192 lines (checkPushCost). The
// times of the drain and of the slowest offer are logged, not checked, as
// the scheduler sets them.
func TestQueueCostDoesNotGrowWithItsLength(t *testing.T) {
	const fill, batch = 1_000_000, 500
	q := newQueue(fill, 256<<20)
	data := []byte(`{"time":"2026-11-19T10:00:00.000Z","level":"INFO","logger":"pipesmith_registry.demo","msg":"short"}`)
	line := func(n int) logs.Line { return logs.Line{Kind: "server", Time: time.Unix(0, int64(n)), JSON: data} }
	for i := range fill {
		if !q.push(line(i)) {
			t.Fatalf("line %d refused while the queue fills", i)
		}
	}
	stop, ended := make(chan struct{}), make(chan struct{})
	var slowest time.Duration
	offered := 0
	go func() {
		defer close(ended)
		for n := fill; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			q.push(line(n))
			slowest = max(slowest, time.Since(start))
			offered++
		}
	}()
	start := time.Now()
	taken := 0
	for taken < fill {
		for _, l := range q.take(batch) {
			if taken < fill && l.Time.UnixNano() != int64(taken) {
				close(stop)
				<-ended
				t.Fatalf("line %d of the fill came as line %d", l.Time.UnixNano(), taken)
			}
			taken++
		}
	}
	drain := time.Since(start)
	close(stop)
	<-ended
	t.Logf("drained %d lines in %v while %d offers ran; the slowest offer took %v", fill, drain, offered, slowest)
	checkPushCost(t)
}
