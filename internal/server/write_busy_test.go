package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/scan"
)

// A write holds the lock of its path while it waits for the write lock of
// the index, which a scan holds for its whole walk. The wait is bounded
// (finding S6 of review 4): after indexWait the write answers 503 with
// Retry-After, its blob is not live, no work file stays, and the write taken
// again once the index is free succeeds. A delete does the same and marks
// nothing.
func TestABusyIndexAnswers503(t *testing.T) {
	indexWait.Store(int64(300 * time.Millisecond))
	t.Cleanup(func() { indexWait.Store(0) })
	busy := map[string]func(w *writing) (release func()){
		// a scan in another process holds the lock of the file
		"another process": func(w *writing) func() {
			db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: w.cfg.IndexPath()}).EscapedPath()+"?_txlock=immediate")
			if err != nil {
				t.Fatal(err)
			}
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			return func() {
				if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
					t.Fatal(err)
				}
				_ = conn.Close() // a test connection; the rollback above is the one that counts
				_ = db.Close()
			}
		},
		// a scan of this process holds the lock of the index for its walk
		"this process": func(w *writing) func() {
			st, err := w.srv.rp.sync()
			if err != nil {
				t.Fatal(err)
			}
			paused := &pausedStore{Store: st.stores["store-a"], paused: make(chan struct{}), resume: make(chan struct{})}
			t.Cleanup(paused.release)
			scanned := make(chan error, 1)
			go func() {
				_, err := scan.Run(paused, w.srv.rp.index, nil)
				scanned <- err
			}()
			<-paused.paused
			return func() {
				paused.release()
				if err := <-scanned; err != nil {
					t.Fatal(err)
				}
			}
		},
	}
	for name, hold := range busy {
		t.Run(name, func(t *testing.T) {
			w := startREST(t, restAttributes(), spec("demo-raw", "old/readme.txt", []byte("readme"), "text/plain"))
			put := func(path, body string) *answer {
				return do(t, w.addr(), "PUT", "/repository/demo-raw/"+path, []byte(body), "basic", asAdmin)
			}
			if r := put("busy/kept.txt", "kept"); r.status != http.StatusCreated {
				t.Fatalf("%d %q", r.status, r.body)
			}
			release := hold(w)
			started := time.Now()
			r := put("busy/a.txt", "one")
			if r.status != http.StatusServiceUnavailable || r.header.Get("Retry-After") == "" || time.Since(started) < 300*time.Millisecond {
				t.Errorf("a PUT while the index is busy: %d %v %q after %s", r.status, r.header, r.body, time.Since(started))
			}
			d := do(t, w.addr(), "DELETE", "/repository/demo-raw/busy/kept.txt", nil, "basic", asAdmin)
			if d.status != http.StatusServiceUnavailable || d.header.Get("Retry-After") == "" {
				t.Errorf("a DELETE while the index is busy: %d %v %q", d.status, d.header, d.body)
			}
			blob := do(t, w.addr(), "POST", "/repository/demo-docker/v2/app/blobs/uploads/"+digestQuery([]byte("layer")), []byte("layer"), "basic", asAdmin)
			if blob.status != http.StatusServiceUnavailable || blob.header.Get("Retry-After") == "" || errorCodeOf(t, blob.body) != "UNAVAILABLE" {
				t.Errorf("a Docker blob while the index is busy: %d %v %q", blob.status, blob.header, blob.body)
			}
			release()
			if files := w.workFiles(); len(files) != 0 {
				t.Fatalf("work files %v", files)
			}
			if counts, live := w.scan(); counts.Live != 1 || live[0].Path != "busy/kept.txt" {
				t.Fatalf("live blobs %+v %+v", counts, live)
			}
			if r := w.fetch("demo-raw", "busy/a.txt"); r.status != http.StatusNotFound {
				t.Fatalf("the refused PUT is served: %d %q", r.status, r.body)
			}
			if r := w.fetch("demo-raw", "busy/kept.txt"); r.status != http.StatusOK || string(r.body) != "kept" {
				t.Fatalf("the refused DELETE: %d %q", r.status, r.body)
			}
			if r := put("busy/a.txt", "one"); r.status != http.StatusCreated {
				t.Fatalf("the PUT again: %d %q", r.status, r.body)
			}
			if r := w.fetch("demo-raw", "busy/a.txt"); string(r.body) != "one" {
				t.Fatalf("%d %q", r.status, r.body)
			}
			if r := do(t, w.addr(), "DELETE", "/repository/demo-raw/busy/kept.txt", nil, "basic", asAdmin); r.status != http.StatusNoContent {
				t.Fatalf("the DELETE again: %d %q", r.status, r.body)
			}
		})
	}
}
