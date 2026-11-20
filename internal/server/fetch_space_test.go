package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// The free space floor of a fetch (fix part A of leg 5b, after the security
// assessment of the leg).

// A body without a declared length stops at the room above the floor (phase
// 4, f50450b), and a fetch takes that through ingest: a chunked answer of the
// remote larger than the room answers 502 and leaves no row, blob or work
// file, while a smaller one is stored.
func TestProxyChunkedFetchStopsAtTheFloor(t *testing.T) {
	const floor, room = 1 << 20, 4096
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: "demo-raw-proxy", format: "raw",
		attributes: map[string]any{"free_space_floor_bytes": floor, "httpclient": map[string]any{"autoBlock": false}}}}})
	u.srv.writes.freeSpace = func(string) (uint64, error) { return floor + room, nil }
	u.remote.Script(remoteBase("demo-raw-proxy")+"docs/big.bin", testkit.Chunked(strings.Repeat("c", 16*room)))
	u.remote.Script(remoteBase("demo-raw-proxy")+"docs/small.bin", testkit.Chunked(strings.Repeat("s", room/8)))
	if r := u.get("demo-raw-proxy", "docs/big.bin"); r.status != http.StatusBadGateway {
		t.Fatalf("a chunked body over the room: %d %q", r.status, r.body)
	}
	if row := u.row("demo-raw-proxy", "docs/big.bin"); row != nil {
		t.Fatalf("a row of the refused body: %+v", row)
	}
	if blobs, work := u.blobFiles(); len(blobs) != 0 || len(work) != 0 {
		t.Fatalf("blobs %v, work files %v", blobs, work)
	}
	if r := u.get("demo-raw-proxy", "docs/small.bin"); r.status != 200 || len(r.body) != room/8 {
		t.Fatalf("a chunked body within the room: %d %d bytes", r.status, len(r.body))
	}
}

// flipper is a body of left bytes that calls flip once it has given flipAt
// of them.
type flipper struct {
	left, given, flipAt int64
	flip                func()
}

func (f *flipper) Read(p []byte) (int, error) {
	if f.left == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), f.left, 64<<10)
	for i := range p[:n] {
		p[i] = 'f'
	}
	f.left -= n
	before := f.given
	f.given += n
	if before < f.flipAt && f.given >= f.flipAt {
		f.flip()
	}
	return int(n), nil
}

// The reader of a fetch looks at the free space of the write store after
// every fetchFloorStep bytes and ends the body with the refusal no-space at
// the first look that finds it below the floor of the repository: at most
// one step after the free space fell. A body with room enough passes whole.
func TestFetchFloorReaderStopsWithinAStep(t *testing.T) {
	const floor = 1 << 30
	for _, flipAt := range []int64{1, fetchFloorStep / 2, fetchFloorStep, fetchFloorStep*3/2 + 7} {
		var below atomic.Bool
		looks := 0
		free := func() (uint64, error) {
			looks++
			if below.Load() {
				return floor - 1, nil
			}
			return floor + 10<<30, nil
		}
		body := &flipper{left: 5 * fetchFloorStep, flipAt: flipAt, flip: func() { below.Store(true) }}
		n, err := io.Copy(io.Discard, newFloorReader(body, free, floor, "demo-raw-proxy", "local-write"))
		var refusal *writeRefusal
		if !errors.As(err, &refusal) || refusal.rule != ruleNoSpace || refusal.status != http.StatusInsufficientStorage {
			t.Fatalf("flip at %d: %d bytes, %v", flipAt, n, err)
		}
		// the first look at or after the fall: the next whole step
		if want := (flipAt + fetchFloorStep - 1) / fetchFloorStep * fetchFloorStep; n != want || n-flipAt >= fetchFloorStep {
			t.Fatalf("flip at %d: the body ended after %d bytes, want %d; %d looks", flipAt, n, want, looks)
		}
		if !strings.Contains(refusal.message, "demo-raw-proxy") || !strings.Contains(refusal.message, "local-write") {
			t.Fatalf("the refusal %q", refusal.message)
		}
	}
	body := &flipper{left: 3*fetchFloorStep + 5, flipAt: -1, flip: func() {}}
	free := func() (uint64, error) { return floor, nil }
	if n, err := io.Copy(io.Discard, newFloorReader(body, free, floor, "demo-raw-proxy", "local-write")); err != nil || n != 3*fetchFloorStep+5 {
		t.Fatalf("room enough: %d %v", n, err)
	}
}

