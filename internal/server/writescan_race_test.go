package server

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/scan"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// pausedStore stops a walk after its first entry until resume is closed; the
// scan then holds the write lock of the index.
type pausedStore struct {
	stores.Store
	once           sync.Once
	paused, resume chan struct{}
	released       sync.Once
}

// release lets the walk go on; a test also calls it in its cleanup, which
// runs before the server closes, so that a failed test does not leave the
// index locked.
func (p *pausedStore) release() { p.released.Do(func() { close(p.resume) }) }

func (p *pausedStore) Entries(counts *stores.ScanCounts, yield func(stores.Entry) error) error {
	return p.Store.Entries(counts, func(e stores.Entry) error {
		err := yield(e)
		p.once.Do(func() {
			close(p.paused)
			<-p.resume
		})
		return err
	})
}

// A write that comes while a scan of the write store holds the index waits
// for the scan: its blob is placed only under the write lock of the index
// (index.PutAfter), so the walk never meets it. Once the scan gives the index
// back the write lands, its row replaces the one the scan wrote, and the blob
// it replaced gets its mark at once (findings S6 of review 4 and 1 of the
// review of leg 4g).
func TestAWriteDuringAScanOfTheWriteStore(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	// a blob of the day before, so that the walk stops before it lists the
	// day of the two writes of the path
	w.clock.add(-24 * time.Hour)
	if _, err := w.ingest("demo-raw", "docs/early.txt", "early"); err != nil {
		t.Fatal(err)
	}
	w.clock.add(24 * time.Hour)
	first, err := w.ingest("demo-raw", "docs/a.txt", "first")
	if err != nil {
		t.Fatal(err)
	}
	w.clock.add(time.Minute)
	repo := w.repo("demo-raw")
	st, err := w.srv.rp.sync()
	if err != nil {
		t.Fatal(err)
	}
	paused := &pausedStore{Store: st.stores["local-write"], paused: make(chan struct{}), resume: make(chan struct{})}
	t.Cleanup(paused.release)
	scanned := make(chan error, 1)
	go func() {
		_, err := scan.Run(paused, w.srv.rp.index, nil)
		scanned <- err
	}()
	<-paused.paused
	type outcome struct {
		result *ingestResult
		err    error
	}
	written := make(chan outcome, 1)
	go func() {
		result, err := w.srv.ingest(ingestRequest{repo: repo, path: "docs/a.txt", body: strings.NewReader("second"), size: 6,
			subject: subject("demo-admin", "", "nexus:*"), address: "192.0.2.7"})
		written <- outcome{result, err}
	}()
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if counts, _ := w.scan(); counts.Live != 2 {
			t.Fatalf("a blob was placed while the scan held the index: %+v", counts)
		}
	}
	if len(written) != 0 {
		t.Fatal("the write ended while the scan held the index")
	}
	paused.release()
	if err := <-scanned; err != nil {
		t.Fatal(err)
	}
	done := <-written
	if done.err != nil {
		t.Fatal(done.err)
	}
	second := done.result
	if props := w.properties(second.asset.Location); props["deleted"] != "" {
		t.Fatalf("the write marked its own blob: %q", props)
	}
	if r := w.fetch("demo-raw", "docs/a.txt"); r.status != 200 || string(r.body) != "second" {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if props := w.properties(first.asset.Location); props["deleted"] != "true" {
		t.Fatalf("the replaced blob: %q", props)
	}
	if counts, _ := w.scan(); counts.Live != 2 {
		t.Fatalf("%+v", counts)
	}
}

// twoPauses stops a walk after its first entry and again after its last one,
// each time until the test lets it go on; the scan holds the write lock of
// the index meanwhile.
type twoPauses struct {
	stores.Store
	once          sync.Once
	first, last   chan struct{} // closed at the pauses
	goOn, finally chan struct{} // closed by the test
	wentOn, ended sync.Once
}

func (p *twoPauses) goOnNow() { p.wentOn.Do(func() { close(p.goOn) }) }
func (p *twoPauses) endNow()  { p.ended.Do(func() { close(p.finally) }) }
func (p *twoPauses) release() { p.goOnNow(); p.endNow() }

func (p *twoPauses) Entries(counts *stores.ScanCounts, yield func(stores.Entry) error) error {
	err := p.Store.Entries(counts, func(e stores.Entry) error {
		err := yield(e)
		p.once.Do(func() {
			close(p.first)
			<-p.goOn
		})
		return err
	})
	close(p.last)
	<-p.finally
	return err
}

// A write that gives up with 503 while a scan of the write store holds the
// index leaves nothing the scan can take (finding 1 of the review of leg
// 4g). With the blob placed before the wait, the walk met it live, the scan
// gave it the row after the write had answered 503, and the blob of the
// acknowledged write before it got the mark of a replaced one.
func TestAWriteThatGivesUpDuringAScanOfTheWriteStore(t *testing.T) {
	indexWait.Store(int64(300 * time.Millisecond))
	t.Cleanup(func() { indexWait.Store(0) })
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	w.clock.add(-24 * time.Hour)
	if _, err := w.ingest("demo-raw", "docs/early.txt", "early"); err != nil {
		t.Fatal(err)
	}
	w.clock.add(24 * time.Hour)
	first, err := w.ingest("demo-raw", "docs/a.txt", "first")
	if err != nil {
		t.Fatal(err)
	}
	w.clock.add(time.Minute)
	st, err := w.srv.rp.sync()
	if err != nil {
		t.Fatal(err)
	}
	walk := &twoPauses{Store: st.stores["local-write"], first: make(chan struct{}), last: make(chan struct{}), goOn: make(chan struct{}),
		finally: make(chan struct{})}
	t.Cleanup(walk.release) // before the server closes, also after a failure
	scanned := make(chan error, 1)
	go func() {
		_, err := w.srv.writes.scanStore(walk, w.srv.rp.index, nil)
		scanned <- err
	}()
	<-walk.first
	refused := make(chan error, 1)
	go func() {
		_, err := w.ingest("demo-raw", "docs/a.txt", "second")
		refused <- err
	}()
	// the walk goes on once the write placed a blob or gave up, and then
	// lists the day of the two writes of the path
	waitFor(t, "the write to place its blob or to give up", func() bool { counts, _ := w.scan(); return counts.Live == 3 || len(refused) == 1 })
	walk.goOnNow()
	<-walk.last
	var refusal *writeRefusal
	if err := <-refused; !errors.As(err, &refusal) || refusal.status != 503 {
		t.Fatalf("the write while the scan held the index: %v", err)
	}
	walk.endNow()
	if err := <-scanned; err != nil {
		t.Fatal(err)
	}
	if r := w.fetch("demo-raw", "docs/a.txt"); r.status != 200 || string(r.body) != "first" {
		t.Fatalf("after the scan: %d %q", r.status, r.body)
	}
	if props := w.properties(first.asset.Location); props["deleted"] != "" {
		t.Fatalf("the acknowledged blob carries a mark: %q", props)
	}
	if counts, _ := w.scan(); counts.Live != 2 || len(w.workFiles()) != 0 {
		t.Fatalf("%+v, work files %v", counts, w.workFiles())
	}
}
