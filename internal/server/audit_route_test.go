package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/logs/audit"
	rtable "github.com/iogentic/pipesmith-registry/internal/route"
)

// Task 7.15: every route of the tables that takes a state changing method
// gets exactly one audit line per request, as administrator and as reader,
// written before the header of the answer; a deploy gets none (L5), and a
// GET none. The list comes from Table.Changing, so a route added later is
// sent without a change of this test.

// auditTrace collects the audit lines, the request records and the order
// of audit lines and answer headers of requests sent in one goroutine.
type auditTrace struct {
	mu       sync.Mutex
	lines    []logs.AuditEvent
	requests []logs.Request
	events   []string
}

func (tr *auditTrace) reset() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.lines, tr.requests, tr.events = nil, nil, nil
}

func (tr *auditTrace) event(e string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.events = append(tr.events, e)
}

// orderWriter notes when the header of the answer goes out.
type orderWriter struct {
	*httptest.ResponseRecorder
	trace *auditTrace
	wrote bool
}

func (w *orderWriter) WriteHeader(status int) {
	if !w.wrote {
		w.wrote = true
		w.trace.event("header")
	}
	w.ResponseRecorder.WriteHeader(status)
}

func (w *orderWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseRecorder.Write(p)
}

// traced sets the sinks of the server of a; the test then sends its
// requests through ServeHTTP in its own goroutine.
func traced(a *app) *auditTrace {
	tr := &auditTrace{}
	a.srv.audit = func(e logs.AuditEvent) error {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		tr.lines = append(tr.lines, e)
		tr.events = append(tr.events, "audit")
		return nil
	}
	a.srv.requests = func(r logs.Request) {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		tr.requests = append(tr.requests, r)
	}
	return tr
}