// streamRemote sends a body of total bytes in pieces of 1 MiB for every path,
// holds each body after gateAt bytes until gate is closed, and counts what
// it could write to each path.
type streamRemote struct {
	server         *httptest.Server
	gate           chan struct{}
	total, gateAt  int
	held           atomic.Int32
	mu             sync.Mutex
	written        map[string]int
	declaredLength bool
}

func newStreamRemote(t *testing.T, total, gateAt int) *streamRemote {
	t.Helper()
	s := &streamRemote{gate: make(chan struct{}), total: total, gateAt: gateAt, written: map[string]int{}}
	piece := bytes.Repeat([]byte("p"), 1<<20)
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		sent := 0
		defer func() {
			s.mu.Lock()
			s.written[r.URL.Path] = sent
			s.mu.Unlock()
		}()
		for sent < s.total {
			if sent == s.gateAt {
				s.held.Add(1)
				<-s.gate
			}
			n, err := w.Write(piece[:min(len(piece), s.total-sent)])
			sent += n
			if err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(func() {
		select {
		case <-s.gate:
		default:
			close(s.gate)
		}
		s.server.Close()
	})
	return s
}

// Fetches that run at the same time do not pass the floor together: once
// the free space of the write store falls below the floor of the repository
// while they stream, each one ends within fetchFloorStep bytes, answers 502
// and leaves no row, blob or work file, and the log line names the rule. A
// fetch of a repository with room enough is untouched.
func TestProxyFetchesStopWhenTheFloorIsPassed(t *testing.T) {
	log := captureLog(t)
	const floor = 1 << 30
	const total, gateAt = 4 * fetchFloorStep, 2 << 20
	remote := newStreamRemote(t, total, gateAt)
	at := func(name string) map[string]any {
		return map[string]any{"remoteUrl": remote.server.URL + "/" + name + "/"}
	}
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{
		{name: "demo-raw-proxy", format: "raw", attributes: map[string]any{"proxy": at("demo-raw-proxy"), "free_space_floor_bytes": floor,
			"httpclient": map[string]any{"autoBlock": false}}},
		{name: "demo-raw-roomy", format: "raw", attributes: map[string]any{"proxy": at("demo-raw-roomy"), "free_space_floor_bytes": 0}},
	}})
	var below atomic.Bool
	u.srv.writes.freeSpace = func(string) (uint64, error) {
		if below.Load() {
			return floor - 1, nil
		}
		return floor + 10<<30, nil
	}
	targets := [][2]string{{"demo-raw-proxy", "big/a.bin"}, {"demo-raw-proxy", "big/b.bin"}, {"demo-raw-proxy", "big/c.bin"},
		{"demo-raw-roomy", "big/d.bin"}}
	type got struct {
		r   *answer
		err error
	}
	answers := make([]chan got, len(targets))
	for i, target := range targets {
		answers[i] = make(chan got, 1)
		go func() {
			r, err := u.tryGet(target[0], target[1])
			answers[i] <- got{r, err}
		}()
	}
	deadline := time.Now().Add(20 * time.Second)
	for remote.held.Load() < int32(len(targets)) {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d bodies reached the gate", remote.held.Load(), len(targets))
		}
		time.Sleep(5 * time.Millisecond)
	}
	below.Store(true)
	close(remote.gate)
	for i, target := range targets {
		g := <-answers[i]
		if g.err != nil {
			t.Fatalf("%v: %v", target, g.err)
		}
		want := http.StatusBadGateway
		if target[0] == "demo-raw-roomy" {
			want = http.StatusOK
		}
		if g.r.status != want || want == http.StatusOK && len(g.r.body) != total {
			t.Fatalf("%v: %d with %d bytes, want %d", target, g.r.status, len(g.r.body), want)
		}
		if row := u.row(target[0], target[1]); (row != nil) != (want == http.StatusOK) {
			t.Fatalf("%v: row %+v", target, row)
		}
	}
	blobs, work := u.blobFiles()
	if len(blobs) != 2 || len(work) != 0 {
		t.Fatalf("blobs %v, work files %v; want the bytes and properties of the one stored fetch", blobs, work)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	const slack = 8 << 20 // what the socket buffers hold beyond the bytes the server read
	for _, target := range targets[:3] {
		if sent := remote.written["/"+target[0]+"/"+target[1]]; sent > gateAt+fetchFloorStep+slack || sent >= total {
			t.Fatalf("%v: the remote wrote %d of %d bytes", target, sent, total)
		}
	}
	text := strings.Join(log.all(), "\n")
	if n := strings.Count(text, "rule no-space"); n != 3 {
		t.Fatalf("%d log lines name the rule no-space, want 3:\n%s", n, text)
	}
}
