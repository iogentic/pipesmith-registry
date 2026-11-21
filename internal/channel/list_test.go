package channel

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/release"
	"github.com/iogentic/pipesmith-registry/tools/forgestub/forge"
)

// listQueries returns the query strings of the list requests.
func (r *rig) listQueries() []string {
	var out []string
	for _, req := range r.stub.Requests() {
		if req.Path == "/api/v1/packages/acme" {
			out = append(out, req.Query)
		}
	}
	return out
}

func TestListEveryPage(t *testing.T) {
	r := newRig(t)
	// 59 listed versions without files sort before 0.3.0, so 0.3.0 is
	// entry 60, on the second page of 50.
	for i := 1; i <= 59; i++ {
		r.folder(fmt.Sprintf("0.0.%d", i))
	}
	r.publish(published{version: "0.3.0"})
	r.start()
	r.fetched("0.3.0")
	queries := r.listQueries()
	if len(queries) != 2 {
		t.Fatalf("list requests %v", queries)
	}
	for i, q := range queries {
		for _, part := range []string{"type=generic", "q=" + Package, fmt.Sprintf("page=%d", i+1), "limit=50"} {
			if !strings.Contains(q, part) {
				t.Fatalf("query %d %q lacks %s", i+1, q, part)
			}
		}
	}
	if !strings.Contains(r.out.String(), "listed: 60 versions of pipesmith-registry at "+r.srv.URL+" on 2 pages\n") {
		t.Fatalf("output:\n%s", r.out.String())
	}
}

// alwaysNext answers every list page with a Link to a next page.
func alwaysNext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/v1/packages/acme" {
			w.Header().Set("Link", `<`+req.URL.String()+`>; rel="next"`)
		}
		next.ServeHTTP(w, req)
	})
}

func TestListStopsAtAnEmptyPage(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.wrap = alwaysNext
	r.start()
	r.fetched("0.2.0")
	if n := len(r.listQueries()); n != 2 {
		t.Fatalf("%d list requests, want 2 (the second page is empty)", n)
	}
}

func TestListAtMostHundredPages(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.wrap = func(next http.Handler) http.Handler {
		return alwaysNext(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/api/v1/packages/acme" {
				q := req.URL.Query()
				q.Set("page", "1")
				req.URL.RawQuery = q.Encode()
			}
			next.ServeHTTP(w, req)
		}))
	}
	r.start()
	err := r.refused(ErrList)
	if !strings.Contains(err.Error(), "more than 100 pages") || len(r.listQueries()) != 100 || len(r.requested()) != 0 {
		t.Fatalf("%v after %d list requests, %d file requests", err, len(r.listQueries()), len(r.requested()))
	}
}

func TestListSkipsNamesAndOtherPackages(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	for _, v := range []string{"latest", "v1", "v1.2", "v01.2.3", "1.2.3", "v1.2.3-rc1", "v1.2.3.4", "V2.0.0", ""} {
		r.extra = append(r.extra, forge.Entry{Type: "generic", Name: Package, Version: v})
	}
	r.extra = append(r.extra,
		forge.Entry{Type: "pypi", Name: Package, Version: "v9.0.0"},
		forge.Entry{Type: "generic", Name: Package + "-ui", Version: "v9.0.1"},
		forge.Entry{Type: "generic", Name: "Pipesmith-Registry", Version: "v9.0.2"})
	r.start()
	r.fetched("0.2.0")
	for _, path := range r.requested() {
		if !strings.HasPrefix(path, "v0.2.0/") {
			t.Fatalf("requested %s", path)
		}
	}
	if !strings.Contains(r.out.String(), "listed: 1 versions") {
		t.Fatalf("output:\n%s", r.out.String())
	}
}

func TestCandidateIsHighestComplete(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.publish(published{version: "0.1.5"})
	r.publish(published{version: "0.3.0", noPub: true})
	r.publish(published{version: "0.0.9"})
	r.start()
	r.fetched("0.2.0")
	var skipped []string
	for _, line := range strings.Split(r.out.String(), "\n") {
		if strings.HasPrefix(line, "skipped: ") {
			skipped = append(skipped, line)
		}
	}
	if len(skipped) != 1 || skipped[0] != "skipped: v0.3.0 is incomplete, cosign.pub answers 404" {
		t.Fatalf("skip lines %q", skipped)
	}
	for _, path := range r.requested() {
		if strings.HasPrefix(path, "v0.3.0/") && path != "v0.3.0/cosign.pub" || strings.HasPrefix(path, "v0.1.5/") || strings.HasPrefix(path, "v0.0.9/") {
			t.Fatalf("requested %s", path)
		}
	}
	if !slices.Contains(r.releases(), "0.2.0") {
		t.Fatalf("releases %v", r.releases())
	}
}

