package channel

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/release"
	"github.com/iogentic/pipesmith-registry/tools/githubstub/gh"
)

// startGitHub serves the folders of the rig as the releases of
// acme/pipesmith-registry and writes update.json with the GitHub form of
// channel to it.
func (r *rig) startGitHub(opt gh.Options) *gh.Stub {
	r.t.Helper()
	opt.Dir, opt.Owner, opt.Repo = r.stubDir, "acme", "pipesmith-registry"
	stub := gh.New(opt)
	var h http.Handler = stub
	if r.wrap != nil {
		h = r.wrap(h)
	}
	r.srv = httptest.NewServer(h)
	r.t.Cleanup(r.srv.Close)
	r.config(map[string]any{"trusted_key": r.trusted.Pub, "data_dir": r.data,
		"channel": map[string]any{"github": "acme/pipesmith-registry", "api": r.srv.URL}})
	return stub
}

func TestGitHubStagesTheHighestRelease(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	files := r.publish(published{version: "0.3.0"})
	if err := os.WriteFile(filepath.Join(r.stubDir, "0.3.0", "notes.md"), []byte("# 0.3.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := r.startGitHub(gh.Options{})
	res := r.fetched("0.3.0")
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(res.Dir, name))
		if name == "sbom.spdx.json" || name == release.PublicKeyFile {
			if err == nil {
				t.Fatalf("%s was downloaded; the stage needs it not", name)
			}
			continue
		}
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %v, content equal %v", name, err, bytes.Equal(got, want))
		}
	}
	if exists(filepath.Join(res.Dir, "notes.md")) {
		t.Fatal("notes.md is no asset and was downloaded")
	}
	assets, lists := 0, 0
	for _, req := range stub.Requests() {
		if req.Agent != "pipesmith-registry" {
			t.Fatalf("request %s with User-Agent %q, want the product name without a version", req.Path, req.Agent)
		}
		switch {
		case strings.Contains(req.Path, "/releases/assets/"):
			assets++
			if req.Accept != "application/octet-stream" {
				t.Fatalf("asset request with Accept %q", req.Accept)
			}
		case strings.HasSuffix(req.Path, "/releases"):
			lists++
			if req.Accept != "application/vnd.github+json" {
				t.Fatalf("list request with Accept %q", req.Accept)
			}
		}
	}
	// SHA256SUMS, the bundle, the binary and the bill
	if assets != 4 || lists != 1 {
		t.Fatalf("%d asset and %d list requests, want 4 and 1\n%s", assets, lists, r.out.String())
	}
	if !strings.Contains(r.out.String(), "listed: 2 versions of acme/pipesmith-registry") {
		t.Fatalf("output:\n%s", r.out.String())
	}
}

