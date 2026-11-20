package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// gate is a remote that holds every request until it is opened and counts
// how many it holds at once.
type gate struct {
	mu     sync.Mutex
	now    int
	max    int
	total  int
	open   chan struct{}
	server *httptest.Server
}

func newGate(t *testing.T) *gate {
	t.Helper()
	g := &gate{open: make(chan struct{})}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.now++
		g.total++
		g.max = max(g.max, g.now)
		g.mu.Unlock()
		<-g.open
		g.mu.Lock()
		g.now--
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "bytes of "+r.URL.Path)
	}))
	t.Cleanup(g.server.Close)
	return g
}

func (g *gate) counts() (now, most, total int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.now, g.max, g.total
}

// rawProxy is a proxy of the format raw whose remote is base.
func rawProxy(name, base string) repos.Repo {
	return repos.Repo{Name: name, Format: "raw", Type: "proxy", Attributes: map[string]any{"proxy": map[string]any{"remoteUrl": base}}}
}

// storing is a query whose store reads the body, counted.
func storing(repo repos.Repo, path string, stored *atomic.Int64) Query {
	return Query{Repo: repo, Path: path,
		Look: func() (*Cached, error) { return nil, nil },
		Store: func(f Fetched) error {
			if _, err := io.Copy(io.Discard, f.Body); err != nil {
				return err
			}
			stored.Add(1)
			return nil
		}}
}

// At most 16 fetches of one repository and 64 of the process run at once
// (decision 5.8); the others wait for a slot and all of them end stored. The
// test runs three times.
func TestFetchLimits(t *testing.T) {
	for run := 1; run <= 3; run++ {
		for _, c := range []struct {
			repositories, paths, want int
		}{{1, 40, DefaultPerRepository}, {5, 20, DefaultPerProcess}} {
			g := newGate(t)
			p := New(Config{})
			var stored atomic.Int64
			var wg sync.WaitGroup
			outcomes := make(chan Outcome, c.repositories*c.paths)
			for r := 0; r < c.repositories; r++ {
				repo := rawProxy(fmt.Sprintf("demo-proxy-%d", r), fmt.Sprintf("%s/r%d/", g.server.URL, r))
				for i := 0; i < c.paths; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						outcome, err := p.Get(context.Background(), storing(repo, fmt.Sprintf("files/f-%d.bin", i), &stored))
						if err != nil {
							outcome = -1
						}
						outcomes <- outcome
					}()
				}
			}
			deadline := time.Now().Add(20 * time.Second)
			for {
				if now, _, _ := g.counts(); now >= c.want || time.Now().After(deadline) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(300 * time.Millisecond) // time for a fetch past the limit to arrive
			now, most, _ := g.counts()
			close(g.open)
			wg.Wait()
			close(outcomes)
			for o := range outcomes {
				if o != Stored {
					t.Fatalf("run %d, %d repositories: an outcome %v", run, c.repositories, o)
				}
			}
			_, _, total := g.counts()
			if now != c.want || most != c.want || total != c.repositories*c.paths || stored.Load() != int64(total) {
				t.Fatalf("run %d, %d repositories of %d paths: %d held at once, at most %d, %d requests, %d stored; want %d at once",
					run, c.repositories, c.paths, now, most, total, stored.Load(), c.want)
			}
			t.Logf("run %d, %d repositories of %d paths: at most %d requests at once at the remote, %d in all", run, c.repositories,
				c.paths, most, total)
			p.Close()
		}
	}
}

// Fifty queries at once for one path make one request and one store; each
// gets the outcome of that fetch (decision 5.8). A query that arrives after
// the fetch finds the stored copy through Look and asks nothing.
func TestOneFlightPerPath(t *testing.T) {
	g := newGate(t)
	p := New(Config{})
	defer p.Close()
	repo := rawProxy("demo-proxy", g.server.URL+"/")
	var stored atomic.Int64
	var wg sync.WaitGroup
	outcomes := make([]Outcome, 50)
	for i := range outcomes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcomes[i], _ = p.Get(context.Background(), storing(repo, "files/once.bin", &stored))
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(g.open)
	wg.Wait()
	for i, o := range outcomes {
		if o != Stored {
			t.Fatalf("query %d: %v", i, o)
		}
	}
	if _, _, total := g.counts(); total != 1 || stored.Load() != 1 {
		t.Fatalf("%d requests, %d stores", total, stored.Load())
	}
	q := storing(repo, "files/once.bin", &stored)
	q.Look = func() (*Cached, error) { return &Cached{CreatedMS: time.Now().UnixMilli()}, nil }
	if o, err := p.Get(context.Background(), q); err != nil || o != Cache {
		t.Fatalf("after the fetch: %v %v", o, err)
	}
	if _, _, total := g.counts(); total != 1 {
		t.Fatalf("%d requests after the fetch", total)
	}
}

// A caller that leaves gets its context error at once; the fetch goes on and
// stores (decision 5.8).
func TestAFetchOutlivesItsCaller(t *testing.T) {
	g := newGate(t)
	p := New(Config{})
	defer p.Close()
	var stored atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.Get(ctx, storing(rawProxy("demo-proxy", g.server.URL+"/"), "files/left.bin", &stored))
		done <- err
	}()
	for {
		if now, _, _ := g.counts(); now == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("the caller that left: %v", err)
	}
	close(g.open)
	deadline := time.Now().Add(10 * time.Second)
	for stored.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the fetch did not store after its caller left")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Many first queries of one proxy at once may each make a client, and a
// change of the settings makes a new one while fetches run; a fetch uses the
// client its query got, never the field another query writes. The race
// detector checks it (the full check of leg 5b found the race). The queries
// alternate between two body limits, so the client changes all the time.
func TestFirstQueriesOfAProxyAtOnce(t *testing.T) {
	g := newGate(t)
	close(g.open)
	for round := 0; round < 20; round++ {
		p := New(Config{})
		repo := rawProxy("demo-proxy", g.server.URL+"/")
		var stored atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				q := storing(repo, fmt.Sprintf("files/r%d-%d.bin", round, i), &stored)
				q.Limit = int64(1000 + 1000*(i%2))
				_, _ = p.Get(context.Background(), q)
			}()
		}
		wg.Wait()
		p.Close()
		if stored.Load() != 32 {
			t.Fatalf("round %d: %d of 32 stored", round, stored.Load())
		}
	}
}
