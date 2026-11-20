package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Task 13 of the phase 5 plan, the operation that invalidates a cache
// (decision 5.17): POST /service/rest/v1/repositories/{name}/invalidate-cache
// calls Invalidate of leg 5b for a proxy, and for a group for every proxy
// member, the members of groups inside it included. Status codes, the
// permission and the answers come from the public source of the old
// product, tag release-3.87.2-01, read 2026-11-19: RepositoriesApiResourceDoc.java
// declares 204 for a cache invalidated, 400 for a repository that is neither
// proxy nor group, 401, 403 and 404 for an unknown repository;
// AuthorizingRepositoryManagerImpl.java asks the edit action of the
// administration permission of the repository before the type. A missing
// repository and a forbidden one answer alike here, as every rule of a
// repository does (review 6, finding 7).

const invalidatePath = "/service/rest/v1/repositories/%s/invalidate-cache"

func invalidate(u *upstreamApp, repo string, headers ...string) *answer {
	u.t.Helper()
	return do(u.t, u.addr(), "POST", strings.Replace(invalidatePath, "%s", repo, 1), nil, headers...)
}

func TestInvalidateCache(t *testing.T) {
	state := openProxyState(t)
	clock := testkit.NewClock(time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC))
	u := startUpstream(t, upstreamSetup{upstream: true,
		proxies: []proxyDef{{name: "demo-raw-proxy", format: "raw"}, {name: "demo-raw-other", format: "raw"}},
		repos: []repoDef{{name: "demo-raw-inner", format: "raw", kind: "group", members: []string{"demo-raw-other"}},
			{name: "demo-raw-group", format: "raw", kind: "group", members: []string{"demo-raw-proxy", "demo-raw", "demo-raw-inner"}}},
		tune: func(o *appOptions) {
			o.proxyState = state
			o.upstreamConfig = proxy.Config{Now: clock.Now}
		}})
	a, other := remoteBase("demo-raw-proxy"), remoteBase("demo-raw-other")
	u.remote.Script(a+"docs/a.txt", testkit.OK("bytes of a").With("ETag", `"a-1"`), testkit.NotModified())
	u.remote.Script(other+"docs/b.txt", testkit.OK("bytes of b").With("ETag", `"b-1"`), testkit.NotModified())
	// a copy each, and a 404 the not found cache keeps
	for i := 0; i < 2; i++ {
		u.get("demo-raw-proxy", "docs/a.txt")
		u.get("demo-raw-other", "docs/b.txt")
		u.get("demo-raw-proxy", "docs/missing.txt")
	}
	counts := func() [3]int {
		return [3]int{u.remote.Count(a + "docs/a.txt"), u.remote.Count(other + "docs/b.txt"), u.remote.Count(a + "docs/missing.txt")}
	}
	if got := counts(); got != [3]int{1, 1, 1} {
		t.Fatalf("requests before an invalidate %v", got)
	}
	clock.Advance(time.Millisecond)
	r := invalidate(u, "demo-raw-proxy", "basic", asAdmin)
	if r.status != http.StatusNoContent || len(r.body) != 0 {
		t.Fatalf("invalidate of a proxy: %d %v %q", r.status, r.header, r.body)
	}
	for i := 0; i < 2; i++ {
		u.get("demo-raw-proxy", "docs/a.txt")
		u.get("demo-raw-other", "docs/b.txt")
		u.get("demo-raw-proxy", "docs/missing.txt")
	}
	// the stale copy is checked once with its entity tag, the mark is gone, the other proxy is left alone
	if got := counts(); got != [3]int{2, 1, 2} {
		t.Fatalf("requests after the invalidate of one proxy %v", got)
	}
	checked := false
	for _, req := range u.remote.Requests() {
		checked = checked || req.Target == a+"docs/a.txt" && req.Header.Get("If-None-Match") == `"a-1"`
	}
	if !checked {
		t.Fatalf("the stale copy was not checked with its entity tag: %+v", u.remote.Requests())
	}
	clock.Advance(time.Millisecond)
	if r := invalidate(u, "demo-raw-group", "basic", asAdmin); r.status != http.StatusNoContent {
		t.Fatalf("invalidate of a group: %d %q", r.status, r.body)
	}
	u.get("demo-raw-proxy", "docs/a.txt")
	u.get("demo-raw-other", "docs/b.txt")
	if got := counts(); got != [3]int{3, 2, 2} {
		t.Fatalf("requests after the invalidate of the group %v: every proxy member, the one of the inner group too", got)
	}
	u.sec.addBasic("demo-viewer:viewer-pw", subject("demo-viewer", "", "nexus:repository-admin:raw:demo-raw-proxy:read"))
	u.sec.addBasic("demo-editor:editor-pw", subject("demo-editor", "", "nexus:repository-admin:raw:demo-raw-proxy:edit"))
	for _, c := range []struct {
		repo    string
		headers []string
		status  int
	}{
		{"demo-raw", []string{"basic", asAdmin}, http.StatusBadRequest},
		{"demo-missing", []string{"basic", asAdmin}, http.StatusNotFound},
		{"demo-raw-proxy", nil, http.StatusUnauthorized},
		{"demo-raw-proxy", []string{"basic", asReader}, http.StatusForbidden},
		{"demo-raw-proxy", []string{"basic", "demo-viewer:viewer-pw"}, http.StatusForbidden},
		{"demo-raw-proxy", []string{"basic", "demo-editor:editor-pw"}, http.StatusNoContent},
		{"demo-raw-proxy", []string{"Cookie", "fake-session=s1"}, http.StatusForbidden},
		{"demo-raw-proxy", []string{"Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1"}, http.StatusNoContent},
	} {
		r := invalidate(u, c.repo, c.headers...)
		if r.status != c.status {
			t.Errorf("%s %v: %d %q, want %d", c.repo, c.headers, r.status, r.body, c.status)
		}
		if c.status == http.StatusBadRequest && !strings.Contains(string(r.body), "proxy or a group") {
			t.Errorf("the refusal of a hosted repository: %q", r.body)
		}
	}
}

