package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Task 11 of the phase 5 plan: auto block (decision 5.3), with the fake
// clock, and the states of a proxy (decision 5.2) as the pages of phase 6
// read them.

// blockApp is a server with upstream on over proxies that send each request
// once, on the clock returned.
func blockApp(t *testing.T, proxies []proxyDef, blobs ...testkit.BlobSpec) (*upstreamApp, *testkit.Clock) {
	t.Helper()
	clock := testkit.NewClock(time.UnixMilli(createdMS).Add(time.Hour))
	state := openProxyState(t)
	for i := range proxies {
		client, _ := proxies[i].attributes["httpclient"].(map[string]any)
		merged := map[string]any{"connection": map[string]any{"retries": 0}}
		for k, v := range client {
			merged[k] = v
		}
		attributes := map[string]any{"httpclient": merged}
		for k, v := range proxies[i].attributes {
			if k != "httpclient" {
				attributes[k] = v
			}
		}
		proxies[i].attributes = attributes
	}
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: proxies, blobs: blobs, tune: func(o *appOptions) {
		o.proxyState = state
		o.upstreamConfig = proxy.Config{Now: clock.Now}
		o.writeOptions = blobwrite.Options{Now: clock.Now}
	}})
	return u, clock
}

func (u *upstreamApp) repository(name string) repos.Repo {
	u.t.Helper()
	st, err := u.srv.rp.sync()
	if err != nil {
		u.t.Fatal(err)
	}
	repo, ok := st.repositories.Get(name)
	if !ok {
		u.t.Fatalf("no repository %s", name)
	}
	return repo
}

func (u *upstreamApp) state(name string) proxy.State {
	u.t.Helper()
	return u.srv.proxies.State(u.repository(name))
}

// miss is a GET of a path the cache lacks, with the answer and the requests
// at the remote it must cause.
func (u *upstreamApp) miss(t *testing.T, repo, path string, status, requests int) {
	t.Helper()
	before := len(u.remote.Requests())
	if r := u.get(repo, path); r.status != status {
		t.Fatalf("%s/%s: %d %q, want %d", repo, path, r.status, r.body, status)
	}
	if n := len(u.remote.Requests()) - before; n != requests {
		t.Fatalf("%s/%s: %d requests at the remote, want %d", repo, path, n, requests)
	}
}

