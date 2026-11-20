package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Task 9 of the phase 5 plan: the not found cache (section Settings and
// decision 5.17), with the fake clock.

// notFoundApp is a server with upstream on whose proxies keep their state in
// a catalog of their own and run on the clock returned. Each proxy sends a
// request once and never blocks itself, so the counts at the remote are
// those of the read path.
func notFoundApp(t *testing.T, proxies []proxyDef, blobs ...testkit.BlobSpec) (*upstreamApp, *testkit.Clock) {
	t.Helper()
	clock := testkit.NewClock(time.UnixMilli(createdMS).Add(time.Hour))
	state := openProxyState(t)
	for i := range proxies {
		connection := map[string]any{"retries": 0}
		client := map[string]any{"autoBlock": false, "connection": connection}
		if extra, ok := proxies[i].attributes["httpclient"].(map[string]any); ok {
			for k, v := range extra {
				if more, ok := v.(map[string]any); ok && k == "connection" {
					for ck, cv := range more {
						connection[ck] = cv
					}
					continue
				}
				client[k] = v
			}
		}
		attributes := map[string]any{"httpclient": client}
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

// notFoundCache is the attribute negativeCache of a proxy.
func notFoundCache(enabled bool, minutes int) map[string]any {
	return map[string]any{"negativeCache": map[string]any{"enabled": enabled, "timeToLive": minutes}}
}

func (u *upstreamApp) expectMiss(t *testing.T, repo, path string, status, requests int) {
	t.Helper()
	if r := u.get(repo, path); r.status != status {
		t.Fatalf("%s/%s: %d %q, want %d", repo, path, r.status, r.body, status)
	}
	if n := u.remote.Count(remoteBase(repo) + path); n != requests {
		t.Fatalf("%s/%s: %d requests at the remote, want %d", repo, path, n, requests)
	}
}

// A 404 of the remote is remembered for the time to live of the proxy and
// asked again after it; a path the remote holds by then is fetched.
func TestProxyRemembersA404(t *testing.T) {
	u, clock := notFoundApp(t, []proxyDef{{name: "demo-raw-proxy", format: "raw", attributes: notFoundCache(true, 10)}})
	u.expectMiss(t, "demo-raw-proxy", rawPath, 404, 1)
	clock.Advance(9*time.Minute + 59*time.Second)
	u.expectMiss(t, "demo-raw-proxy", rawPath, 404, 1)
	clock.Advance(time.Second)
	u.expectMiss(t, "demo-raw-proxy", rawPath, 404, 2)
	u.remote.Script(remoteBase("demo-raw-proxy")+rawPath, testkit.OK(rawBytes))
	u.expectMiss(t, "demo-raw-proxy", rawPath, 404, 2) // remembered again for ten minutes
	clock.Advance(10 * time.Minute)
	if r := u.get("demo-raw-proxy", rawPath); r.status != 200 || string(r.body) != rawBytes {
		t.Fatalf("after the time: %d %q", r.status, r.body)
	}
	if n := u.remote.Count(remoteBase("demo-raw-proxy") + rawPath); n != 3 {
		t.Fatalf("%d requests at the remote, want 3", n)
	}
}

// The defaults of decision 5.4 remember a 404 for 1,440 minutes; a proxy
// with the not found cache off sends every miss to the remote.
func TestProxyNotFoundCacheOffAndDefault(t *testing.T) {
	u, clock := notFoundApp(t, []proxyDef{{name: "demo-raw-proxy", format: "raw"},
		{name: "demo-raw-open", format: "raw", attributes: notFoundCache(false, 1440)}})
	for i := 1; i <= 3; i++ {
		if i > 1 {
			clock.Advance(719 * time.Minute)
		}
		u.expectMiss(t, "demo-raw-open", rawPath, 404, i)
		u.expectMiss(t, "demo-raw-proxy", rawPath, 404, 1)
	}
	clock.Advance(2 * time.Minute) // 1,440 minutes after the first 404
	u.expectMiss(t, "demo-raw-proxy", rawPath, 404, 2)
}

// A 500, a timeout and a 401 of the remote are not remembered: the next miss
// asks again.
func TestProxyRemembersNoFailure(t *testing.T) {
	u, _ := notFoundApp(t, []proxyDef{{name: "demo-raw-proxy", format: "raw",
		attributes: map[string]any{"httpclient": map[string]any{"connection": map[string]any{"timeout": 1}}}}})
	for _, c := range []struct {
		path   string
		answer testkit.Answer
		status int
	}{
		{"docs/failed.txt", testkit.ServerError(), http.StatusBadGateway},
		{"docs/slow.txt", testkit.SlowHeader(3*time.Second, testkit.OK(rawBytes)), http.StatusBadGateway},
		{"docs/signed.txt", testkit.BasicChallenge("demo-user", "demo-secret", testkit.OK(rawBytes)), http.StatusNotFound},
	} {
		u.remote.Script(remoteBase("demo-raw-proxy")+c.path, c.answer)
		u.expectMiss(t, "demo-raw-proxy", c.path, c.status, 1)
		u.expectMiss(t, "demo-raw-proxy", c.path, c.status, 2)
	}
}

// Invalidate clears the not found marks of a repository and makes every
// cached path of it stale (decision 5.17): a fresh fetched copy, and a copy
// of the old product without a row, are asked for again; another repository
// keeps its marks.
func TestProxyInvalidate(t *testing.T) {
	old := spec("demo-raw-proxy", "docs/old.txt", []byte("cached by the old product\n"), "text/plain")
	u, clock := notFoundApp(t, []proxyDef{{name: "demo-raw-proxy", format: "raw"}, {name: "demo-raw-other", format: "raw"}}, old)
	u.remote.Script(remoteBase("demo-raw-proxy")+rawPath, testkit.OK(rawBytes).With("ETag", `"v1"`), testkit.NotModified())
	u.remote.Script(remoteBase("demo-raw-proxy")+"docs/old.txt", testkit.NotModified())
	if r := u.get("demo-raw-proxy", rawPath); r.status != 200 {
		t.Fatalf("the fetch: %d %q", r.status, r.body)
	}
	u.expectMiss(t, "demo-raw-proxy", "docs/missing.txt", 404, 1)
	u.expectMiss(t, "demo-raw-other", "docs/missing.txt", 404, 1)
	clock.Advance(time.Minute)
	for _, path := range []string{rawPath, "docs/old.txt", "docs/missing.txt"} {
		u.get("demo-raw-proxy", path)
	}
	if n := len(u.remote.Requests()); n != 3 {
		t.Fatalf("%d requests before the invalidation, want 3", n)
	}
	if err := u.srv.proxies.Invalidate("demo-raw-proxy"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	u.expectMiss(t, "demo-raw-proxy", "docs/missing.txt", 404, 2)
	u.expectMiss(t, "demo-raw-other", "docs/missing.txt", 404, 1)
	for _, path := range []string{rawPath, "docs/old.txt"} {
		if r := u.get("demo-raw-proxy", path); r.status != 200 {
			t.Fatalf("%s after the invalidation: %d %q", path, r.status, r.body)
		}
	}
	requests := u.remote.Requests()
	if len(requests) != 6 {
		t.Fatalf("%d requests after the invalidation, want 6: %+v", len(requests), requests)
	}
	if got := requests[4].Header.Get("If-None-Match"); requests[4].Target != remoteBase("demo-raw-proxy")+rawPath || got != `"v1"` {
		t.Fatalf("the check of the fetched copy: %+v", requests[4])
	}
	for _, path := range []string{rawPath, "docs/old.txt"} {
		u.get("demo-raw-proxy", path)
	}
	if n := len(u.remote.Requests()); n != 6 {
		t.Fatalf("%d requests once checked again, want 6", n)
	}
}
