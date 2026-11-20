package server

import (
	"encoding/json"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/metrics"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// areaShapes is testdata/rest/status-area.json.
type areaShapes struct {
	Paths map[string]struct {
		Answer   string   `json:"answer"`
		Fields   []string `json:"fields"`
		Result   []string `json:"result"`
		Statuses []int    `json:"statuses"`
	} `json:"paths"`
}

func readAreaShapes(t *testing.T) areaShapes {
	t.Helper()
	data, err := os.ReadFile("../../testdata/rest/status-area.json")
	if err != nil {
		t.Fatal(err)
	}
	var s areaShapes
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func keysOf(t *testing.T, v any) []string {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %v", v)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sorted(list []string) []string {
	out := slices.Clone(list)
	sort.Strings(out)
	return out
}

// Each of the 6 paths of the status area answers an administrator in the
// shape of testdata/rest/status-area.json; the reads of phase 7 need the
// permission of the health checks.
func TestStatusAreaAnswersEachPathInItsShape(t *testing.T) {
	shapes := readAreaShapes(t)
	if len(shapes.Paths) != 6 {
		t.Fatalf("%d paths in status-area.json, want 6", len(shapes.Paths))
	}
	a := newSecureApp(t, newFakeSecurity())
	clock := tasks.NewFakeClock(time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC))
	r := metrics.NewRecorder(metrics.Options{Clock: clock})
	a.srv.figures = r
	node := a.srv.nodeID()
	if !nodeUUID.MatchString(node) || node != a.srv.nodeID() {
		t.Fatalf("node id %q", node)
	}
	for path, shape := range shapes.Paths {
		target := "/service/rest" + strings.Replace(path, "{nodeId}", node, 1)
		got := a.send("GET", target, "basic", asAdmin)
		want := 200
		if path == "/v1/status/writable" {
			want = 503 // writes are off in this app
		}
		if got.status != want {
			t.Errorf("%s: %d %q", target, got.status, got.body)
			continue
		}
		switch shape.Answer {
		case "empty":
			if len(got.body) != 0 {
				t.Errorf("%s: body %q", target, got.body)
			}
		case "map":
			for name, check := range got.object(t) {
				if keys := keysOf(t, check); !slices.Equal(keys, sorted(shape.Fields)) {
					t.Errorf("%s %s: fields %v, want %v", target, name, keys, sorted(shape.Fields))
				}
			}
		case "object", "list":
			items := []any{got.json(t)}
			if shape.Answer == "list" {
				list, ok := got.json(t).([]any)
				if !ok || len(list) == 0 {
					t.Errorf("%s: not a list with items: %q", target, got.body)
					continue
				}
				items = list
			}
			for _, item := range items {
				if keys := keysOf(t, item); !slices.Equal(keys, sorted(shape.Fields)) {
					t.Errorf("%s: fields %v, want %v", target, keys, sorted(shape.Fields))
				}
				if shape.Result == nil {
					continue
				}
				results := item.(map[string]any)["results"].(map[string]any)
				if len(results) == 0 || item.(map[string]any)["nodeId"] != node {
					t.Errorf("%s: %v", target, item)
				}
				for _, res := range results {
					if keys := keysOf(t, res); !slices.Equal(keys, sorted(shape.Result)) {
						t.Errorf("%s: result fields %v", target, keys)
					}
				}
			}
		default:
			t.Fatalf("%s: answer kind %q", path, shape.Answer)
		}
		if strings.Contains(path, "{nodeId}") || strings.HasPrefix(path, "/beta/") || path == "/v1/monthly-metrics" {
			if got := a.send("GET", target, "basic", asNobody); got.status != 403 {
				t.Errorf("%s without the permission: %d", target, got.status)
			}
		}
	}
	if got := a.send("GET", "/service/rest/beta/status/check/0000", "basic", asAdmin); got.status != 404 {
		t.Errorf("an unknown node: %d %q", got.status, got.body)
	}
}

// The monthly figures are the requests of request_days per month, the 12
// months up to the current one, oldest first, with the change against the
// month before.
func TestMonthlyMetricsCountTheRequestsOfEachMonth(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	clock := tasks.NewFakeClock(time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC))
	r := metrics.NewRecorder(metrics.Options{Clock: clock, Usage: &memoryUsage{}})
	t.Cleanup(func() { _ = r.Close() })
	for range 4 {
		r.Request(logs.Request{Status: 200}, false)
	}
	clock.Advance(2 * time.Hour) // 1 November
	for range 6 {
		r.Request(logs.Request{Status: 200}, false)
	}
	a.srv.figures = r
	var months []map[string]any
	got := a.send("GET", "/service/rest/v1/monthly-metrics", "basic", asAdmin)
	if err := json.Unmarshal(got.body, &months); err != nil || len(months) != 12 {
		t.Fatalf("%d %q %v", got.status, got.body, err)
	}
	if months[0]["metricDate"] != "2025-12-01T00:00:00Z" || months[11]["metricDate"] != "2026-11-01T00:00:00Z" {
		t.Fatalf("months %v .. %v", months[0], months[11])
	}
	if months[10]["requestCount"] != float64(4) || months[10]["percentageChangeRequest"] != "N/A" {
		t.Fatalf("October %v", months[10])
	}
	// the request of the page itself is counted after its answer
	if months[11]["requestCount"] != float64(6) || months[11]["percentageChangeRequest"] != "50.00%" {
		t.Fatalf("November %v", months[11])
	}
	if months[11]["componentCount"] != float64(0) || months[11]["percentageChangeComponent"] != "N/A" {
		t.Fatalf("November %v", months[11])
	}
}

