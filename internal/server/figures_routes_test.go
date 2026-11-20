package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/logship"
	"github.com/iogentic/pipesmith-registry/internal/metrics"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// figuresServer is a server with security on whose figures run on a fake
// clock, with daily counts in memory and one log target of fixed counters.
func figuresServer(t *testing.T) (*secureApp, *metrics.Recorder, *tasks.FakeClock) {
	t.Helper()
	a := newSecureApp(t, newFakeSecurity())
	clock := tasks.NewFakeClock(time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC))
	stats := []logship.Stats{{Target: "demo-loki", Kind: "loki", Queued: 120, Sent: 100, Dropped: 7, Failed: 5, Rejected: 3, Attempts: 9, Errors: 2}}
	r := metrics.NewRecorder(metrics.Options{Clock: clock, Usage: &memoryUsage{}, Targets: func() []logship.Stats { return stats }})
	t.Cleanup(func() { _ = r.Close() })
	a.srv.figures = r
	return a, r, clock
}

// The counters of /api/metrics equal the requests the test sent before it:
// their count, the bytes of their answers and their status classes; the
// log target shows its counters and the lines it holds.
func TestMetricsCountTheRequestsSent(t *testing.T) {
	a, r, _ := figuresServer(t)
	var sent int64
	statuses := map[int]int64{}
	for _, target := range []string{"/api/health", "/api/health", "/api/status", "/repository/demo-raw/docs/readme.txt",
		"/repository/demo-raw/missing.txt", "/api/nothing-here"} {
		got := a.send("GET", target, "basic", asAdmin)
		sent += int64(len(got.body))
		statuses[got.status/100]++
	}
	waitRequests(t, r, 6)
	got := a.send("GET", "/api/metrics", "basic", asAdmin)
	if got.status != 200 || got.header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %q", got.status, got.body)
	}
	var m struct {
		Requests, BytesSent, BytesReceived, Downloads int64
		Status                                        map[string]int64
		LogTargets                                    []metrics.TargetFigures
	}
	if err := json.Unmarshal(got.body, &m); err != nil {
		t.Fatal(err)
	}
	if m.Requests != 6 || m.BytesSent != sent || m.BytesReceived != 0 || m.Downloads != 1 {
		t.Fatalf("counters %+v, want 6 requests, %d bytes sent, 1 download", m, sent)
	}
	for class, n := range map[string]int64{"1xx": 0, "2xx": statuses[2], "3xx": statuses[3], "4xx": statuses[4], "5xx": statuses[5]} {
		if m.Status[class] != n {
			t.Fatalf("status %s: %d, want %d (%v)", class, m.Status[class], n, m.Status)
		}
	}
	want := metrics.TargetFigures{Name: "demo-loki", Kind: "loki", QueueDepth: 12, Queued: 120, Sent: 100, Dropped: 7, Failed: 5,
		Rejected: 3, Attempts: 9, Errors: 2}
	if len(m.LogTargets) != 1 || m.LogTargets[0] != want {
		t.Fatalf("targets %+v, want %+v", m.LogTargets, want)
	}
	for _, target := range []string{"/api/metrics", "/api/metrics/requests", "/api/metrics/downloads", "/api/figures?range=24h"} {
		if got := a.send("GET", target, "basic", asNobody); got.status != 403 {
			t.Errorf("%s without the permission: %d", target, got.status)
		}
	}
}

