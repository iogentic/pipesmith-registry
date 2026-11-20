package server

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	readOnlyPath     = "/service/rest/v1/read-only"
	freezePath       = readOnlyPath + "/freeze"
	releasePath      = readOnlyPath + "/release"
	forceReleasePath = readOnlyPath + "/force-release"
)

type readOnlyAnswer struct {
	Frozen          bool
	SystemInitiated bool
	SummaryReason   string
}

func readOnlyOf(t *testing.T, a *secureApp) readOnlyAnswer {
	t.Helper()
	r := a.send("GET", readOnlyPath, "basic", asAdmin)
	status(t, r, 200)
	m := r.object(t)
	if len(m) != 3 {
		t.Fatalf("read only answer %s", r.body)
	}
	frozen, _ := m["frozen"].(bool)
	system, _ := m["systemInitiated"].(bool)
	reason, _ := m["summaryReason"].(string)
	return readOnlyAnswer{frozen, system, reason}
}

// frozenWrites are writes of every method the gate refuses while the
// server is frozen: a capability save and delete, a role create, the scan
// of the index, and a client write below /repository/.
var frozenWrites = []struct{ method, target, body string }{
	{"PUT", capabilitiesPath + "/c-none", `{"notes":"x"}`},
	{"DELETE", capabilitiesPath + "/c-none", ``},
	{"POST", "/service/rest/v1/security/roles", `{"id":"demo-frozen","name":"demo-frozen","privileges":[],"roles":[]}`},
	{"PATCH", "/repository/demo-raw/docs/readme.txt", `x`},
}

// Ruling SB2: the mode starts released and says that a restart releases a
// freeze; a freeze by a user refuses every write of every method with 503
// and its reason and lets the session, a rule test, the reads and the
// release through; force release does not end it, release does; a second
// release answers 409. GET /api/areas follows the state.
func TestReadOnlyFreezeAndRelease(t *testing.T) {
	sec := newFakeSecurity()
	a := newSecureApp(t, sec)
	start := readOnlyOf(t, a)
	if start.Frozen || start.SystemInitiated || !strings.Contains(start.SummaryReason, "a restart") {
		t.Fatalf("at start %+v", start)
	}
	if areas := areasOf(t, a, asNobody); areas.ReadOnly.Frozen || areas.ReadOnly.Reason != start.SummaryReason {
		t.Fatalf("areas at start %+v", areas.ReadOnly)
	}
	_, resets := sec.writes()
	freeze := a.send("POST", freezePath, "basic", asAdmin)
	if freeze.status != 204 || len(freeze.body) != 0 {
		t.Fatalf("freeze %d %s", freeze.status, freeze.body)
	}
	frozen := readOnlyOf(t, a)
	if !frozen.Frozen || frozen.SystemInitiated || !strings.Contains(frozen.SummaryReason, "a restart") {
		t.Fatalf("frozen %+v", frozen)
	}
	if areas := areasOf(t, a, asNobody); !areas.ReadOnly.Frozen || areas.ReadOnly.Reason != frozen.SummaryReason {
		t.Fatalf("areas while frozen %+v", areas.ReadOnly)
	}
	updates, _ := sec.writes()
	for _, c := range frozenWrites {
		r := do(t, a.addr(), c.method, c.target, []byte(c.body), "basic", asAdmin, "Content-Type", jsonType)
		if r.status != 503 || !strings.Contains(string(r.body), frozen.SummaryReason) {
			t.Fatalf("%s %s while frozen: %d %s", c.method, c.target, r.status, r.body)
		}
	}
	if again := a.send("POST", freezePath, "basic", asAdmin); again.status != 503 {
		t.Fatalf("second freeze %d %s", again.status, again.body)
	}
	if now, _ := sec.writes(); now != updates {
		t.Fatal("a write changed the state while frozen")
	}
	// what passes the gate
	if r := a.send("POST", "/api/session", "basic", asAdmin); r.status != 299 {
		t.Fatalf("session while frozen %d", r.status)
	}
	if r := do(t, a.addr(), "POST", "/api/routing-rules/test", []byte(`{"mode":"BLOCK","matchers":["/a"],"path":"/a"}`), "basic", asAdmin,
		"Content-Type", jsonType); r.status != 200 {
		t.Fatalf("rule test while frozen %d %s", r.status, r.body)
	}
	status(t, a.send("GET", "/service/rest/v1/repositories", "basic", asAdmin), 200)
	force := a.send("POST", forceReleasePath, "basic", asAdmin)
	if force.status != 409 || !strings.Contains(force.object(t)["message"].(string), "a user froze the server") {
		t.Fatalf("force release of a freeze by a user %d %s", force.status, force.body)
	}
	if !readOnlyOf(t, a).Frozen {
		t.Fatal("a refused force release ended the freeze")
	}
	release := a.send("POST", releasePath, "basic", asAdmin)
	if release.status != 204 || len(release.body) != 0 {
		t.Fatalf("release %d %s", release.status, release.body)
	}
	if readOnlyOf(t, a).Frozen {
		t.Fatal("still frozen after the release")
	}
	if again := a.send("POST", releasePath, "basic", asAdmin); again.status != 409 || again.object(t)["message"] != "the server is not frozen" {
		t.Fatalf("release without a freeze %d %s", again.status, again.body)
	}
	for _, c := range frozenWrites {
		if r := do(t, a.addr(), c.method, c.target, []byte(c.body), "basic", asAdmin, "Content-Type", jsonType); r.status == 503 {
			t.Fatalf("%s %s after the release: %d %s", c.method, c.target, r.status, r.body)
		}
	}
	if _, now := sec.writes(); now < resets+2 {
		t.Fatalf("%d hooks for a freeze and a release, want 2 or more", now-resets)
	}
}

