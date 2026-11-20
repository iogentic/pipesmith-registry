package metrics

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// usageStub keeps what AddUsage got and refuses while fail is set.
type usageStub struct {
	mu        sync.Mutex
	fail      bool
	downloads []catalog.Download
	days      []catalog.RequestDay
}

func (u *usageStub) AddUsage(downloads []catalog.Download, days []catalog.RequestDay) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.fail {
		return errors.New("disk full")
	}
	u.downloads = append(u.downloads, downloads...)
	u.days = append(u.days, days...)
	return nil
}

// A flush the store refuses keeps the counts for the next one, and counts
// the failure.
func TestRefusedFlushKeepsTheCounts(t *testing.T) {
	clock := tasks.NewFakeClock(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))
	u := &usageStub{fail: true}
	r := NewRecorder(Options{Clock: clock, Usage: u})
	r.Request(logs.Request{Status: 200, ResponseBytes: 7, Repository: "demo-raw"}, true)
	clock.Advance(FlushEvery)
	if got := r.Totals().FlushFailures; got != 1 {
		t.Fatalf("%d flush failures, want 1", got)
	}
	u.mu.Lock()
	u.fail = false
	u.mu.Unlock()
	r.Request(logs.Request{Status: 200, ResponseBytes: 3, Repository: "demo-raw"}, true)
	clock.Advance(FlushEvery)
	want := []catalog.Download{{Repo: "demo-raw", Day: "2026-11-03", Count: 2, Bytes: 10}}
	if !slices.Equal(u.downloads, want) {
		t.Fatalf("downloads %v, want %v", u.downloads, want)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

// A download without a repository is a request, not a download; a nil
// Recorder counts nothing and does not fail.
func TestDownloadNeedsARepository(t *testing.T) {
	r := NewRecorder(Options{})
	r.Request(logs.Request{Status: 200, ResponseBytes: 3}, true)
	if got := r.Totals(); got.Requests != 1 || got.Downloads != 0 || got.Status["2xx"] != 1 {
		t.Fatalf("totals %+v", got)
	}
	var none *Recorder
	none.Request(logs.Request{Status: 200}, true)
	if got := none.Totals(); got.Requests != 0 || len(got.Status) != 5 {
		t.Fatalf("totals of nil %+v", got)
	}
	if err := none.Close(); err != nil {
		t.Fatal(err)
	}
}