func TestNothingToDo(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.1.0"})
	r.publish(published{version: "0.0.9"})
	r.publish(published{version: "0.2.0", noPub: true})
	r.start()
	res, err := r.f.Fetch(Options{})
	if err != nil || !res.Nothing {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.HasSuffix(r.out.String(), "nothing to do: no complete version above 0.1.0 at "+r.srv.URL+"\n") {
		t.Fatalf("output:\n%s", r.out.String())
	}
	if got := strings.Join(r.releases(), ","); got != "0.1.0" {
		t.Fatalf("releases %s", got)
	}
	if got := strings.Join(r.requested(), ","); got != "v0.2.0/cosign.pub" {
		t.Fatalf("requested %s", got)
	}
}

func TestFailedCandidateStopsTheStage(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.publish(published{version: "0.3.0", signer: newOther(t)})
	r.start()
	r.refused(release.ErrSignature)
	for _, path := range r.requested() {
		if !strings.HasPrefix(path, "v0.3.0/") {
			t.Fatalf("a file of a lower version was requested: %s", path)
		}
	}
	if exists(r.root+"/releases/0.3.0.partial") || exists(r.root+"/releases/0.2.0") {
		t.Fatalf("releases %v", r.releases())
	}
}

// The fetch stage locks releases/ and not the root (decision P8-18).
func TestOwnerAndLockOfTheRoot(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.publish(published{version: "0.3.0"})
	r.start()
	hold, err := release.HoldReleases(r.root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.f.Fetch(Options{})
	hold.Release()
	if !errors.Is(err, release.ErrFetching) || len(r.stub.Requests()) != 0 {
		t.Fatalf("under a held lock of releases/: %v, %d requests", err, len(r.stub.Requests()))
	}
	hold, err = release.HoldRoot(r.root)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	if _, err := r.f.Fetch(Options{Version: "0.2.0"}); err != nil {
		t.Fatalf("under a held lock of the root: %v", err)
	}
	r.fetched("0.3.0")
}

func TestSettingsRefusals(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.start()
	for name, cfg := range map[string]map[string]any{
		"empty channel":      {"trusted_key": r.trusted.Pub, "channel": map[string]any{}},
		"channel null":       {"trusted_key": r.trusted.Pub, "channel": nil},
		"no server":          {"trusted_key": r.trusted.Pub, "channel": map[string]any{"owner": "acme"}},
		"no owner":           {"trusted_key": r.trusted.Pub, "channel": map[string]any{"server": r.srv.URL}},
		"owner with a slash": {"trusted_key": r.trusted.Pub, "channel": map[string]any{"server": r.srv.URL, "owner": "a/b"}},
		"unknown key":        {"trusted_key": r.trusted.Pub, "channel": map[string]any{"server": r.srv.URL, "owner": "acme", "sever": 1}},
		"ftp":                {"trusted_key": r.trusted.Pub, "channel": map[string]any{"server": "ftp://127.0.0.1", "owner": "acme"}},
		"user information":   {"trusted_key": r.trusted.Pub, "channel": map[string]any{"server": "https://u:p@forge.example.test", "owner": "acme"}},
		"query":              {"trusted_key": r.trusted.Pub, "channel": map[string]any{"server": "https://forge.example.test/?a=b", "owner": "acme"}},
		"api path":           {"trusted_key": r.trusted.Pub, "channel": map[string]any{"server": "https://forge.example.test/api/v1", "owner": "acme"}},
		"http to a name":     {"trusted_key": r.trusted.Pub, "channel": map[string]any{"server": "http://forge.example.test", "owner": "acme"}},
		"port":               {"trusted_key": r.trusted.Pub, "channel": map[string]any{"server": "https://forge.example.test:0", "owner": "acme"}},
	} {
		r.config(cfg)
		_, err := r.f.Fetch(Options{})
		if err == nil || !strings.Contains(err.Error(), "channel") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := len(r.stub.Requests()); n != 0 {
		t.Fatalf("%d requests after refused settings", n)
	}
}

func TestServerPrefix(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.wrap = func(next http.Handler) http.Handler { return http.StripPrefix("/forge", next) }
	r.prefix = "/forge/"
	r.start()
	r.fetched("0.2.0")
}