// A freeze the system set ends with force release, not with release.
func TestReadOnlyFreezeOfTheSystem(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	a.srv.readOnly.FreezeBySystem("The disk of the data directory is full.")
	s := readOnlyOf(t, a)
	if !s.Frozen || !s.SystemInitiated || !strings.HasPrefix(s.SummaryReason, "The disk of the data directory is full.") {
		t.Fatalf("%+v", s)
	}
	if r := a.send("POST", releasePath, "basic", asAdmin); r.status != 409 {
		t.Fatalf("release %d %s", r.status, r.body)
	}
	if r := a.send("POST", forceReleasePath, "basic", asAdmin); r.status != 204 {
		t.Fatalf("force release %d %s", r.status, r.body)
	}
	if readOnlyOf(t, a).Frozen {
		t.Fatal("frozen after the force release")
	}
}

// A subject without nexus:settings:update gets 403 for the three changes
// and changes nothing; without nexus:settings:read the state is 403; a
// session needs the request token (A22).
func TestReadOnlyRights(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	for _, who := range []string{asReader, asNobody} {
		for _, target := range []string{freezePath, releasePath, forceReleasePath} {
			r := a.send("POST", target, "basic", who)
			if r.status != 403 || r.object(t)["message"] != "this needs the permission nexus:settings:update" {
				t.Fatalf("%s by %s: %d %s", target, who, r.status, r.body)
			}
		}
		if r := a.send("GET", readOnlyPath, "basic", who); r.status != 403 {
			t.Fatalf("state by %s: %d", who, r.status)
		}
	}
	for _, target := range []string{freezePath, releasePath, forceReleasePath} {
		for _, headers := range [][]string{{"Cookie", "fake-session=s1"}, {"Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1x"}} {
			r := do(t, a.addr(), "POST", target, nil, headers...)
			if r.status != 403 || r.object(t)["message"] != "missing or wrong request token" {
				t.Fatalf("%s %v: %d %s", target, headers, r.status, r.body)
			}
		}
	}
	if readOnlyOf(t, a).Frozen {
		t.Fatal("a refused request froze the server")
	}
	if r := do(t, a.addr(), "POST", freezePath, nil, "Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1"); r.status != 204 {
		t.Fatalf("freeze with the token %d %s", r.status, r.body)
	}
}