// With upstream off no proxy keeps a cache state: the checks of the
// repository answer as with upstream on, and a proxy or a group answers 503
// with the switch named.
func TestInvalidateCacheWithUpstreamOff(t *testing.T) {
	u := startUpstream(t, upstreamSetup{proxies: []proxyDef{{name: "demo-raw-proxy", format: "raw"}},
		repos: []repoDef{{name: "demo-raw-group", format: "raw", kind: "group", members: []string{"demo-raw-proxy"}}}})
	for _, c := range []struct {
		repo   string
		status int
	}{{"demo-raw-proxy", http.StatusServiceUnavailable}, {"demo-raw-group", http.StatusServiceUnavailable},
		{"demo-raw", http.StatusBadRequest}, {"demo-missing", http.StatusNotFound}} {
		r := invalidate(u, c.repo, "basic", asAdmin)
		if r.status != c.status {
			t.Errorf("%s: %d %q, want %d", c.repo, r.status, r.body, c.status)
		}
		if c.status == http.StatusServiceUnavailable && !strings.Contains(string(r.body), "upstream") {
			t.Errorf("%s: the answer does not name the switch: %q", c.repo, r.body)
		}
	}
}

// The operation is a route of every table with security on, the first one
// a POST of its path meets, as the route walk meets it, with the rule of the
// administration permission and the action edit; no refusal declares it any
// more.
func TestInvalidateIsARouteOfEverySecuredTable(t *testing.T) {
	for name, table := range map[string][]route{"securedRoutes": securedRoutes, "writingRoutes": writingRoutes} {
		var first *route
		for i := range table {
			if table[i].Pattern.MatchString("/service/rest/v1/repositories/demo-raw-proxy/invalidate-cache") &&
				strings.Contains(strings.Join(table[i].Methods, ","), "POST") {
				first = &table[i]
				break
			}
		}
		if first == nil || first.Rule.kind != ruleAdmin || first.Rule.action != "edit" || first.Rule.group != 1 {
			t.Errorf("%s: the first route of the operation is %+v", name, first)
		}
	}
	for _, d := range slicesConcat(writeRefusals, refusalTable) {
		for _, path := range d.paths() {
			if strings.HasSuffix(path, "/invalidate-cache") {
				t.Errorf("the refusal %s still declares %s", d.template, path)
			}
		}
	}
}

func slicesConcat(a, b []refusalDecl) []refusalDecl {
	return append(append([]refusalDecl(nil), a...), b...)
}
