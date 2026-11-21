package server

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/cleanup"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The preview of a cleanup policy (contract S15, rulings CP6 to CP8, CP11):
// what a run of the sent policy removes from one repository, through
// (*Server).CleanupPreview, with nothing removed or written.

const policyPreviewPath = policiesPath + "/preview"

func previewBody(format, criteria, repository string) string {
	return `{"policy": {"name": "demo-sent", "format": "` + format + `", "notes": "", "criteria": ` + criteria + `}, "repository": "` + repository + `"}`
}

type previewAnswer struct {
	Items []struct {
		Repository, Group, Name, Version, Path string
	}
	Total     int
	Truncated bool
}

// A policy by age over a raw repository: the preview lists the old blobs
// of the write store, leaves the store, the catalog and the batch of the
// download times as they were, also while the server is frozen, and a run
// of the same policy then removes exactly the listed paths (task 12, CL11).
func TestThePreviewListsWhatARunRemoves(t *testing.T) {
	old := spec("demo-raw", "old/kept.txt", []byte("kept by the old product"), "text/plain")
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")}, old)
	now := time.Now().UTC().Truncate(time.Millisecond)
	w.put(now.Add(-40*day), "demo-raw", "docs/old.txt", "old")
	w.put(now.Add(-35*day), "demo-raw", "docs/older-than-30.txt", "old too")
	w.put(now.Add(-1*day), "demo-raw", "docs/new.txt", "new")
	w.catalogRescan()
	w.srv.side().setClock(func() time.Time { return now })
	status(t, do(t, w.addr(), "GET", "/repository/demo-raw/docs/new.txt", nil, "basic", asAdmin), 200)
	before, catalogBefore := w.livePaths(), w.catalogPaths("demo-raw")
	pending := maps.Clone(w.srv.side().downloads.Pending("demo-raw"))
	body := []byte(previewBody("raw", `{"lastBlobUpdatedDays": 30, "lastDownloadedDays": null}`, "demo-raw"))
	preview := func() previewAnswer {
		r := do(t, w.addr(), "POST", policyPreviewPath, body, "basic", asAdmin, "Content-Type", jsonType)
		status(t, r, 200)
		var p previewAnswer
		if err := json.Unmarshal(r.body, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := preview()
	var paths []string
	for _, item := range p.Items {
		if item.Repository != "demo-raw" {
			t.Fatalf("item %+v", item)
		}
		paths = append(paths, item.Path)
	}
	if want := []string{"docs/old.txt", "docs/older-than-30.txt"}; !slices.Equal(paths, want) || p.Total != 2 || p.Truncated {
		t.Fatalf("preview %+v", p)
	}
	if err := w.srv.readOnly.Freeze(); err != nil {
		t.Fatal(err)
	}
	if again := preview(); !slices.Equal(again.Items, p.Items) {
		t.Fatalf("a preview while frozen %+v", again)
	}
	if err := w.srv.readOnly.Release(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.livePaths(), before) || !slices.Equal(w.catalogPaths("demo-raw"), catalogBefore) ||
		!maps.Equal(w.srv.side().downloads.Pending("demo-raw"), pending) || len(pending) != 1 {
		t.Fatalf("the preview changed the store, the catalog or the batch: %v %v", w.livePaths(), w.srv.side().downloads.Pending("demo-raw"))
	}
	policy := model.CleanupPolicy{Name: "demo-sent", Format: "raw", Criteria: map[string]any{"lastBlobUpdatedDays": float64(30)}}
	if _, err := w.srv.CleanupRun(t.Context(), policy, "demo-raw"); err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, path := range before {
		if !slices.Contains(w.livePaths(), path) {
			removed = append(removed, strings.TrimPrefix(path, "demo-raw/"))
		}
	}
	if !slices.Equal(removed, paths) {
		t.Fatalf("the run removed %v, the preview listed %v", removed, paths)
	}
}

// The refusals of a preview: the checks of the sent policy at
// policy.<field> (A9 for a criterion this server does not know, CP3; the
// format outside the five, CP4) and the target at repository travel
// together in one 400 list; the name of the policy is not checked (CP6).
func TestTheRefusalsOfAPreview(t *testing.T) {
	a, _ := policyApp(t)
	for _, c := range []struct {
		body string
		ids  []string
	}{
		{previewBody("raw", `{"lastDownloadedDays": 60, "regex": ".*"}`, "demo-raw"), []string{"policy.criteria.regex"}},
		{previewBody("ALL_FORMATS", `{"lastDownloadedDays": 60}`, "demo-raw"), []string{"policy.format"}},
		{previewBody("raw", `{}`, "demo-gone"), []string{"policy.criteria", "repository"}},
		{previewBody("raw", `{"lastBlobUpdatedDays": 0}`, ""), []string{"policy.criteria.lastBlobUpdatedDays", "repository"}},
		{previewBody("maven2", `{"lastBlobUpdatedDays": 3}`, "demo-group"), []string{"repository"}},
		{previewBody("maven2", `{"lastBlobUpdatedDays": 3}`, "demo-raw"), []string{"repository"}},
		{`{"repository": "demo-raw"}`, []string{"policy.format", "policy.criteria"}},
		{`{"policy": {"format": "raw", "criteria": "weekly"}, "repository": "demo-raw"}`, []string{"policy.criteria"}},
		{`[]`, []string{"*"}},
	} {
		if ids := refusedFields(t, sendTask(t, a, "POST", policyPreviewPath, c.body)); !slices.Equal(ids, c.ids) {
			t.Errorf("%s: ids %v, want %v", c.body, ids, c.ids)
		}
	}
	r := sendTask(t, a, "POST", policyPreviewPath, `{"policy": {"name": "bad name", "format": "*", "criteria": {"lastBlobUpdatedDays": 3}}, "repository": "demo-raw"}`)
	status(t, r, 200)
	sameJSON(t, r.body, map[string]any{"items": []any{}, "total": 0, "truncated": false})
}

// The preview needs nexus:settings:read and the browse permission of the
// target: a repository the caller may not browse answers as one that does
// not exist (CP7). A reader of content and a subject without rights get
// 403.
func TestThePreviewOfARepositoryTheCallerMayNotBrowse(t *testing.T) {
	a, sec := policyApp(t)
	sec.addBasic("demo-settings:settings-pw", subject("demo-settings", "", settingsRead, "nexus:repository-view:maven2:*:browse"))
	as := func(who, repository string) *answer {
		return sendTask(t, a, "POST", policyPreviewPath, previewBody("*", `{"lastBlobUpdatedDays": 3}`, repository), "basic", who)
	}
	hidden, missing := as("demo-settings:settings-pw", "demo-raw"), as("demo-settings:settings-pw", "demo-gone")
	status(t, hidden, 400)
	if want := strings.ReplaceAll(string(missing.body), "demo-gone", "demo-raw"); string(hidden.body) != want {
		t.Fatalf("hidden %s, missing %s", hidden.body, missing.body)
	}
	status(t, as("demo-settings:settings-pw", "demo-maven"), 200)
	status(t, as(asAdmin, "demo-raw"), 200)
	for _, who := range []string{asReader, asNobody} {
		status(t, as(who, "demo-maven"), 403)
	}
	r := sendTask(t, a, "POST", policyPreviewPath, `{"repository": "`+strings.Repeat("x", 65536)+`"}`)
	status(t, r, 413)
	if m := r.object(t); m["message"] != "the body is larger than 65536 bytes" {
		t.Fatalf("%s", r.body)
	}
}

// One preview runs at a time per server: a second one answers 429 with
// Retry-After and a message. The slot is free again after a run that
// failed or panicked (CP8). A failure of the run answers 500 with its fixed
// text, the server without state 503 (CP6).
func TestOnePreviewRunsAtATime(t *testing.T) {
	a, _ := policyApp(t)
	body := previewBody("raw", `{"lastBlobUpdatedDays": 3}`, "demo-raw")
	slot := a.srv.previewSlot()
	if !slot.take() {
		t.Fatal("the slot is taken")
	}
	r := sendTask(t, a, "POST", policyPreviewPath, body)
	status(t, r, http.StatusTooManyRequests)
	if r.header.Get("Retry-After") != "5" || r.object(t)["message"] != "another cleanup preview runs; try again" {
		t.Fatalf("%v %s", r.header, r.body)
	}
	slot.give()
	status(t, sendTask(t, a, "POST", policyPreviewPath, body), 200)
	for _, c := range []struct {
		err    error
		status int
		text   string
	}{
		{failure("the catalog cannot be read"), 500, "the catalog cannot be read"},
		{errCleanupNoState, 503, "the cleanup needs the state of the server, which is off"},
	} {
		fails := previewRun(func(context.Context, model.CleanupPolicy, string) (cleanup.Preview, error) {
			return cleanup.Preview{}, c.err
		})
		slot.run.Store(&fails)
		r = sendTask(t, a, "POST", policyPreviewPath, body)
		status(t, r, c.status)
		if r.object(t)["message"] != c.text {
			t.Fatalf("%s", r.body)
		}
	}
	panics := previewRun(func(context.Context, model.CleanupPolicy, string) (cleanup.Preview, error) {
		panic("an invented fault of the run")
	})
	slot.run.Store(&panics)
	req, err := http.NewRequest("POST", "http://"+a.addr()+policyPreviewPath, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("demo-admin", "admin-pw")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatalf("a run that panicked answered %d", resp.StatusCode)
	}
	slot.run.Store(nil)
	status(t, sendTask(t, a, "POST", policyPreviewPath, body), 200)
}

// The preview is audited as cleanup-policies.preview; the line carries no
// criteria (CP11, review 7b-15). A create keeps the default action.
func TestThePreviewIsAuditedAsAPreview(t *testing.T) {
	a, _ := policyApp(t)
	tr := traced(a.app)
	w, lines := serveBody(t, a.app, tr, "POST", policyPreviewPath, []byte(previewBody("raw", `{"lastBlobUpdatedDays": 987654}`, "demo-raw")),
		"Authorization", basicPair("demo-admin", "admin-pw"), "Content-Type", jsonType)
	line := oneLine(t, "preview", w, lines, 200)
	wantLine(t, "preview", line, "cleanup-policies.preview", "demo-admin", "NexusAuthenticatingRealm", "ok")
	if holdsText(line, "987654") || holdsText(tr.requests, "987654") {
		t.Fatalf("the audit holds the criteria: %+v", line)
	}
	w, lines = serveBody(t, a.app, tr, "POST", policiesPath, []byte(`{"name": "demo-new", "format": "raw", "criteria": {"lastBlobUpdatedDays": 1}}`),
		"Authorization", basicPair("demo-admin", "admin-pw"), "Content-Type", jsonType)
	wantLine(t, "create", oneLine(t, "create", w, lines, 201), "cleanup-policies.create", "demo-admin", "NexusAuthenticatingRealm", "ok")
}

// The cost of one preview over a raw repository of 100000 assets of the
// write store (ruling CP8): the time of the request and the heap in use
// and allocated, read with runtime.ReadMemStats before and after. It runs
// only with PSR_PREVIEW_MEASURE=1 and states numbers, no bound.
func TestMeasureAPreviewOf100000Assets(t *testing.T) {
	if os.Getenv("PSR_PREVIEW_MEASURE") != "1" {
		t.Skip("set PSR_PREVIEW_MEASURE=1 to measure")
	}
	const assets = 100000
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	now := time.Now().UTC().Truncate(time.Millisecond)
	began := time.Now()
	for i := range assets {
		w.put(now.Add(-40*day), "demo-raw", fmt.Sprintf("bulk/%03d/asset-%06d.txt", i%1000, i), "x")
	}
	w.catalogRescan()
	w.srv.side().setClock(func() time.Time { return now })
	t.Logf("written and scanned %d assets in %s", assets, time.Since(began).Round(time.Millisecond))
	body := []byte(previewBody("raw", `{"lastBlobUpdatedDays": 30}`, "demo-raw"))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	r := do(t, w.addr(), "POST", policyPreviewPath, body, "basic", asAdmin, "Content-Type", jsonType)
	took := time.Since(start)
	runtime.ReadMemStats(&after)
	status(t, r, 200)
	var p previewAnswer
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Total != assets || len(p.Items) != 100 || !p.Truncated {
		t.Fatalf("total %d, %d items, truncated %v", p.Total, len(p.Items), p.Truncated)
	}
	t.Logf("preview of %d assets: %s; heap in use %d MiB before, %d MiB after; allocated during the request %d MiB; GC cycles %d",
		assets, took.Round(time.Millisecond), before.HeapInuse>>20, after.HeapInuse>>20, (after.TotalAlloc-before.TotalAlloc)>>20,
		after.NumGC-before.NumGC)
}

// writeLog is a ResponseWriter that counts what a handler writes.
type writeLog struct {
	mu     sync.Mutex
	header http.Header
	status int
	bytes  int
}

func (l *writeLog) Header() http.Header { return l.header }

func (l *writeLog) WriteHeader(status int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.status = status
}

func (l *writeLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.status == 0 {
		l.status = http.StatusOK
	}
	l.bytes += len(p)
	return len(p), nil
}

// A preview whose client leaves while the run works gets no answer: the
// run ends with the request, nothing is written, and the slot is free, so
// the next preview answers 200 (CP6, CP8, review F4).
func TestACancelledPreviewWritesNothing(t *testing.T) {
	a, _ := policyApp(t)
	body := previewBody("raw", `{"lastBlobUpdatedDays": 3}`, "demo-raw")
	slot := a.srv.previewSlot()
	started, ended := make(chan struct{}), make(chan error, 1)
	blocks := previewRun(func(ctx context.Context, _ model.CleanupPolicy, _ string) (cleanup.Preview, error) {
		close(started)
		<-ctx.Done()
		ended <- ctx.Err()
		return cleanup.Preview{}, ctx.Err()
	})
	slot.run.Store(&blocks)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", policyPreviewPath, strings.NewReader(body)).WithContext(ctx)
	r.Host = "localhost:" + strconv.Itoa(a.srv.port)
	r.SetBasicAuth("demo-admin", "admin-pw")
	r.Header.Set("Content-Type", jsonType)
	w := &writeLog{header: http.Header{}}
	served := make(chan struct{})
	go func() {
		defer close(served)
		a.srv.ServeHTTP(w, r)
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not start")
	}
	cancel()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("the preview did not end with its request")
	}
	if err := <-ended; err != context.Canceled {
		t.Fatalf("the run ended with %v", err)
	}
	w.mu.Lock()
	code, bytes := w.status, w.bytes
	w.mu.Unlock()
	if code != 0 || bytes != 0 {
		t.Fatalf("a cancelled preview wrote status %d and %d bytes", code, bytes)
	}
	slot.run.Store(nil)
	status(t, sendTask(t, a, "POST", policyPreviewPath, body), 200)
}

// A preview refused before its handler runs, 401 without credentials and
// 403 for a reader of content, is audited as cleanup-policies.preview with
// the result denied, as an answered one is (CP11, review F5).
func TestARefusedPreviewIsAuditedAsAPreview(t *testing.T) {
	a, _ := policyApp(t)
	tr := traced(a.app)
	body := []byte(previewBody("raw", `{"lastBlobUpdatedDays": 3}`, "demo-raw"))
	for _, c := range []struct {
		what    string
		headers []string
		status  int
	}{
		{"without credentials", []string{"Content-Type", jsonType}, 401},
		{"as a reader", []string{"Authorization", basicPair("demo-reader", "reader-pw"), "Content-Type", jsonType}, 403},
	} {
		w, lines := serveBody(t, a.app, tr, "POST", policyPreviewPath, body, c.headers...)
		line := oneLine(t, c.what, w, lines, c.status)
		if line.Action != "cleanup-policies.preview" || line.Result != "denied" {
			t.Errorf("%s: line %+v", c.what, line)
		}
	}
	w, lines := serveBody(t, a.app, tr, "POST", policyPreviewPath, body,
		"Authorization", basicPair("demo-admin", "admin-pw"), "Content-Type", jsonType)
	wantLine(t, "answered", oneLine(t, "answered", w, lines, 200), "cleanup-policies.preview", "demo-admin", "NexusAuthenticatingRealm", "ok")
}

// The preview slot is a field of its server: each server has its own, so
// a preview that runs on one server leaves the other free (review F6).
func TestEachServerHasItsOwnPreviewSlot(t *testing.T) {
	a, _ := policyApp(t)
	b, _ := policyApp(t)
	if a.srv.previewSlot() != &a.srv.preview || b.srv.previewSlot() != &b.srv.preview {
		t.Fatal("the slot is not the field preview of its server")
	}
	if !a.srv.previewSlot().take() {
		t.Fatal("the slot of a new server is taken")
	}
	defer a.srv.previewSlot().give()
	body := previewBody("raw", `{"lastBlobUpdatedDays": 3}`, "demo-raw")
	status(t, sendTask(t, a, "POST", policyPreviewPath, body), http.StatusTooManyRequests)
	status(t, sendTask(t, b, "POST", policyPreviewPath, body), 200)
}