// While frozen a task run answers 409 with the reason and starts nothing;
// a stop passes the gate (ruling SB8): a stop of an idle task answers 409
// from the scheduler and a run that went before the freeze can be stopped;
// after the release the run passes.
func TestReadOnlyTaskRun(t *testing.T) {
	a, runs, started := taskApp(t, newFakeSecurity())
	status(t, a.send("POST", freezePath, "basic", asAdmin), 204)
	run := a.send("POST", tasksPath+"/demo-local/run", "basic", asAdmin)
	if run.status != 409 || !strings.Contains(run.object(t)["message"].(string), "frozen") {
		t.Fatalf("run while frozen %d %s", run.status, run.body)
	}
	if stop := a.send("POST", tasksPath+"/demo-local/stop", "basic", asAdmin); stop.status != 409 {
		t.Fatalf("stop of an idle task while frozen %d %s", stop.status, stop.body)
	}
	if runs.count() != 0 {
		t.Fatal("a run started while frozen")
	}
	status(t, a.send("POST", releasePath, "basic", asAdmin), 204)
	status(t, a.send("POST", tasksPath+"/demo-local/run", "basic", asAdmin), 204)
	receiveID(t, started)
	status(t, a.send("POST", freezePath, "basic", asAdmin), 204)
	status(t, a.send("POST", tasksPath+"/demo-local/stop", "basic", asAdmin), 204)
	select {
	case <-runs.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end")
	}
}

// Ruling SB7: GET .../status/writable answers 503 while the server is
// frozen, and 200 again after the release, with writes on.
func TestWritableWhileFrozen(t *testing.T) {
	a := writingSecureApp(t)
	status(t, a.send("GET", "/service/rest/v1/status/writable"), 200)
	status(t, a.send("POST", freezePath, "basic", asAdmin), 204)
	status(t, a.send("GET", "/service/rest/v1/status/writable"), 503)
	status(t, a.send("POST", releasePath, "basic", asAdmin), 204)
	status(t, a.send("GET", "/service/rest/v1/status/writable"), 200)
}