// After three failures in a row a proxy sends nothing for 60 s, then the
// first request is the probe; a failed probe doubles the wait up to 3,600 s,
// a good probe sets the proxy ready. While a probe is in flight no other
// request goes out. A cached path answers 200 in every state.
func TestProxyAutoBlock(t *testing.T) {
	cached := spec("demo-raw-proxy", "docs/cached.txt", []byte("cached bytes\n"), "text/plain")
	u, clock := blockApp(t, []proxyDef{{name: "demo-raw-proxy", format: "raw"}}, cached)
	n := 0
	next := func() string {
		n++
		path := fmt.Sprintf("docs/f-%d.txt", n)
		u.remote.Script(remoteBase("demo-raw-proxy")+path, testkit.ServerError())
		return path
	}
	cachedAnswers := func(when string) {
		t.Helper()
		if r := u.get("demo-raw-proxy", "docs/cached.txt"); r.status != 200 || string(r.body) != "cached bytes\n" {
			t.Fatalf("%s: the cached path %d %q", when, r.status, r.body)
		}
	}
	for i := 1; i <= 3; i++ {
		if s := u.state("demo-raw-proxy"); s.Status != proxy.Ready {
			t.Fatalf("after %d failures: %+v", i-1, s)
		}
		u.miss(t, "demo-raw-proxy", next(), http.StatusBadGateway, 1)
	}
	wait := time.Minute
	for probe := 1; probe <= 8; probe++ {
		s := u.state("demo-raw-proxy")
		if s.Status != proxy.AutoBlocked || !s.NextProbe.Equal(clock.Now().Add(wait)) {
			t.Fatalf("probe %d: %+v at %v, want the next probe after %v", probe, s, clock.Now(), wait)
		}
		cachedAnswers(fmt.Sprintf("probe %d", probe))
		u.miss(t, "demo-raw-proxy", next(), http.StatusNotFound, 0)
		clock.Advance(wait - time.Second)
		u.miss(t, "demo-raw-proxy", next(), http.StatusNotFound, 0)
		clock.Advance(time.Second)
		u.miss(t, "demo-raw-proxy", next(), http.StatusBadGateway, 1) // the probe, which fails
		wait = min(2*wait, time.Hour)
	}
	if wait != time.Hour {
		t.Fatalf("the wait after eight failed probes: %v", wait)
	}
	clock.Advance(wait)
	// a probe in flight: a request of another path sends nothing meanwhile
	const probePath = "docs/probe.txt"
	u.remote.Script(remoteBase("demo-raw-proxy")+probePath, testkit.SlowHeader(500*time.Millisecond, testkit.OK(rawBytes)))
	done := make(chan *answer, 1)
	go func() {
		r, err := u.tryGet("demo-raw-proxy", probePath)
		if err != nil {
			r = &answer{status: -1, body: []byte(err.Error())}
		}
		done <- r
	}()
	deadline := time.Now().Add(10 * time.Second)
	for u.remote.Count(remoteBase("demo-raw-proxy")+probePath) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the probe did not reach the remote")
		}
		time.Sleep(5 * time.Millisecond)
	}
	u.miss(t, "demo-raw-proxy", next(), http.StatusNotFound, 0)
	if r := <-done; r.status != 200 || string(r.body) != rawBytes {
		t.Fatalf("the good probe: %d %q", r.status, r.body)
	}
	if s := u.state("demo-raw-proxy"); s.Status != proxy.Ready || !s.NextProbe.IsZero() {
		t.Fatalf("after the good probe: %+v", s)
	}
	u.miss(t, "demo-raw-proxy", next(), http.StatusBadGateway, 1)
	u.miss(t, "demo-raw-proxy", next(), http.StatusBadGateway, 1)
	if s := u.state("demo-raw-proxy"); s.Status != proxy.Ready {
		t.Fatalf("two failures after the good probe: %+v", s)
	}
}

// A 404 and every other 4xx of the remote are no failures, and an answer
// that is no failure ends a row of failures. With auto block off every
// request goes out.
func TestProxyAutoBlockCountsFailuresInARow(t *testing.T) {
	u, _ := blockApp(t, []proxyDef{{name: "demo-raw-proxy", format: "raw"},
		{name: "demo-raw-open", format: "raw", attributes: map[string]any{"httpclient": map[string]any{"autoBlock": false}}}})
	answers := []testkit.Answer{testkit.ServerError(), testkit.ServerError(), testkit.NotFound(), testkit.ServerError(),
		testkit.ServerError(), testkit.BasicChallenge("demo-user", "demo-secret", testkit.OK("x")), testkit.ServerError(),
		testkit.ServerError()}
	for i, a := range answers {
		path := fmt.Sprintf("docs/row-%d.txt", i)
		u.remote.Script(remoteBase("demo-raw-proxy")+path, a)
		status := http.StatusBadGateway
		if a.Status != http.StatusInternalServerError {
			status = http.StatusNotFound
		}
		u.miss(t, "demo-raw-proxy", path, status, 1)
	}
	if s := u.state("demo-raw-proxy"); s.Status != proxy.Ready {
		t.Fatalf("two failures in a row at most: %+v", s)
	}
	u.remote.Script(remoteBase("demo-raw-proxy")+"docs/third.txt", testkit.ServerError())
	u.miss(t, "demo-raw-proxy", "docs/third.txt", http.StatusBadGateway, 1)
	if s := u.state("demo-raw-proxy"); s.Status != proxy.AutoBlocked {
		t.Fatalf("three failures in a row: %+v", s)
	}
	for i := 0; i < 6; i++ {
		path := fmt.Sprintf("docs/open-%d.txt", i)
		u.remote.Script(remoteBase("demo-raw-open")+path, testkit.ServerError())
		u.miss(t, "demo-raw-open", path, http.StatusBadGateway, 1)
	}
	if s := u.state("demo-raw-open"); s.Status != proxy.Ready {
		t.Fatalf("auto block off: %+v", s)
	}
}

