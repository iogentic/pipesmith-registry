package channel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

func TestStageHoldsTheFourFiles(t *testing.T) {
	r := newRig(t)
	files := r.publish(published{version: "0.2.0"})
	r.start()
	res := r.fetched("0.2.0")
	dir := filepath.Join(r.root, "releases", "0.2.0")
	if res.Dir != dir || res.Existed {
		t.Fatalf("result %+v", res)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o644)
		if e.Name() == platformBinary() {
			want = 0o755
		}
		if info.Mode() != want {
			t.Errorf("%s has mode %v, want %v", e.Name(), info.Mode(), want)
		}
		got, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		served, _ := os.ReadFile(r.stubFile("0.2.0", e.Name()))
		if !bytes.Equal(got, served) {
			t.Errorf("%s differs from the published file", e.Name())
		}
	}
	want := []string{release.SumsFile, release.BundleFile, platformBinary(), BillFile}
	slices.Sort(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("staged %v, want %v", names, want)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o755 {
		t.Fatalf("directory mode %v", info.Mode())
	}
	if _, err := release.Check(dir, release.Options{TrustedKey: r.trusted.Pub}); err != nil {
		t.Fatalf("the staged directory fails the check of the switch: %v", err)
	}
	if got := strings.Join(r.releases(), ","); got != "0.1.0,0.2.0" {
		t.Fatalf("releases %s", got)
	}
	if !bytes.Equal(files[platformBinary()], mustRead(t, filepath.Join(dir, platformBinary()))) {
		t.Fatal("binary differs")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSecondFetchRequestsNoFile(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.start()
	r.fetched("0.2.0")
	before := len(r.stub.Requests())
	res := r.fetched("0.2.0")
	if !res.Existed {
		t.Fatalf("result %+v", res)
	}
	for _, req := range r.stub.Requests()[before:] {
		if strings.HasPrefix(req.Path, "/api/packages/") {
			t.Fatalf("the second fetch requested %s", req.Path)
		}
	}
	if !strings.HasSuffix(r.out.String(), "staged: releases/0.2.0 exists, nothing fetched\n") {
		t.Fatalf("output:\n%s", r.out.String())
	}
}

func TestLeftoverPartialIsReplaced(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.start()
	junk := filepath.Join(r.root, "releases", "0.2.0.partial", "junk")
	if err := os.MkdirAll(filepath.Dir(junk), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(junk, []byte("x"), 0o600)
	r.fetched("0.2.0")
	if exists(filepath.Dir(junk)) || exists(filepath.Join(r.root, "releases", "0.2.0", "junk")) {
		t.Fatal("the leftover stayed")
	}
}

// onFile serves the file name of every version with h and the rest with the stub.
func onFile(name string, h func(w http.ResponseWriter, req *http.Request, next http.Handler)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if strings.HasSuffix(req.URL.Path, "/"+name) {
				h(w, req, next)
				return
			}
			next.ServeHTTP(w, req)
		})
	}
}

