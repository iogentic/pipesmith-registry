package metrics

import (
	"bufio"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/logship"
)

// holdingStub is a receiver of http-json that counts the lines it took and
// holds every answer until release is closed.
type holdingStub struct {
	mu      sync.Mutex
	lines   int
	arrived chan struct{}
	release chan struct{}
}

func (h *holdingStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := 0
	scanner := bufio.NewScanner(r.Body)
	for scanner.Scan() {
		n++
	}
	h.mu.Lock()
	h.lines += n
	h.mu.Unlock()
	select {
	case h.arrived <- struct{}{}:
	default:
	}
	<-h.release
	w.WriteHeader(http.StatusNoContent)
}

func (h *holdingStub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lines
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10 s for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The queue depth and the drop counter of a log target in the figures equal
// what a receiver that holds its answer took and what the queue could not
// hold; once it answers, the depth is 0 and the sent lines equal the
// receiver's count.
func TestTargetFiguresEqualTheReceiver(t *testing.T) {
	stub := &holdingStub{arrived: make(chan struct{}, 1), release: make(chan struct{})}
	ts := httptest.NewServer(stub)
	defer ts.Close()
	row := fmt.Sprintf(`[{"name":"demo-json","kind":"http-json","address":%q,"queue_lines":3,"batch_lines":1,"flush_interval_ms":100}]`,
		ts.URL+"/ingest")
	m := logship.New(logship.Options{Rows: func() ([]byte, error) { return []byte(row), nil }})
	released := false
	defer func() {
		if !released {
			close(stub.release)
		}
		m.Close()
	}()
	r := NewRecorder(Options{Targets: m.Stats})
	line := func(n int) logs.Line {
		return logs.Line{Kind: "server", Time: time.Now(), Level: slog.LevelInfo, Logger: "pipesmith_registry.demo",
			Message: fmt.Sprint("line ", n), JSON: []byte(fmt.Sprintf(`{"msg":"line %d"}`, n))}
	}
	m.Ship(line(0))
	<-stub.arrived // the first batch is in flight and held
	const shipped = 10
	for n := 1; n <= shipped; n++ {
		m.Ship(line(n))
	}
	got := r.Targets()
	if len(got) != 1 || got[0].Name != "demo-json" || got[0].Kind != "http-json" {
		t.Fatalf("targets %+v", got)
	}
	// one line in flight and 3 in the queue; the rest found the queue full
	if got[0].QueueDepth != 1+3 || got[0].Dropped != shipped-3 || got[0].Queued != 1+3 || stub.count() != 1 {
		t.Fatalf("held: %+v, the receiver took %d", got[0], stub.count())
	}
	close(stub.release)
	released = true
	waitFor(t, "the queue to empty", func() bool { return r.Targets()[0].QueueDepth == 0 })
	got = r.Targets()
	if got[0].Sent != int64(stub.count()) || got[0].Sent != 1+3 || got[0].Dropped != shipped-3 {
		t.Fatalf("released: %+v, the receiver took %d", got[0], stub.count())
	}
	if none := NewRecorder(Options{}).Targets(); none == nil || len(none) != 0 {
		t.Fatalf("without targets %v", none)
	}
}