func TestGitHubSkipsDraftsPrereleasesAndIncompleteReleases(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.publish(published{version: "0.3.0", edit: func(files map[string][]byte) { delete(files, BillFile) },
		sumsEdit: func(sums string) string { return sums }})
	r.publish(published{version: "0.4.0"})
	r.publish(published{version: "0.5.0"})
	for name, version := range map[string]string{"prerelease": "0.4.0", "draft": "0.5.0"} {
		if err := os.WriteFile(filepath.Join(r.stubDir, version, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r.startGitHub(gh.Options{})
	r.fetched("0.2.0")
	if !strings.Contains(r.out.String(), "skipped: v0.3.0 is incomplete, its release has no asset sbom.cdx.json") {
		t.Fatalf("output:\n%s", r.out.String())
	}
	if !strings.Contains(r.out.String(), "listed: 2 versions") {
		t.Fatalf("the draft or the prerelease was listed:\n%s", r.out.String())
	}
}

func TestGitHubNamedIncompleteVersionIsRefused(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.3.0", noPub: true})
	r.startGitHub(gh.Options{})
	before := strings.Join(r.releases(), ",")
	_, err := r.f.Fetch(Options{Version: "0.3.0"})
	if !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "no asset cosign.pub") {
		t.Fatalf("error %v", err)
	}
	if after := strings.Join(r.releases(), ","); after != before {
		t.Fatalf("releases/ %s, then %s", before, after)
	}
}

func TestGitHubWrongSignatureRunsNoBinary(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.3.0", signer: newOther(t)})
	r.startGitHub(gh.Options{})
	r.refused(release.ErrSignature)
	if strings.Contains(r.out.String(), "version:") || strings.Contains(r.out.String(), "downloaded: "+platformBinary()) {
		t.Fatalf("the binary was downloaded or run before the signature passed:\n%s", r.out.String())
	}
}

func TestGitHubRateLimitIsNamed(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.3.0"})
	reset := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	stub := r.startGitHub(gh.Options{Limit: 1, Reset: reset})
	stub.SetLimit(1)
	// the one request of the limit is the list; the first asset is refused
	err := r.refused(ErrRateLimit)
	var rl *RateLimitError
	if !errors.As(err, &rl) || !rl.Reset.Equal(reset) || rl.Code != 403 {
		t.Fatalf("error %v (%#v)", err, rl)
	}
	if !strings.Contains(err.Error(), "until 2026-11-21T12:00:00Z") {
		t.Fatalf("error %v names no reset time", err)
	}
}

func TestGitHubRetryAfter(t *testing.T) {
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"120"}}}
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var rl *RateLimitError
	if err := rateLimit(resp, now); !errors.As(err, &rl) || !rl.Reset.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("429 with Retry-After: %v", err)
	}
	plain := &http.Response{StatusCode: 403, Header: http.Header{"X-Ratelimit-Remaining": {"12"}}}
	if err := rateLimit(plain, now); err != nil {
		t.Fatalf("a 403 with requests left is no rate limit: %v", err)
	}
}

func TestGitHubAssetOutsideTheAPIIsRefused(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.3.0"})
	r.wrap = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if !strings.HasSuffix(req.URL.Path, "/releases") {
				next.ServeHTTP(w, req)
				return
			}
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, req)
			body := strings.ReplaceAll(rec.Body.String(), "http://"+req.Host+"/repos/", "http://203.0.113.9/repos/")
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
		})
	}
	stub := r.startGitHub(gh.Options{})
	err := r.refused(ErrList)
	if !strings.Contains(err.Error(), "outside") {
		t.Fatalf("error %v", err)
	}
	if n := len(stub.Requests()); n != 1 {
		t.Fatalf("%d requests, want the list alone", n)
	}
}

func TestGitHubRedirectRule(t *testing.T) {
	from, _ := http.NewRequest("GET", "https://api.github.com/repos/a/b/releases/assets/1", nil)
	for target, ok := range map[string]bool{
		"https://release-assets.githubusercontent.com/x":      true,
		"https://objects.githubusercontent.com/x":             true,
		"http://release-assets.githubusercontent.com/x":       false,
		"https://release-assets.githubusercontent.com:8443/x": false,
		"https://example.com/x":                               false,
		"https://api.github.com/other":                        true,
	} {
		to, _ := http.NewRequest("GET", target, nil)
		if err := gitHubRedirect(to, []*http.Request{from}); (err == nil) != ok {
			t.Errorf("redirect to %s: %v, want allowed %v", target, err, ok)
		}
	}
}

func TestGitHubSettings(t *testing.T) {
	dir := t.TempDir()
	write := func(text string) *release.UpdateConfig {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, release.ConfigFile), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := release.LoadUpdateConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	s, err := ParseSettings(write(`{"trusted_key": "k.pub"}`))
	if err != nil || s.GitHub == nil || s.GitHub.String() != DefaultRepository || s.Server.String() != DefaultAPI {
		t.Fatalf("no channel: %+v, %v", s, err)
	}
	s, err = ParseSettings(write(`{"channel": {"github": "acme/tool", "api": "http://127.0.0.1:14150"}}`))
	if err != nil || s.GitHub.String() != "acme/tool" || s.Server.String() != "http://127.0.0.1:14150" {
		t.Fatalf("github form: %+v, %v", s, err)
	}
	for text, want := range map[string]string{
		`{"channel": null}`:                    "channel is null",
		`{"channel": {}}`:                      "no server and no github",
		`{"channel": {"github": "acme"}}`:      "not owner/repo",
		`{"channel": {"github": "acme/../x"}}`: "not owner/repo",
		`{"channel": {"github": "acme/tool", "api": "http://192.0.2.1"}}`:                "plain http only to a loopback",
		`{"channel": {"github": "acme/tool", "token_file": "t"}}`:                        "without a token",
		`{"channel": {"github": "acme/tool", "server": "https://f.example"}}`:            "give one form",
		`{"channel": {"server": "https://f.example", "owner": "o", "api": "https://a"}}`: "belongs to the form with github",
	} {
		if _, err := ParseSettings(write(text)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", text, err, want)
		}
	}
}