func TestShortFile(t *testing.T) {
	r := newRig(t)
	files := r.publish(published{version: "0.2.0"})
	r.wrap = onFile(platformBinary(), func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
		body := files[platformBinary()]
		w.Header().Set("Content-Length", strconv.Itoa(len(body)+10))
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	r.start()
	err := r.refused(ErrShort)
	if !strings.Contains(err.Error(), platformBinary()) {
		t.Fatalf("message %v", err)
	}
}

func TestBodyAboveItsCap(t *testing.T) {
	t.Run("Content-Length above the cap", func(t *testing.T) {
		r := newRig(t)
		r.publish(published{version: "0.2.0"})
		r.start(nil, map[string]any{"limits": map[string]any{"binary_bytes": 4096}})
		err := r.refused(ErrTooLarge)
		if !strings.Contains(err.Error(), "Content-Length") || !strings.Contains(err.Error(), "4096") {
			t.Fatalf("message %v", err)
		}
	})
	t.Run("a body without Content-Length that grows past the cap", func(t *testing.T) {
		r := newRig(t)
		r.publish(published{version: "0.2.0"})
		r.wrap = onFile(release.SumsFile, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
			w.WriteHeader(http.StatusOK)
			for range 64 {
				w.Write(bytes.Repeat([]byte("a"), 1024))
				w.(http.Flusher).Flush()
			}
		})
		r.start(nil, map[string]any{"limits": map[string]any{"sums_bytes": 2048}})
		err := r.refused(ErrTooLarge)
		if !strings.Contains(err.Error(), release.SumsFile) || strings.Contains(err.Error(), "Content-Length") {
			t.Fatalf("message %v", err)
		}
	})
	for _, c := range []struct{ key, file string }{{"bundle_bytes", release.BundleFile}, {"bill_bytes", BillFile}} {
		t.Run(c.key, func(t *testing.T) {
			r := newRig(t)
			r.publish(published{version: "0.2.0"})
			r.start(nil, map[string]any{"limits": map[string]any{c.key: 16}})
			if err := r.refused(ErrTooLarge); !strings.Contains(err.Error(), c.file) {
				t.Fatalf("message %v", err)
			}
		})
	}
}

func TestRedirects(t *testing.T) {
	t.Run("another host", func(t *testing.T) {
		r := newRig(t)
		r.publish(published{version: "0.2.0"})
		var elsewhere atomic.Int64
		other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
		defer other.Close()
		r.wrap = onFile(release.SumsFile, func(w http.ResponseWriter, req *http.Request, _ http.Handler) {
			http.Redirect(w, req, other.URL+req.URL.Path, http.StatusFound)
		})
		r.start()
		err := r.refused(ErrRedirect)
		if elsewhere.Load() != 0 || !strings.Contains(err.Error(), other.Listener.Addr().String()) {
			t.Fatalf("%v; %d requests elsewhere", err, elsewhere.Load())
		}
	})
	t.Run("another port of the same address", func(t *testing.T) {
		r := newRig(t)
		r.publish(published{version: "0.2.0"})
		other := httptest.NewServer(http.NotFoundHandler())
		defer other.Close()
		_, port, _ := net.SplitHostPort(other.Listener.Addr().String())
		r.wrap = onFile(release.BundleFile, func(w http.ResponseWriter, req *http.Request, _ http.Handler) {
			http.Redirect(w, req, "http://127.0.0.1:"+port+req.URL.Path, http.StatusMovedPermanently)
		})
		r.start()
		r.refused(ErrRedirect)
	})
	t.Run("another scheme", func(t *testing.T) {
		r := newRig(t)
		r.publish(published{version: "0.2.0"})
		r.wrap = onFile(BillFile, func(w http.ResponseWriter, req *http.Request, _ http.Handler) {
			http.Redirect(w, req, "https://"+req.Host+req.URL.Path, http.StatusTemporaryRedirect)
		})
		r.start()
		r.refused(ErrRedirect)
	})
	t.Run("the same host is followed three times", func(t *testing.T) {
		r := newRig(t)
		r.publish(published{version: "0.2.0"})
		r.wrap = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				hop, _ := strconv.Atoi(req.URL.Query().Get("hop"))
				if strings.HasSuffix(req.URL.Path, "/"+platformBinary()) && hop < 3 {
					http.Redirect(w, req, req.URL.Path+"?hop="+strconv.Itoa(hop+1), http.StatusFound)
					return
				}
				next.ServeHTTP(w, req)
			})
		}
		r.start()
		r.fetched("0.2.0")
	})
	t.Run("a fourth redirect is refused", func(t *testing.T) {
		r := newRig(t)
		r.publish(published{version: "0.2.0"})
		r.wrap = onFile(platformBinary(), func(w http.ResponseWriter, req *http.Request, _ http.Handler) {
			http.Redirect(w, req, req.URL.Path, http.StatusFound)
		})
		r.start()
		r.refused(ErrRedirect)
	})
}