// serveDirect sends one request through ServeHTTP of the server of a.
func serveDirect(a *app, tr *auditTrace, method, target, basic string, headers map[string]string) *orderWriter {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "localhost:" + strconv.Itoa(a.srv.port) // the guard of POST /api/scan asks for the main port
	r.RemoteAddr = "192.0.2.7:40000"
	if basic != "" {
		user, password, _ := strings.Cut(basic, ":")
		r.SetBasicAuth(user, password)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := &orderWriter{ResponseRecorder: httptest.NewRecorder(), trace: tr}
	defer func() {
		// a streamed archive breaks its transfer with ErrAbortHandler, which
		// net/http takes; the recorder cannot stream
		if v := recover(); v != nil && v != http.ErrAbortHandler {
			panic(v)
		}
	}()
	a.srv.ServeHTTP(w, r)
	return w
}

// exampleTarget is the first example of the entry with its placeholders
// filled by names of the test stores.
func exampleTarget(info RouteInfo) string {
	repo := "demo-raw"
	switch info.Format {
	case "npm":
		repo = "demo-npm"
	case "docker":
		repo = "demo-docker"
	}
	asset := base64.RawURLEncoding.EncodeToString([]byte(repo + ":/docs/readme.txt"))
	target := strings.NewReplacer("{repository}", repo, "{path}", "audit/probe.txt", "{asset}", asset,
		"{token}", "NpmToken.demo.demo").Replace(info.Examples[0])
	return strings.TrimSuffix(target, "\n")
}

func TestAuditLinePerChangingRoute(t *testing.T) {
	for _, writes := range []bool{false, true} {
		a := startWriting(t, newFakeSecurity(), writes)
		tr := traced(a)
		table := a.srv.table()
		changing := table.Changing()
		if len(changing) < 10 {
			t.Fatalf("writes %v: %d changing routes; the list is not read", writes, len(changing))
		}
		sent, deploys := 0, 0
		for _, rt := range changing {
			info := routeInfo("audit", rt, writes)
			target := exampleTarget(info)
			for _, method := range rt.Methods {
				if !rtable.ChangesState([]string{method}) {
					continue
				}
				matched, _, _ := table.Match(method, routePath(strings.SplitN(target, "?", 2)[0]))
				if matched == nil {
					t.Errorf("writes %v: %s %s reaches no entry", writes, method, target)
					continue
				}
				if matched.Pattern.String() != rt.Pattern.String() {
					// an entry before it takes every path of it; the request is held to that entry
					t.Logf("writes %v: %s %s is taken by the entry %s before %s", writes, method, target, matched.Pattern, rt.Pattern)
				}
				rt := *matched
				for _, user := range []string{asAdmin, asReader} {
					tr.reset()
					w := serveDirect(a, tr, method, target, user, info.Headers)
					sent++
					tr.mu.Lock()
					lines, requests, events := slices.Clone(tr.lines), slices.Clone(tr.requests), slices.Clone(tr.events)
					tr.mu.Unlock()
					what := method + " " + target + " as " + user
					if rt.Rule.kind == ruleClient && method != http.MethodDelete {
						deploys++
						if len(lines) != 0 {
							t.Errorf("writes %v: %s: a deploy wrote %d audit lines, want 0 (L5)", writes, what, len(lines))
						}
						continue
					}
					if len(lines) != 1 {
						t.Errorf("writes %v: %s: %d audit lines, want 1 (status %d)", writes, what, len(lines), w.Code)
						continue
					}
					line := lines[0]
					checkAuditLine(t, what, line, w.Code, user)
					if len(events) < 2 || events[0] != "audit" || events[1] != "header" {
						t.Errorf("%s: order %v, want the audit line before the header", what, events)
					}
					if len(requests) != 1 || requests[0].ID != line.RequestID {
						t.Errorf("%s: request id %q, request records %v", what, line.RequestID, requests)
					}
				}
			}
		}
		if deploys == 0 || sent < 2*len(changing) {
			t.Fatalf("writes %v: %d requests, %d deploys over %d entries", writes, sent, deploys, len(changing))
		}
		t.Logf("writes %v: %d changing entries, %d requests, %d of them deploys", writes, len(changing), sent, deploys)
	}
}

var requestIDText = regexp.MustCompile(`^[0-9a-f]{16}$`)

// checkAuditLine holds a line to the status of its answer: ok below 400,
// denied for 401, 403 and 429, failed else; an administrator is never
// denied, the client address is the one of the request, the subject the
// user of the credentials.
func checkAuditLine(t *testing.T, what string, line logs.AuditEvent, code int, user string) {
	t.Helper()
	want := "failed"
	switch {
	case code < 400:
		want = "ok"
	case code == 401 || code == 403 || code == 429:
		want = "denied"
	}
	if line.Result != want {
		t.Errorf("%s: result %q for status %d, want %q", what, line.Result, code, want)
	}
	name, _, _ := strings.Cut(user, ":")
	if user == asAdmin && line.Result == "denied" {
		t.Errorf("%s: the administrator is denied (status %d)", what, code)
	}
	if line.Client != "192.0.2.7" || !requestIDText.MatchString(line.RequestID) || !strings.Contains(line.Action, ".") ||
		line.ObjectType == "" || line.Fields == nil {
		t.Errorf("%s: line %+v", what, line)
	}
	if line.Subject != "" && line.Subject != name {
		t.Errorf("%s: subject %q, want %q", what, line.Subject, name)
	}
}

func TestAuditNoLineForReads(t *testing.T) {
	a := startWriting(t, newFakeSecurity(), true)
	tr := traced(a)
	reads := 0
	for _, rt := range a.srv.table().Entries() {
		if !slices.Contains(rt.Methods, http.MethodGet) || strings.HasSuffix(rt.Pattern.String(), "/v2/token$") {
			continue // the token route of Docker writes a line per issue (finding 7b-4)
		}
		target := exampleTarget(routeInfo("audit", rt, true))
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			tr.reset()
			serveDirect(a, tr, method, target, asAdmin, nil)
			reads++
			tr.mu.Lock()
			n := len(tr.lines)
			tr.mu.Unlock()
			if n != 0 {
				t.Errorf("%s %s wrote %d audit lines, want 0", method, target, n)
			}
		}
	}
	if reads < 20 {
		t.Fatalf("%d reads sent", reads)
	}
}

func TestAuditResultOfStatus(t *testing.T) {
	for status, want := range map[int]string{200: "ok", 201: "ok", 204: "ok", 299: "ok", 307: "ok", 400: "failed", 401: "denied",
		403: "denied", 404: "failed", 409: "failed", 429: "denied", 500: "failed", 501: "failed"} {
		if got := audit.Result(status); got != want {
			t.Errorf("status %d: %q, want %q", status, got, want)
		}
	}
}