func TestReleasesListsNotesAndPrereleases(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.folder("0.3.0")
	if err := os.WriteFile(filepath.Join(r.stubDir, "0.3.0", "notes.md"), []byte("## Notes\n\n<b>x</b>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.stubDir, "0.3.0", "prerelease"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r.startGitHub(gh.Options{})
	cfg, err := release.LoadUpdateConfig(r.root)
	if err != nil {
		t.Fatal(err)
	}
	set, err := ParseSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	list, err := Releases(context.Background(), set, ListOptions{})
	if err != nil || len(list) != 2 {
		t.Fatalf("%d releases, %v", len(list), err)
	}
	if list[0].Tag != "v0.3.0" || !list[0].Prerelease || list[0].Body != "## Notes\n\n<b>x</b>\n" || list[0].Complete(Platform()) {
		t.Fatalf("first %+v", list[0])
	}
	if list[1].Version.String() != "0.2.0" || !list[1].OK || !list[1].Complete(Platform()) || list[1].Published.IsZero() {
		t.Fatalf("second %+v", list[1])
	}
}

func TestGitHubClientTakesTheAssetRedirectOnlyForGitHub(t *testing.T) {
	from, _ := http.NewRequest("GET", "https://api.github.com/repos/a/b/releases/assets/1", nil)
	to, _ := http.NewRequest("GET", "https://release-assets.githubusercontent.com/x", nil)
	for api, ok := range map[string]bool{"https://api.github.com": true, "http://127.0.0.1:14150": false} {
		server, err := parseServer(api)
		if err != nil {
			t.Fatal(err)
		}
		set := &Settings{Server: server, GitHub: &Repo{Owner: "a", Name: "b"}, Timeouts: defaultTimeouts}
		c := newClient(set, "", nil, nil)
		if err := c.http.CheckRedirect(to, []*http.Request{from}); (err == nil) != ok {
			t.Errorf("api %s: redirect to the asset host %v, want allowed %v", api, err, ok)
		}
	}
}

func TestGitHubAssetAddress(t *testing.T) {
	server, _ := parseServer("https://api.github.com")
	set := &Settings{Server: server, GitHub: &Repo{Owner: "IOgentic", Name: "Pipesmith-Registry"}}
	for target, ok := range map[string]bool{
		"https://api.github.com/repos/iogentic/pipesmith-registry/releases/assets/773103929": true,
		"https://api.github.com/repos/IOGENTIC/PIPESMITH-REGISTRY/releases/assets/1":         true,
		"https://api.github.com/repos/other/pipesmith-registry/releases/assets/1":            false,
		"https://api.github.com/repos/iogentic/pipesmith-registry/releases/assets/1/x":       false,
		"https://api.github.com/repos/iogentic/pipesmith-registry/releases/assets/x":         false,
		"https://api.github.com/repos/iogentic/pipesmith-registry/releases/assets/1?a=b":     false,
		"https://api.github.com:8443/repos/iogentic/pipesmith-registry/releases/assets/1":    false,
		"http://api.github.com/repos/iogentic/pipesmith-registry/releases/assets/1":          false,
		"https://u@api.github.com/repos/iogentic/pipesmith-registry/releases/assets/1":       false,
		"https://evil.example/repos/iogentic/pipesmith-registry/releases/assets/1":           false,
	} {
		if inside(target, set) != ok {
			t.Errorf("%s: inside %v", target, !ok)
		}
	}
}
