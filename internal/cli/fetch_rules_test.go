package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/release"
	"github.com/iogentic/pipesmith-registry/tools/forgestub/forge"
)

// serveWith points update.json of the fixture at a new stub of its forge
// directory, wrapped by wrap and listing extra, and merges more into the
// file.
func (f *fetchFixture) serveWith(t *testing.T, wrap func(http.Handler) http.Handler, extra []forge.Entry, more map[string]any) {
	t.Helper()
	f.stub = forge.New(forge.Options{Dir: f.forge, Owner: "acme", Extra: extra})
	var h http.Handler = f.stub
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	path := filepath.Join(f.root, release.ConfigFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["channel"].(map[string]any)["server"] = srv.URL
	for k, v := range more {
		cfg[k] = v
	}
	if data, err = json.Marshal(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A forge that sends one byte per second, or that lists a thousand versions
// and answers each slowly, ends the fetch stage at its deadline with exit
// code 1.
func TestFetchStageDeadline(t *testing.T) {
	bin := release.BinaryName(runtime.GOOS, runtime.GOARCH)
	cases := map[string]struct {
		wrap  func(http.Handler) http.Handler
		extra []forge.Entry
		more  map[string]any
	}{
		"one byte per second": {
			wrap: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if !strings.HasSuffix(req.URL.Path, "/"+bin) {
						next.ServeHTTP(w, req)
						return
					}
					w.Header().Set("Content-Length", "1000000")
					w.WriteHeader(http.StatusOK)
					for range 8 {
						w.Write([]byte("a"))
						w.(http.Flusher).Flush()
						select {
						case <-req.Context().Done():
							return
						case <-time.After(time.Second):
						}
					}
				})
			},
			more: map[string]any{"timeouts": map[string]any{"stage_seconds": 1}},
		},
		"a thousand versions": {
			wrap: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if strings.HasSuffix(req.URL.Path, "/"+release.PublicKeyFile) {
						select {
						case <-req.Context().Done():
							return
						case <-time.After(100 * time.Millisecond):
						}
					}
					next.ServeHTTP(w, req)
				})
			},
			extra: thousandVersions(),
			more:  map[string]any{"timeouts": map[string]any{"stage_seconds": 1}, "limits": map[string]any{"candidates": 1000}},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFetchFixture(t, "0.2.0")
			f.serveWith(t, c.wrap, c.extra, c.more)
			start := time.Now()
			code, out, errOut := run(t, "update", "--fetch", "--root", f.root)
			if took := time.Since(start); code != 1 || took > 5*time.Second || !strings.Contains(errOut, "deadline") {
				t.Fatalf("%d after %v: %q\n%s", code, took, errOut, out)
			}
			if f.staged(t, "0.2.0") {
				t.Fatal("staged")
			}
		})
	}
}

func thousandVersions() []forge.Entry {
	out := make([]forge.Entry, 1000)
	for i := range out {
		out[i] = forge.Entry{Type: "generic", Name: "pipesmith-registry", Version: "v1." + strconv.Itoa(i) + ".0"}
	}
	return out
}

// A thousand incomplete versions end the stage at the cap of candidates
// with exit code 1.
func TestFetchCandidateCap(t *testing.T) {
	f := newFetchFixture(t)
	f.serveWith(t, nil, thousandVersions(), nil)
	code, out, errOut := run(t, "update", "--fetch", "--root", f.root)
	if code != 1 || !strings.Contains(errOut, "limits.candidates") {
		t.Fatalf("%d %q\n%s", code, errOut, out)
	}
	asked := 0
	for _, req := range f.stub.Requests() {
		if strings.HasSuffix(req.Path, "/"+release.PublicKeyFile) {
			asked++
		}
	}
	if asked != 10 {
		t.Fatalf("%d requests of %s, want 10", asked, release.PublicKeyFile)
	}
}

// --allow-downgrade with --fetch needs --version; without it the flag would
// reach only the scheduled switch (C-5).
func TestFetchAllowDowngradeNeedsVersion(t *testing.T) {
	f, svc := newScheduledFixture(t, "true", "0.2.0")
	for _, args := range [][]string{
		{"update", "--fetch", "--allow-downgrade", "--root", f.root},
		{"update", "--fetch", "--scheduled", "--allow-downgrade", "--root", f.root},
	} {
		code, out, errOut := run(t, args...)
		if code != 2 || out != "" || !strings.Contains(errOut, "--allow-downgrade with --fetch needs --version") {
			t.Errorf("%v: %d %q %q", args, code, out, errOut)
		}
	}
	if len(f.stub.Requests()) != 0 || svc.restarts != 0 || f.staged(t, "0.2.0") {
		t.Fatalf("%d requests, %d restarts", len(f.stub.Requests()), svc.restarts)
	}
}