func TestTimeouts(t *testing.T) {
	t.Run("header", func(t *testing.T) {
		r := newRig(t)
		r.publish(published{version: "0.2.0"})
		r.wrap = onFile(release.SumsFile, func(w http.ResponseWriter, req *http.Request, _ http.Handler) {
			select {
			case <-req.Context().Done():
			case <-time.After(20 * time.Second):
			}
		})
		r.start(nil, map[string]any{"timeouts": map[string]any{"header_seconds": 1}})
		start := time.Now()
		r.refused(ErrDownload)
		if took := time.Since(start); took > 10*time.Second {
			t.Fatalf("took %v", took)
		}
	})
	t.Run("total", func(t *testing.T) {
		r := newRig(t)
		r.publish(published{version: "0.2.0"})
		r.wrap = onFile(release.SumsFile, func(w http.ResponseWriter, req *http.Request, _ http.Handler) {
			w.WriteHeader(http.StatusOK)
			for {
				w.Write([]byte("a"))
				w.(http.Flusher).Flush()
				select {
				case <-req.Context().Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
			}
		})
		r.start(nil, map[string]any{"timeouts": map[string]any{"total_seconds": 1}})
		start := time.Now()
		r.refused(ErrDownload)
		if took := time.Since(start); took > 10*time.Second {
			t.Fatalf("took %v", took)
		}
	})
}

func TestLimitsAndTimeoutsSettings(t *testing.T) {
	r := newRig(t)
	r.start()
	cfg, err := release.LoadUpdateConfig(r.root)
	if err != nil {
		t.Fatal(err)
	}
	set, err := ParseSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if set.Limits != (Limits{Sums: 1 << 20, Bundle: 1 << 20, Binary: 512 << 20, Bill: 64 << 20}) ||
		set.Timeouts != (Timeouts{Dial: 10 * time.Second, Header: 30 * time.Second, Total: 1800 * time.Second}) {
		t.Fatalf("defaults %+v %+v", set.Limits, set.Timeouts)
	}
	for name, more := range map[string]map[string]any{
		"zero cap":          {"limits": map[string]any{"binary_bytes": 0}},
		"negative cap":      {"limits": map[string]any{"sums_bytes": -1}},
		"fraction":          {"limits": map[string]any{"bill_bytes": 1.5}},
		"text":              {"timeouts": map[string]any{"dial_seconds": "10"}},
		"unknown limit":     {"limits": map[string]any{"image_bytes": 10}},
		"unknown timeout":   {"timeouts": map[string]any{"read_seconds": 10}},
		"limits not object": {"limits": 3},
		"huge timeout":      {"timeouts": map[string]any{"total_seconds": 1 << 40}},
	} {
		r.start(nil, more)
		cfg, _ := release.LoadUpdateConfig(r.root)
		if _, err := ParseSettings(cfg); !errors.Is(err, ErrSettings) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

const canary = "canary-7f3a9c1e5b2d4086"

// writeToken writes the token file secrets/<name> of the data directory.
func (r *rig) writeToken(name, content string, mode os.FileMode) {
	r.t.Helper()
	dir := filepath.Join(r.data, "secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		r.t.Fatal(err)
	}
}

// noCanary fails when the token is in the output or in err.
func (r *rig) noCanary(err error) {
	r.t.Helper()
	if strings.Contains(r.out.String(), canary) || err != nil && strings.Contains(err.Error(), canary) {
		r.t.Fatalf("the token was written: %v\n%s", err, r.out.String())
	}
}

func TestTokenReachesTheConfiguredHost(t *testing.T) {
	for _, overTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls %v", overTLS), func(t *testing.T) {
			r := newRig(t)
			r.publish(published{version: "0.2.0"})
			r.token, r.tls = canary, overTLS
			r.writeToken("forge.token", canary+"\n", 0o600)
			r.start(map[string]any{"token_file": "forge.token"})
			_, err := r.f.Fetch(Options{})
			if err != nil {
				t.Fatalf("fetch: %v\n%s", err, r.out.String())
			}
			reqs := r.stub.Requests()
			if len(reqs) < 6 {
				t.Fatalf("%d requests", len(reqs))
			}
			for _, req := range reqs {
				if req.Authorization != "token "+canary {
					t.Fatalf("%s carried %q", req.Path, req.Authorization)
				}
			}
			r.noCanary(err)
			staged := filepath.Join(r.root, "releases", "0.2.0")
			filepath.WalkDir(r.root, func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() && !strings.HasPrefix(path, staged) {
					if data, _ := os.ReadFile(path); bytes.Contains(data, []byte(canary)) {
						t.Errorf("%s holds the token", path)
					}
				}
				return nil
			})
		})
	}
}