// The request and download figures of the contract (S43): every day from
// from to to, both included, with the requests and the bytes sent, and one
// row per repository and day with downloads.
func TestRequestAndDownloadFiguresPerDay(t *testing.T) {
	a, r, clock := figuresServer(t)
	r.Request(logs.Request{Status: 200, ResponseBytes: 40, Repository: "demo-raw"}, true)
	clock.Advance(24 * time.Hour)
	r.Request(logs.Request{Status: 200, ResponseBytes: 60, Repository: "demo-maven"}, true)
	r.Request(logs.Request{Status: 404, ResponseBytes: 5}, false)
	got := a.send("GET", "/api/metrics/requests?from=2026-11-19&to=2026-11-21", "basic", asAdmin)
	var days struct {
		From, To string
		Days     []struct {
			Day      string
			Requests int64
			BytesOut int64
		}
	}
	if err := json.Unmarshal(got.body, &days); err != nil || got.status != 200 {
		t.Fatalf("%d %q %v", got.status, got.body, err)
	}
	if days.From != "2026-11-19" || days.To != "2026-11-21" || len(days.Days) != 3 || days.Days[0].Requests != 0 ||
		days.Days[1].Day != "2026-11-20" || days.Days[1].Requests != 1 || days.Days[1].BytesOut != 40 ||
		days.Days[2].Requests != 2 || days.Days[2].BytesOut != 65 {
		t.Fatalf("days %+v", days)
	}
	got = a.send("GET", "/api/metrics/downloads?from=2026-11-20&to=2026-11-21", "basic", asAdmin)
	var rows struct {
		From, To string
		Rows     []struct {
			Repository, Day string
			Count           int64
		}
	}
	if err := json.Unmarshal(got.body, &rows); err != nil || got.status != 200 || len(rows.Rows) != 2 ||
		rows.Rows[0].Repository != "demo-raw" || rows.Rows[0].Day != "2026-11-20" || rows.Rows[0].Count != 1 ||
		rows.Rows[1].Repository != "demo-maven" || rows.Rows[1].Count != 1 {
		t.Fatalf("%d %q %v", got.status, got.body, err)
	}
	for _, bad := range []string{"?from=2026-11-21&to=2026-11-19", "?from=yesterday&to=2026-11-19", "?from=2025-01-01&to=2026-11-19"} {
		if got := a.send("GET", "/api/metrics/requests"+bad, "basic", asAdmin); got.status != 400 {
			t.Errorf("%s: %d %q", bad, got.status, got.body)
		}
	}
	// without from and to: the 30 days up to today
	got = a.send("GET", "/api/metrics/requests", "basic", asAdmin)
	if err := json.Unmarshal(got.body, &days); err != nil || days.To != "2026-11-21" || days.From != "2026-10-23" || len(days.Days) != 30 {
		t.Fatalf("default range %d %q", got.status, got.body)
	}
}

// The figures of the cards: a series of the samples of metrics.sqlite for
// 24 hours, 7 days or 30 days; with the samples off the answer says so.
func TestFiguresSeries(t *testing.T) {
	a, r, clock := figuresServer(t)
	got := a.send("GET", "/api/figures?range=24h", "basic", asAdmin)
	var off struct{ Enabled bool }
	if err := json.Unmarshal(got.body, &off); err != nil || got.status != 200 || off.Enabled {
		t.Fatalf("without samples %d %q", got.status, got.body)
	}
	waitRequests(t, r, 1) // the request above is counted before the samples start
	store, err := metrics.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := metrics.StartSampler(r, store, clock, func() (time.Duration, int64) { return 0, 1 << 20 }, nil)
	t.Cleanup(s.Stop)
	r.AttachSamples(store)
	for range 30 {
		r.Request(logs.Request{Status: 200, ResponseBytes: 1000}, false)
		clock.Advance(time.Minute)
	}
	var series struct {
		Enabled     bool
		Range       string
		StepSeconds int
		Points      []struct {
			Time        string
			Requests    int64
			BytesSent   int64
			MemoryBytes int64
		}
	}
	got = a.send("GET", "/api/figures?range=24h", "basic", asAdmin)
	if err := json.Unmarshal(got.body, &series); err != nil || !series.Enabled || series.Range != "24h" || series.StepSeconds != 300 {
		t.Fatalf("%d %q %v", got.status, got.body, err)
	}
	var requests int64
	for _, p := range series.Points {
		requests += p.Requests
		if p.MemoryBytes != 1<<20 {
			t.Fatalf("point %+v", p)
		}
	}
	if requests != 30 || len(series.Points) != 6 {
		t.Fatalf("%d requests in %d points: %q", requests, len(series.Points), got.body)
	}
	for _, rng := range []string{"7d", "30d"} {
		if got := a.send("GET", "/api/figures?range="+rng, "basic", asAdmin); got.status != 200 {
			t.Errorf("%s: %d", rng, got.status)
		}
	}
	if got := a.send("GET", "/api/figures?range=1y", "basic", asAdmin); got.status != 400 {
		t.Errorf("range 1y: %d", got.status)
	}
}