// A connection error and a timeout count as failures, and a stale cached
// copy is served while the proxy is auto blocked, with no request.
func TestProxyAutoBlockOnConnectionFailures(t *testing.T) {
	stale := spec("demo-raw-proxy", "docs/stale.txt", []byte("stale bytes\n"), "text/plain")
	u, _ := blockApp(t, []proxyDef{{name: "demo-raw-proxy", format: "raw", attributes: map[string]any{
		"proxy": map[string]any{"contentMaxAge": 0}, "httpclient": map[string]any{"connection": map[string]any{"retries": 0, "timeout": 1}}}}},
		stale)
	u.remote.Script(remoteBase("demo-raw-proxy")+"docs/closed.txt", testkit.Closed())
	u.remote.Script(remoteBase("demo-raw-proxy")+"docs/slow.txt", testkit.SlowHeader(3*time.Second, testkit.OK("late")))
	u.remote.Script(remoteBase("demo-raw-proxy")+"docs/stale.txt", testkit.Closed())
	u.miss(t, "demo-raw-proxy", "docs/closed.txt", http.StatusBadGateway, 1)
	u.miss(t, "demo-raw-proxy", "docs/slow.txt", http.StatusBadGateway, 1)
	if r := u.get("demo-raw-proxy", "docs/stale.txt"); r.status != 200 || string(r.body) != "stale bytes\n" {
		t.Fatalf("a stale copy with a closed remote: %d %q", r.status, r.body)
	}
	if s := u.state("demo-raw-proxy"); s.Status != proxy.AutoBlocked {
		t.Fatalf("after three failures: %+v", s)
	}
	before := len(u.remote.Requests())
	if r := u.get("demo-raw-proxy", "docs/stale.txt"); r.status != 200 || string(r.body) != "stale bytes\n" {
		t.Fatalf("a stale copy while auto blocked: %d %q", r.status, r.body)
	}
	if n := len(u.remote.Requests()) - before; n != 0 {
		t.Fatalf("%d requests while auto blocked", n)
	}
}

// The state of a proxy for the pages of phase 6: ready, blocked by its
// attribute, auto blocked with the time of the next probe, and not ready
// because its settings are refused, each with the cache served.
func TestProxyStates(t *testing.T) {
	u, _ := blockApp(t, []proxyDef{{name: "demo-raw-proxy", format: "raw"},
		{name: "demo-raw-blocked", format: "raw", attributes: map[string]any{"httpclient": map[string]any{"blocked": true}}},
		{name: "demo-raw-broken", format: "raw", attributes: map[string]any{"proxy": map[string]any{"remoteUrl": "ftp://repo.example.com/"}}}})
	for name, want := range map[string]proxy.Status{"demo-raw-proxy": proxy.Ready, "demo-raw-blocked": proxy.Blocked,
		"demo-raw-broken": proxy.Refused} {
		s := u.state(name)
		if s.Status != want || !s.NextProbe.IsZero() || want != proxy.Ready && s.Reason == "" {
			t.Errorf("%s: %+v, want %s", name, s, want)
		}
	}
	if s := u.state("demo-raw-broken"); s.Reason == "" || strings.Contains(s.Reason, "repo.example.com") {
		t.Fatalf("the reason of refused settings: %q", s.Reason)
	}
	for _, name := range []string{"demo-raw-blocked", "demo-raw-broken"} {
		u.miss(t, name, "docs/any.txt", http.StatusNotFound, 0)
	}
}