func TestTokenInNoErrorText(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.token = "another-token"
	r.writeToken("forge.token", canary, 0o600)
	r.start(map[string]any{"token_file": "forge.token"})
	err := r.refused(ErrList)
	r.noCanary(err)

	r2 := newRig(t)
	r2.publish(published{version: "0.2.0"})
	r2.token = canary
	r2.writeToken("forge.token", canary, 0o600)
	r2.wrap = onFile(release.BundleFile, func(w http.ResponseWriter, req *http.Request, _ http.Handler) {
		http.Redirect(w, req, "http://forge.example.test/?token="+req.Header.Get("Authorization"), http.StatusFound)
	})
	r2.start(map[string]any{"token_file": "forge.token"})
	err = r2.refused(ErrRedirect)
	r2.noCanary(err)
}

// loopbackDialer maps every address to the stub.
func (r *rig) loopbackDialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	target := r.srv.Listener.Addr().String()
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, target)
	}
}

func TestNoTokenOverPlainHTTPToAName(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.writeToken("forge.token", canary, 0o600)
	r.start()
	r.f.dial = r.loopbackDialer()
	_, port, _ := net.SplitHostPort(r.srv.Listener.Addr().String())
	r.config(map[string]any{"trusted_key": r.trusted.Pub, "data_dir": r.data,
		"channel": map[string]any{"server": "http://forge.example.test:" + port, "owner": "acme", "token_file": "forge.token"}})
	err := r.refused(ErrToken)
	if !strings.Contains(err.Error(), "forge.example.test") || len(r.stub.Requests()) != 0 {
		t.Fatalf("%v after %d requests", err, len(r.stub.Requests()))
	}
	r.noCanary(err)
}

// TestTokenRuleOfTheTransport sends requests through a client whose server
// is plain http to a name, mapped to the stub by the dialer: the token goes
// with none of them. A request to another address gets no token either.
func TestTokenRuleOfTheTransport(t *testing.T) {
	r := newRig(t)
	r.start()
	_, port, _ := net.SplitHostPort(r.srv.Listener.Addr().String())
	set := &Settings{Owner: "acme", Limits: defaultLimits, Timeouts: defaultTimeouts}
	for _, server := range []string{"http://forge.example.test:" + port, r.srv.URL} {
		set.Server = mustURL(t, server)
		c := newClient(set, canary, r.loopbackDialer(), nil)
		other := "http://other.example.test:" + port + "/api/v1/packages/acme"
		for _, u := range []string{server + "/api/v1/packages/acme", other} {
			if _, _, err := c.fetch(u, 1<<20); err != nil {
				t.Fatal(err)
			}
		}
	}
	reqs := r.stub.Requests()
	if len(reqs) != 4 {
		t.Fatalf("%d requests", len(reqs))
	}
	for i, req := range reqs {
		want := ""
		if i == 2 {
			want = "token " + canary // loopback, the configured host
		}
		if req.Authorization != want {
			t.Errorf("request %d carried %q, want %q", i+1, req.Authorization, want)
		}
	}
}