// update --fetch takes the data directory of a token file from data_dir of
// update.json alone; a token file without data_dir is refused, also when
// the data directory of the environment holds it.
func TestFetchTokenNeedsDataDir(t *testing.T) {
	f := newFetchFixture(t, "0.2.0")
	env := dataDir(t)
	if err := os.MkdirAll(filepath.Join(env, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env, "secrets", "forge.token"), []byte("token-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.serveWith(t, nil, nil, nil)
	path := filepath.Join(f.root, release.ConfigFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	delete(cfg, "data_dir")
	cfg["channel"].(map[string]any)["token_file"] = "forge.token"
	data, _ = json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "update", "--fetch", "--root", f.root)
	if code != 1 || !strings.Contains(errOut, "data_dir") || len(f.stub.Requests()) != 0 || f.staged(t, "0.2.0") {
		t.Fatalf("%d %q after %d requests\n%s", code, errOut, len(f.stub.Requests()), out)
	}
}

// With scheduled_switch, a version whose scheduled switch failed is not
// switched again by the timer, and every later scheduled run ends with exit
// code 1 and names the switch by hand (A-15, C-4).
func TestScheduledSwitchFailedOnce(t *testing.T) {
	f, svc := newScheduledFixture(t, "true", "0.2.0")
	svc.sick["0.2.0"] = true
	code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 1 || f.current(t) != "releases/0.1.0" {
		t.Fatalf("first run: %d %q, current %s\n%s", code, errOut, f.current(t), out)
	}
	restarts := svc.restarts
	for i := range 2 {
		code, out, errOut = run(t, "update", "--fetch", "--scheduled", "--root", f.root)
		if code != 1 || !strings.Contains(errOut, "update --switch 0.2.0") || svc.restarts != restarts {
			t.Fatalf("run %d after the failed switch: %d %q, %d restarts\n%s", i+2, code, errOut, svc.restarts, out)
		}
	}
	svc.sick["0.2.0"] = false
	if code, out, errOut = run(t, "update", "--switch", "0.2.0", "--root", f.root); code != 0 {
		t.Fatalf("switch by hand: %d %q\n%s", code, errOut, out)
	}
	code, out, errOut = run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 0 || f.current(t) != "releases/0.2.0" {
		t.Fatalf("run after the switch by hand: %d %q\n%s", code, errOut, out)
	}
}

// A version staged again after its directory was removed is switched by the
// timer again.
func TestScheduledSwitchAfterTheVersionWasRemoved(t *testing.T) {
	f, svc := newScheduledFixture(t, "true", "0.2.0")
	svc.sick["0.2.0"] = true
	if code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root); code != 1 {
		t.Fatalf("first run: %d %q\n%s", code, errOut, out)
	}
	if err := os.RemoveAll(filepath.Join(f.root, "releases", "0.2.0")); err != nil {
		t.Fatal(err)
	}
	svc.sick["0.2.0"] = false
	code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 0 || f.current(t) != "releases/0.2.0" {
		t.Fatalf("%d %q, current %s\n%s", code, errOut, f.current(t), out)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "releases", ".switch-failed-0.2.0")); err == nil {
		t.Fatal("the mark stayed after the version was staged anew")
	}
}

// An update that switches current to a version as high as the one staged
// while a scheduled fetch runs makes the scheduled switch restart nothing and
// end with exit code 0 (C-11).
func TestScheduledSwitchAfterCurrentMoved(t *testing.T) {
	f, svc := newScheduledFixture(t, "true", "0.2.0")
	bin := release.BinaryName(runtime.GOOS, runtime.GOARCH)
	f.serveWith(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if strings.HasSuffix(req.URL.Path, "/"+bin) {
				if err := os.MkdirAll(filepath.Join(f.root, "releases", "0.3.0"), 0o755); err != nil {
					t.Error(err)
				}
				link := filepath.Join(f.root, release.CurrentLink)
				os.Remove(link)
				if err := os.Symlink("releases/0.3.0", link); err != nil {
					t.Error(err)
				}
			}
			next.ServeHTTP(w, req)
		})
	}, nil, nil)
	code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 0 || errOut != "" || svc.restarts != 0 || f.current(t) != "releases/0.3.0" {
		t.Fatalf("%d %q, %d restarts, current %s\n%s", code, errOut, svc.restarts, f.current(t), out)
	}
	if !f.staged(t, "0.2.0") || !strings.HasSuffix(out, "scheduled_switch: current runs 0.3.0 now, not lower than 0.2.0; nothing restarted\n") {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "releases", ".switch-failed-0.2.0")); err == nil {
		t.Fatal("a mark of a failed switch")
	}
}
