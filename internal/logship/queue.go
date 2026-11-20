package logship

import (
	"sync"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// blockLines is the number of lines one block of a queue holds.
const blockLines = 256

// queue is the bounded queue of one target, in memory only. Its lines sit
// in a list of blocks: push writes into the last block and take reads from
// the first, so neither copies the rest of the queue. A block read to its
// end is kept for the next block push needs, and dropped when one is kept
// already. A line counts its JSON bytes against the byte bound.
type queue struct {
	mu       sync.Mutex
	head     *block // the block of the oldest line
	tail     *block // the block that takes the next line
	first    int    // the index of the oldest line in head
	n        int    // the lines held
	bytes    int64
	maxLines int
	maxBytes int64
	closed   bool
	spare    *block // a block read to its end, kept for the next block push needs
}

// block is one part of a queue: lines[:used] were pushed, the lines of head
// before queue.first were taken.
type block struct {
	lines [blockLines]logs.Line
	used  int
	next  *block
}

func newQueue(maxLines int, maxBytes int64) *queue {
	return &queue{maxLines: maxLines, maxBytes: maxBytes}
}

// push appends a line and reports whether it fits: a full or closed queue
// drops the new line (L7).
func (q *queue) push(line logs.Line) bool {
	size := int64(len(line.JSON))
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.n >= q.maxLines || q.bytes+size > q.maxBytes {
		return false
	}
	if q.tail == nil || q.tail.used == blockLines {
		b := q.spare
		if b == nil {
			b = &block{}
		}
		q.spare = nil
		if q.tail == nil {
			q.head = b
		} else {
			q.tail.next = b
		}
		q.tail = b
	}
	q.tail.lines[q.tail.used] = line
	q.tail.used++
	q.n++
	q.bytes += size
	return true
}

// take removes up to max lines from the front, in their order; it copies the
// lines it returns and no others.
func (q *queue) take(max int) []logs.Line {
	n := min(max, q.len())
	if n == 0 {
		return nil
	}
	batch := make([]logs.Line, 0, n) // before the lock, so the collector work of an allocation never holds up push
	q.mu.Lock()
	defer q.mu.Unlock()
	n = min(n, q.n) // only lines that another take removed meanwhile are gone
	for len(batch) < n {
		b := q.head
		part := b.lines[q.first:min(b.used, q.first+n-len(batch))]
		batch = append(batch, part...)
		clear(part) // the queue keeps no reference to a line it gave away
		q.first += len(part)
		if q.first == b.used { // the block was read to its end
			if b == q.tail {
				b.used, q.first = 0, 0 // the queue is empty: its block takes the next lines from the start
			} else {
				q.head, q.first = b.next, 0
				b.used, b.next, q.spare = 0, nil, b // its lines were cleared as they were taken
			}
		}
	}
	for _, line := range batch {
		q.bytes -= int64(len(line.JSON))
	}
	q.n -= n
	return batch
}

func (q *queue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.n
}

// closeIfEmpty closes the queue when it holds no line and reports whether it
// did. A stopped sender ends with it, so a line that comes later finds the
// queue closed and goes to the sender that took the row over.
func (q *queue) closeIfEmpty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.n > 0 {
		return false
	}
	q.closed = true
	return true
}

func (q *queue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

// close refuses every later line and returns how many lines it dropped.
func (q *queue) close() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := q.n
	q.closed, q.head, q.tail, q.spare, q.first, q.n, q.bytes = true, nil, nil, nil, 0, 0, 0
	return n
}