// slowWrite sends the head of a PUT of five bytes below /repository/ as
// the admin and gives a function that sends the body and reads the status.
func slowWrite(t *testing.T, addr, target string) func() int {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	head := "PUT " + target + " HTTP/1.1\r\nHost: " + addr + "\r\nAuthorization: Basic " +
		base64.StdEncoding.EncodeToString([]byte(asAdmin)) + "\r\nContent-Length: 5\r\nContent-Type: text/plain\r\n\r\n"
	if _, err := conn.Write([]byte(head)); err != nil {
		t.Fatal(err)
	}
	return func() int {
		if _, err := conn.Write([]byte("slow\n")); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
}

// Ruling SB9: a freeze waits for a write that began before it and is
// frozen once that write has ended; a freeze whose write outlasts the wait
// answers 409 and the server stays released, and the write ends as usual.
func TestReadOnlyFreezeWaitsForARunningWrite(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	a := &secureApp{app: w.app}
	finish := slowWrite(t, a.addr(), "/repository/demo-raw/docs/slow-1.txt")
	time.Sleep(300 * time.Millisecond)
	frozen := make(chan *answer, 1)
	go func() { frozen <- a.send("POST", freezePath, "basic", asAdmin) }()
	time.Sleep(300 * time.Millisecond)
	select {
	case r := <-frozen:
		t.Fatalf("the freeze did not wait for the write: %d %s", r.status, r.body)
	default:
	}
	if code := finish(); code != 201 {
		t.Fatalf("the write that began before the freeze: %d", code)
	}
	if r := <-frozen; r.status != 204 || !readOnlyOf(t, a).Frozen {
		t.Fatalf("freeze %d %s", r.status, r.body)
	}
	status(t, a.send("POST", releasePath, "basic", asAdmin), 204)

	a.srv.readOnly.SetFreezeWait(300 * time.Millisecond)
	finish = slowWrite(t, a.addr(), "/repository/demo-raw/docs/slow-2.txt")
	time.Sleep(300 * time.Millisecond)
	r := a.send("POST", freezePath, "basic", asAdmin)
	if r.status != 409 || !strings.Contains(r.object(t)["message"].(string), "the server stays released") {
		t.Fatalf("freeze over a long write %d %s", r.status, r.body)
	}
	if readOnlyOf(t, a).Frozen {
		t.Fatal("frozen after a freeze that gave up")
	}
	if code := finish(); code != 201 {
		t.Fatalf("the long write: %d", code)
	}
}

// While frozen the gate refuses the writes of every path with writes on:
// the scan of the index, a Docker write on its port, a PUT below
// /repository/ and the REST upload of a component, each with 503 in the
// shape of its path; after the release none answers 503.
func TestReadOnlyFreezeRefusesTheWritesOfEveryPath(t *testing.T) {
	a := writingSecureApp(t)
	docker := a.docker["demo-docker"].Listener.Addr().String()
	writes := []struct {
		name, addr, method, target, body, shape string
	}{
		{"scan", a.addr(), "POST", "/api/scan", `{"store": "store-a"}`, "message"},
		{"docker", docker, "POST", "/v2/app/blobs/uploads/", ``, "docker"},
		{"repository", a.addr(), "PUT", "/repository/demo-raw/docs/frozen.txt", `frozen`, "text"},
		{"components", a.addr(), "POST", "/service/rest/v1/components?repository=demo-raw", ``, "message"},
	}
	status(t, a.send("POST", freezePath, "basic", asAdmin), 204)
	reason := readOnlyOf(t, a).SummaryReason
	for _, c := range writes {
		r := do(t, c.addr, c.method, c.target, []byte(c.body), "basic", asAdmin, "Content-Type", jsonType)
		body := string(r.body)
		ok := r.status == 503 && strings.Contains(body, reason)
		switch c.shape {
		case "message":
			ok = ok && r.object(t)["message"] == reason
		case "docker":
			var e struct {
				Errors []struct{ Code, Message string } `json:"errors"`
			}
			ok = ok && json.Unmarshal(r.body, &e) == nil && len(e.Errors) == 1 && e.Errors[0].Code == "UNAVAILABLE" && e.Errors[0].Message == reason
		case "text":
			ok = ok && strings.TrimSpace(body) == reason
		}
		if !ok {
			t.Fatalf("%s while frozen: %d %s", c.name, r.status, r.body)
		}
	}
	status(t, a.send("POST", releasePath, "basic", asAdmin), 204)
	for _, c := range writes {
		if r := do(t, c.addr, c.method, c.target, []byte(c.body), "basic", asAdmin, "Content-Type", jsonType); r.status == 503 {
			t.Fatalf("%s after the release: %d %s", c.name, r.status, r.body)
		}
	}
}

// The read only state follows the writes switch (Findings, row Read only
// state, of docs/plans/0.2.0.md): with writes on GET /v1/read-only and GET
// /api/areas answer released, not by the system, with the reason of a
// server that takes changes; with writes off they stay released and the
// reason says that content writes stay refused (ruling SB2).
func TestTheReadOnlyStateFollowsTheWritesSwitch(t *testing.T) {
	const takes = "The server takes changes."
	for _, writes := range []bool{true, false} {
		a := startWriting(t, newFakeSecurity(), writes)
		r := do(t, a.addr(), "GET", readOnlyPath, nil, "basic", asAdmin)
		status(t, r, 200)
		m := r.object(t)
		reason, _ := m["summaryReason"].(string)
		if m["frozen"] != false || m["systemInitiated"] != false || len(m) != 3 || !strings.HasPrefix(reason, takes) {
			t.Errorf("writes %v: %s", writes, r.body)
		}
		if off := strings.Contains(reason, "writes off"); off == writes {
			t.Errorf("writes %v: the reason %q", writes, reason)
		}
		var areas struct {
			ReadOnly struct {
				Frozen bool   `json:"frozen"`
				Reason string `json:"reason"`
			} `json:"readOnly"`
		}
		got := do(t, a.addr(), "GET", "/api/areas", nil, "basic", asNobody)
		if got.status != 200 || json.Unmarshal(got.body, &areas) != nil || areas.ReadOnly.Frozen || areas.ReadOnly.Reason != reason {
			t.Errorf("writes %v: areas %d %s", writes, got.status, got.body)
		}
	}
}
