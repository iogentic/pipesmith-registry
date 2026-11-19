package server

import (
	"bytes"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two bounds of finding N4 of review 4. The closing PUT of a blob the
// repository holds already hashes its body and stores nothing; it reads
// that body under the limit of the repository, as every other body.
func TestTheClosingPutOfAHeldBlobIsBounded(t *testing.T) {
	w := startOCI(t, map[string]map[string]any{"demo-docker": {"storage": map[string]any{"writePolicy": "ALLOW"}, "max_body_bytes": 64}})
	layer := []byte("a layer the repository holds")
	if r := do(t, w.port("demo-docker"), "POST", "/v2/app/blobs/uploads/"+digestQuery(layer), layer, "basic", asAdmin); r.status != http.StatusCreated {
		t.Fatalf("the first upload: %d %q", r.status, r.body)
	}
	r := chunked(t, w.port("demo-docker"), "PUT", w.startUploadAt(t, asAdmin)+digestQuery(layer), bytes.Repeat([]byte("x"), 4096), "basic", asAdmin)
	if r.status != http.StatusRequestEntityTooLarge || errorCodeOf(t, r.body) != "SIZE_INVALID" {
		t.Fatalf("a closing PUT of 4096 bytes with a limit of 64: %d %q", r.status, r.body)
	}
	if r := chunked(t, w.port("demo-docker"), "PUT", w.startUploadAt(t, asAdmin)+digestQuery(layer), layer, "basic", asAdmin); r.status != http.StatusCreated {
		t.Fatalf("a closing PUT of the held bytes: %d %q", r.status, r.body)
	}
	eventually(t, "no work file", func() bool { return len(w.workFiles()) == 0 })
}

// GET /v1/status/writable needs no sign in, so the rate of its requests
// must not set the rate of the retries of the waiting marks: it tries them
// again at most once per second.
func TestTheWritableStatusRetriesAtMostOncePerSecond(t *testing.T) {
	w := startIngestWithIDs(t, lowID, highID)
	first, _ := w.replaceWithAFailedMark()
	var tries atomic.Int32
	w.srv.writes.failMarks(func(location, _ string) error {
		if location == first.asset.Location {
			tries.Add(1)
			return errors.New("the disk refuses the mark")
		}
		return nil
	})
	var mu sync.Mutex
	now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	w.srv.writes.retryClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}
	for range 20 {
		if r := w.get(writablePath); r.status != http.StatusServiceUnavailable {
			t.Fatalf("writable while the mark waits: %s", r)
		}
	}
	if n := tries.Load(); n != 1 {
		t.Fatalf("20 requests in one second tried the mark %d times", n)
	}
	advance(time.Second)
	if r := w.get(writablePath); r.status != http.StatusServiceUnavailable || tries.Load() != 2 {
		t.Fatalf("a second later: %s, %d tries", r, tries.Load())
	}
	w.srv.writes.failMarks(nil)
	advance(time.Second)
	if r := w.get(writablePath); r.status != http.StatusOK {
		t.Fatalf("once the store takes the mark: %s", r)
	}
}