func TestTokenFileRefusals(t *testing.T) {
	for _, c := range []struct {
		name, file, content string
		mode                os.FileMode
		says                string
	}{
		{"group can read", "forge.token", canary, 0o640, "group or others"},
		{"others can read", "forge.token", canary, 0o604, "group or others"},
		{"missing", "none.token", "", 0, "none.token"},
		{"empty", "forge.token", "\n", 0o600, "empty"},
		{"too large", "forge.token", strings.Repeat("a", 5000), 0o600, "larger"},
		{"a space", "forge.token", canary + " x", 0o600, "characters"},
		{"a separator", "../forge.token", canary, 0o600, "plain file name"},
		{"dot dot", "..", canary, 0o600, "plain file name"},
		{"dot", ".", canary, 0o600, "plain file name"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.publish(published{version: "0.2.0"})
			if c.mode != 0 && !strings.Contains(c.file, "/") && c.file != "." && c.file != ".." {
				r.writeToken(c.file, c.content, c.mode)
			}
			r.start(map[string]any{"token_file": c.file})
			err := r.refused(ErrToken)
			if !strings.Contains(err.Error(), c.says) || len(r.stub.Requests()) != 0 {
				t.Fatalf("%v after %d requests", err, len(r.stub.Requests()))
			}
			r.noCanary(err)
		})
	}
	t.Run("a link", func(t *testing.T) {
		r := newRig(t)
		r.publish(published{version: "0.2.0"})
		r.writeToken("real.token", canary, 0o600)
		os.Symlink("real.token", filepath.Join(r.data, "secrets", "forge.token"))
		r.start(map[string]any{"token_file": "forge.token"})
		if err := r.refused(ErrToken); !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("%v", err)
		}
	})
}

// A secrets folder that is a symlink is refused.
func TestTokenSecretsLink(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "forge.token"), []byte(canary), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(r.data, "secrets")); err != nil {
		t.Fatal(err)
	}
	r.start(map[string]any{"token_file": "forge.token"})
	err := r.refused(ErrToken)
	if !strings.Contains(err.Error(), filepath.Join(r.data, "secrets")) || !strings.Contains(err.Error(), "symlink") || len(r.stub.Requests()) != 0 {
		t.Fatalf("%v after %d requests", err, len(r.stub.Requests()))
	}
	r.noCanary(err)
}

func TestTokenNeedsADataDirectory(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.writeToken("forge.token", canary, 0o600)
	r.start(map[string]any{"token_file": "forge.token"}, map[string]any{"data_dir": ""})
	r.refused(ErrToken)
	r.f.DataDir = func(*release.UpdateConfig) (string, error) { return r.data, nil }
	r.fetched("0.2.0")
}

func mustURL(t *testing.T, text string) *url.URL {
	t.Helper()
	u, err := url.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// A releases/<version>/ that does not pass the release check is no staged
// version: the fetch stage refuses it, names it and leaves it as it is.
func TestStagedDirectoryIsChecked(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		r := newRig(t)
		r.publish(published{version: "0.2.0"})
		r.start()
		dir := filepath.Join(r.root, "releases", "0.2.0")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		err := r.refused(ErrStage)
		if !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "move it away") {
			t.Fatalf("message %q", err)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("the directory changed: %v %v", entries, err)
		}
	})
	t.Run("changed binary", func(t *testing.T) {
		r := newRig(t)
		r.publish(published{version: "0.2.0"})
		r.start()
		res := r.fetched("0.2.0")
		bin := filepath.Join(res.Dir, platformBinary())
		if err := os.WriteFile(bin, []byte("changed"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := r.refused(release.ErrDigest)
		if !errors.Is(err, ErrStage) || !strings.Contains(err.Error(), res.Dir) {
			t.Fatalf("message %q", err)
		}
		if got, _ := os.ReadFile(bin); string(got) != "changed" {
			t.Fatal("the binary changed")
		}
	})
}