// The writable route answers 503 when the data directory takes no new file,
// also with writes on and a writable store.
func TestWritableNeedsAWritableDataDirectory(t *testing.T) {
	on := startWriting(t, newFakeSecurity(), true)
	if r := on.get(writablePath); r.status != 200 {
		t.Fatalf("writes on: %s", r)
	}
	on.srv.dataAccess = func(path string, mode uint32) error {
		if path == on.cfg.DataDir && mode == accessWrite {
			return syscall.EACCES
		}
		return nil
	}
	if r := on.get(writablePath); r.status != 503 || len(r.body) != 0 {
		t.Fatalf("data directory refused: %s", r)
	}
	on.srv.dataAccess = nil
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory without the write bit; the access check stands in for the modes")
	}
	if err := os.Chmod(on.cfg.DataDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(on.cfg.DataDir, 0o755) })
	if r := on.get(writablePath); r.status != 503 {
		t.Fatalf("data directory of mode 555: %s", r)
	}
}

// memoryUsage keeps the daily counts in memory.
type memoryUsage struct {
	mu        sync.Mutex
	downloads []catalog.Download
	days      []catalog.RequestDay
}

func (m *memoryUsage) AddUsage(downloads []catalog.Download, days []catalog.RequestDay) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.downloads = append(m.downloads, downloads...)
	m.days = append(m.days, days...)
	return nil
}

func (m *memoryUsage) Downloads(from, to string) ([]catalog.Download, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []catalog.Download
	for _, d := range m.downloads {
		if d.Day >= from && d.Day <= to {
			out = append(out, d)
		}
	}
	return out, nil
}

func (m *memoryUsage) RequestDays(from, to string) ([]catalog.RequestDay, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sums := map[string]catalog.RequestDay{}
	for _, d := range m.days {
		if d.Day >= from && d.Day <= to {
			s := sums[d.Day]
			s.Day, s.Requests, s.BytesSent = d.Day, s.Requests+d.Requests, s.BytesSent+d.BytesSent
			sums[d.Day] = s
		}
	}
	out := make([]catalog.RequestDay, 0, len(sums))
	for _, d := range sums {
		out = append(out, d)
	}
	return out, nil
}
