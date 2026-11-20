package logship

import (
	"runtime"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// costRounds is how often pushCost measures; it keeps the least count, as a
// goroutine left by another test can only add to a count of the heap.
const costRounds = 5

// pushCost is the heap allocations and bytes of n pushes onto a queue that
// holds length lines already. The counts come from runtime.MemStats, which
// the scheduler cannot change.
func pushCost(length, n int) (allocs, bytes uint64) {
	data := []byte(`{"time":"2026-11-19T10:00:00.000Z","level":"INFO","logger":"pipesmith_registry.demo","msg":"short"}`)
	line := logs.Line{Kind: "server", Time: time.Unix(0, 1), JSON: data}
	allocs, bytes = ^uint64(0), ^uint64(0)
	for range costRounds {
		q := newQueue(length+n, 1<<40)
		for range length {
			q.push(line)
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for range n {
			q.push(line)
		}
		runtime.ReadMemStats(&after)
		allocs = min(allocs, after.Mallocs-before.Mallocs)
		bytes = min(bytes, after.TotalAlloc-before.TotalAlloc)
		runtime.KeepAlive(q)
	}
	return allocs, bytes
}

// checkPushCost pushes 64 blocks of lines onto a queue of 1,024 lines and
// onto one of 1,000,192 lines, both at the start of a block: each push
// costs the same at both lengths, one new block per 256 lines and nothing
// else.
func checkPushCost(t *testing.T) {
	t.Helper()
	const n = 64 * blockLines
	shortAllocs, shortBytes := pushCost(4*blockLines, n)
	longAllocs, longBytes := pushCost(3907*blockLines, n)
	t.Logf("%d pushes: %d allocations and %d bytes at 1,024 lines, %d and %d at 1,000,192 lines",
		n, shortAllocs, shortBytes, longAllocs, longBytes)
	if shortAllocs != n/blockLines || longAllocs != shortAllocs || longBytes != shortBytes {
		t.Fatalf("%d pushes allocate %d times and %d bytes at 1,024 lines, %d times and %d bytes at 1,000,192 lines; want %d times at both and the same bytes",
			n, shortAllocs, shortBytes, longAllocs, longBytes, n/blockLines)
	}
}
