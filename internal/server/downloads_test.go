package server

import (
	"slices"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/metrics"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// figuresApp is a server with a raw repository, a maven2 repository and a
// group of it, whose figures go to a catalog.sqlite of its own on the fake
// clock.
func figuresApp(t *testing.T) (*app, *metrics.Recorder, *catalog.Catalog, *tasks.FakeClock) {
	t.Helper()
	base := t.TempDir()
	a := newApp(t, []config.StoreConfig{blobStore(t, base, "s1",
		spec("demo-raw", "files/a.txt", []byte("0123456789"), "text/plain"),
		jarSpec("demo jar content"))}, appOptions{defs: []*pyjson.Object{
		def("demo-raw", "raw", "hosted"), def("demo-releases", "maven2", "hosted"),
		def("demo-group", "maven2", "group", "demo-releases")}})
	c, err := catalog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	clock := tasks.NewFakeClock(time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC))
	r := metrics.NewRecorder(metrics.Options{Clock: clock, Usage: c})
	t.Cleanup(func() { _ = r.Close() })
	a.srv.figures = r
	return a, r, c, clock
}

// waitRequests waits until the recorder counted n requests: the record of a
// request is made once its answer ended, which the client may see first.
func waitRequests(t *testing.T, r *metrics.Recorder, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for r.Totals().Requests < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d requests counted, want %d", r.Totals().Requests, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.Totals().Requests; got != n {
		t.Fatalf("%d requests counted, want %d", got, n)
	}
}

// Decision L11 of the phase 7 plan: a GET answered with 200, or with 206
// from byte 0, is a download of the repository of the request, a group
// included, on its day. HEAD, 404 and a range from above byte 0 add
// nothing. Counts reach catalog.sqlite every 10 s and at the close.
func TestDownloadsAreCountedPerRepositoryAndDay(t *testing.T) {
	a, r, c, clock := figuresApp(t)
	jar := "/repository/demo-releases/" + jarPath
	statuses := []int{}
	send := func(target string, headers ...string) []byte {
		res := a.get(target, headers...)
		statuses = append(statuses, res.status)
		return res.body
	}
	var sentDay1 int64
	for range 3 {
		sentDay1 += int64(len(send("/repository/demo-raw/files/a.txt")))
	}
	for range 2 {
		sentDay1 += int64(len(send("/repository/demo-group/" + jarPath)))
	}
	sentDay1 += int64(len(send(jar, "Range", "bytes=0-3")))
	// none of these is a download
	head := a.head("/repository/demo-raw/files/a.txt")
	statuses = append(statuses, head.status)
	sentDay1 += int64(len(send("/repository/demo-raw/files/missing.txt")))
	sentDay1 += int64(len(send("/repository/demo-raw/files/a.txt", "Range", "bytes=2-5")))
	if want := []int{200, 200, 200, 200, 200, 206, 200, 404, 206}; !slices.Equal(statuses, want) {
		t.Fatalf("statuses %v, want %v", statuses, want)
	}
	waitRequests(t, r, 9)
	if got, _ := c.Downloads("2026-11-01", "2026-11-30"); len(got) != 0 {
		t.Fatalf("rows before the first flush: %v", got)
	}
	clock.Advance(metrics.FlushEvery)
	day1 := []catalog.Download{
		{Repo: "demo-group", Day: "2026-11-03", Count: 2, Bytes: 2 * int64(len("demo jar content"))},
		{Repo: "demo-raw", Day: "2026-11-03", Count: 3, Bytes: 30},
		{Repo: "demo-releases", Day: "2026-11-03", Count: 1, Bytes: 4},
	}
	if got, err := c.Downloads("2026-11-01", "2026-11-30"); err != nil || !slices.Equal(got, day1) {
		t.Fatalf("after the flush of 10 s: %v %v, want %v", got, err, day1)
	}
	clock.Advance(time.Hour)
	send("/repository/demo-raw/files/a.txt")
	waitRequests(t, r, 10)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clip(day1), catalog.Download{Repo: "demo-raw", Day: "2026-11-04", Count: 1, Bytes: 10})
	if got, err := c.Downloads("2026-11-01", "2026-11-30"); err != nil || !slices.Equal(got, want) {
		t.Fatalf("after the close: %v %v, want %v", got, err, want)
	}
	days, err := c.RequestDays("2026-11-01", "2026-11-30")
	wantDays := []catalog.RequestDay{{Day: "2026-11-03", Requests: 9, BytesSent: sentDay1}, {Day: "2026-11-04", Requests: 1, BytesSent: 10}}
	if err != nil || !slices.Equal(days, wantDays) {
		t.Fatalf("request days %v %v, want %v", days, err, wantDays)
	}
	totals := r.Totals()
	if totals.Downloads != 7 || totals.Status["2xx"] != 9 || totals.Status["4xx"] != 1 {
		t.Fatalf("totals %+v", totals)
	}
}
