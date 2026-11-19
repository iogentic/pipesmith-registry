package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeService runs the release current points to: a restart reads the link,
// and the fake server answers with that version.
type fakeService struct {
	mu        sync.Mutex
	l         Layout
	running   string
	restarts  []string
	stops     []string
	failNext  int             // restarts that fail
	unhealthy map[string]bool // versions whose health answers 503
	late      int             // health answers 503 this many times after each restart
	pending   int
}

func (f *fakeService) Restart(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts = append(f.restarts, unit)
	if f.failNext > 0 {
		f.failNext--
		f.running = ""
		return errors.New("unit failed to start")
	}
	name, err := f.l.ReadLink(CurrentLink)
	if err != nil {
		return err
	}
	f.running, f.pending = name, f.late
	return nil
}

func (f *fakeService) Stop(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, unit)
	f.running = ""
	return nil
}

func (f *fakeService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	running, sick := f.running, f.unhealthy[f.running]
	if r.URL.Path == "/api/health" && f.pending > 0 {
		f.pending--
		sick = true
	}
	f.mu.Unlock()
	if running == "" {
		http.Error(w, "down", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/health":
		if sick {
			http.Error(w, `{"status":"starting"}`, http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	case "/api/status":
		fmt.Fprintf(w, `{"name":"Pipesmith Registry","version":%q}`, running)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeService) restartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.restarts)
}

// rig is an install root with release 0.1.0 running, a trusted key, a fake
// service manager and a fake server.
type rig struct {
	t    *testing.T
	l    Layout
	s    *signer
	svc  *fakeService
	srv  *httptest.Server
	out  bytes.Buffer
	u    *Updater
	data string
}

func newRig(t *testing.T, current string, others ...string) *rig {
	t.Helper()
	r := &rig{t: t, l: installRoot(t, append([]string{current}, others...)...), s: newSigner(t), data: t.TempDir()}
	r.config("")
	r.svc = &fakeService{l: r.l, running: current, unhealthy: map[string]bool{}}
	r.srv = httptest.NewServer(r.svc)
	t.Cleanup(r.srv.Close)
	r.u = &Updater{Root: r.l.Root, Service: r.svc, Out: &r.out,
		Address:       func(*UpdateConfig) (string, error) { return r.srv.URL, nil },
		HealthTimeout: 2 * time.Second, HealthInterval: 10 * time.Millisecond, RequestTimeout: time.Second}
	return r
}

// config writes update.json with the trusted key, the data directory of the
// rig and the members in extra, a text such as `, "unit": "x.service"`.
func (r *rig) config(extra string) {
	r.t.Helper()
	writeConfig(r.t, r.l.Root, fmt.Sprintf(`{"trusted_key": %q, "data_dir": %q%s}`, r.s.pub, r.data, extra))
}

// script is a release binary that answers version with answer and schema
// with the schema line of testSchema.
func script(answer string) []byte { return schemaScript(answer, `echo '`+testSchema+`'; exit 0`) }

const testSchema = `{"catalog":2,"state":1}`

// schemaScript answers version with answer and schema with schema.
func schemaScript(answer, schema string) []byte {
	return []byte("#!/bin/sh\nif [ \"$1\" = version ]; then\n" + answer + "\nfi\nif [ \"$1\" = schema ]; then\n" + schema +
		"\nfi\nexit 3\n")
}

func versionScript(v string) []byte { return script(`echo "pipesmith-registry ` + v + `"; exit 0`) }

// release writes a release directory with the binary bin, signed by s.
func (r *rig) release(bin []byte) string {
	r.t.Helper()
	dir := filepath.Join(r.t.TempDir(), "release")
	files := map[string][]byte{platformBinary(): bin, "sbom.spdx.json": []byte("{}"), PublicKeyFile: []byte("not the trusted key\n")}
	var sums string
	for _, name := range []string{platformBinary(), "sbom.spdx.json"} {
		sums += hexSum(files[name]) + "  " + name + "\n"
	}
	writeSigned(r.t, dir, r.s, files, sums)
	return dir
}

// tree lists every entry below root: kind, mode, size, link target and the
// SHA-256 of each file.
func tree(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		line := fmt.Sprintf("%s %v %d", rel, info.Mode(), info.Size())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			line += " -> " + target
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			line += " " + hex.EncodeToString(sum[:])
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func (r *rig) link(name string) string {
	r.t.Helper()
	target, err := os.Readlink(filepath.Join(r.l.Root, name))
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return target
}

// unchanged runs f and fails unless it returns an error wrapping want, the
// tree below the root is equal before and after, and nothing was restarted.
func (r *rig) unchanged(want error, f func() error) error {
	r.t.Helper()
	before := tree(r.t, r.l.Root)
	err := f()
	if !errors.Is(err, want) {
		r.t.Fatalf("error %v, want %v", err, want)
	}
	if after := tree(r.t, r.l.Root); after != before {
		r.t.Fatalf("the tree changed:\nbefore\n%s\nafter\n%s", before, after)
	}
	if n := r.svc.restartCount(); n != 0 {
		r.t.Fatalf("%d restarts after a refusal", n)
	}
	return err
}

// noLeftovers fails when a .partial or .staging entry is left in releases/.
func (r *rig) noLeftovers() {
	r.t.Helper()
	entries, err := os.ReadDir(r.l.Releases())
	if err != nil {
		r.t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".partial") || strings.HasPrefix(e.Name(), ".") {
			r.t.Fatalf("left behind: %s", e.Name())
		}
	}
	for _, link := range []string{".current.new", ".previous.new"} {
		if _, err := os.Lstat(filepath.Join(r.l.Root, link)); err == nil {
			r.t.Fatalf("left behind: %s", link)
		}
	}
}

func TestUpdatePasses(t *testing.T) {
	r := newRig(t, "0.1.0")
	bin := versionScript("0.2.0")
	dir := r.release(bin)
	if err := r.u.Update(dir, false); err != nil {
		t.Fatalf("update: %v\n%s", err, r.out.String())
	}
	if r.link(CurrentLink) != "releases/0.2.0" || r.link(PreviousLink) != "releases/0.1.0" {
		t.Fatalf("current %q previous %q", r.link(CurrentLink), r.link(PreviousLink))
	}
	if strings.Join(r.svc.restarts, ",") != DefaultUnit {
		t.Fatalf("restarts %v", r.svc.restarts)
	}
	installed := filepath.Join(r.l.Release("0.2.0"))
	entries, err := os.ReadDir(installed)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != SumsFile+","+BundleFile+","+InstalledBinary+","+platformBinary() {
		t.Fatalf("installed release holds %v", names)
	}
	info, err := os.Stat(filepath.Join(installed, InstalledBinary))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("installed binary: %v %v", info, err)
	}
	if got, _ := os.ReadFile(filepath.Join(installed, InstalledBinary)); !bytes.Equal(got, bin) {
		t.Fatal("installed binary differs from the release binary")
	}
	for _, name := range []string{SumsFile, BundleFile} {
		a, _ := os.ReadFile(filepath.Join(dir, name))
		b, _ := os.ReadFile(filepath.Join(installed, name))
		if !bytes.Equal(a, b) {
			t.Fatalf("%s copy differs", name)
		}
	}
	r.noLeftovers()
	lines := strings.Split(strings.TrimSpace(r.out.String()), "\n")
	for i, prefix := range []string{"owner: ", "current: releases/0.1.0", "checked: ", "version: 0.2.0, current runs 0.1.0",
		"schema: state.sqlite none, the release writes 1, catalog.sqlite none, the release writes 2", "installed: releases/0.2.0", "switched: current -> releases/0.2.0, previous -> releases/0.1.0",
		"restarted: " + DefaultUnit, "healthy: "} {
		if i >= len(lines) || !strings.HasPrefix(lines[i], prefix) {
			t.Fatalf("line %d of the output, want %q:\n%s", i+1, prefix, r.out.String())
		}
	}
	if len(lines) != 9 {
		t.Fatalf("%d lines of output:\n%s", len(lines), r.out.String())
	}
}

func TestUpdateUnitOfConfig(t *testing.T) {
	r := newRig(t, "0.1.0")
	r.config(`, "unit": "registry@b.service"`)
	if err := r.u.Update(r.release(versionScript("0.2.0")), false); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.svc.restarts, ",") != "registry@b.service" {
		t.Fatalf("restarts %v", r.svc.restarts)
	}
}

func TestUpdateSignatureRefusals(t *testing.T) {
	r := newRig(t, "0.1.0")
	other := newSigner(t)

	dir := r.release(versionScript("0.2.0"))
	sums, _ := os.ReadFile(filepath.Join(dir, SumsFile))
	if err := os.WriteFile(filepath.Join(dir, BundleFile), other.bundle(t, sums), 0o644); err != nil {
		t.Fatal(err)
	}
	r.unchanged(ErrSignature, func() error { return r.u.Update(dir, false) })

	dir = r.release(versionScript("0.2.0"))
	changed := append(append([]byte{}, sums...), (strings.Repeat("a", 64) + "  extra\n")...)
	if err := os.WriteFile(filepath.Join(dir, SumsFile), changed, 0o644); err != nil {
		t.Fatal(err)
	}
	r.unchanged(ErrSignature, func() error { return r.u.Update(dir, false) })

	dir = r.release(versionScript("0.2.0"))
	if err := os.Remove(filepath.Join(dir, BundleFile)); err != nil {
		t.Fatal(err)
	}
	r.unchanged(ErrNoBundle, func() error { return r.u.Update(dir, false) })

	// The cosign.pub of the release is never the trusted key.
	dir = r.release(versionScript("0.2.0"))
	pub, _ := os.ReadFile(other.pub)
	if err := os.WriteFile(filepath.Join(dir, PublicKeyFile), pub, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, BundleFile), other.bundle(t, sums), 0o644); err != nil {
		t.Fatal(err)
	}
	r.unchanged(ErrSignature, func() error { return r.u.Update(dir, false) })
}

func TestUpdateNoTrustedKey(t *testing.T) {
	r := newRig(t, "0.1.0")
	dir := r.release(versionScript("0.2.0"))
	for _, text := range []string{`{"data_dir": %q}`, `{"trusted_key": "", "data_dir": %q}`} {
		writeConfig(t, r.l.Root, fmt.Sprintf(text, r.data))
		r.unchanged(ErrNoTrustedKey, func() error { return r.u.Update(dir, false) })
	}
	if err := os.Remove(filepath.Join(r.l.Root, ConfigFile)); err != nil {
		t.Fatal(err)
	}
	err := r.unchanged(ErrConfig, func() error { return r.u.Update(dir, false) })
	if !strings.Contains(err.Error(), filepath.Join(r.l.Root, ConfigFile)) {
		t.Fatalf("missing update.json: %v", err)
	}
}

func TestUpdateChecksumRefusal(t *testing.T) {
	r := newRig(t, "0.1.0")
	dir := r.release(versionScript("0.2.0"))
	if err := os.WriteFile(filepath.Join(dir, platformBinary()), versionScript("0.2.1"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.unchanged(ErrDigest, func() error { return r.u.Update(dir, false) })
}

func TestUpdateVersionRefusals(t *testing.T) {
	t.Setenv("PSR_TEST_SECRET", "set")
	for name, bin := range map[string][]byte{
		"leading v":        versionScript("v0.2.0"),
		"no newline":       script(`printf "pipesmith-registry 0.2.0"; exit 0`),
		"two lines":        script(`echo "pipesmith-registry 0.2.0"; echo more; exit 0`),
		"other program":    script(`echo "artifact-repository 0.2.0"; exit 0`),
		"two numbers":      versionScript("0.2"),
		"suffix":           versionScript("0.2.0-rc1"),
		"answer on stderr": script(`echo "pipesmith-registry 0.2.0" >&2; exit 0`),
		"fails":            script(`echo "pipesmith-registry 0.2.0"; exit 1`),
		"no answer":        script(`exit 0`),
		"not executable":   []byte("\x7fELF not a program"),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "0.1.0")
			r.unchanged(ErrVersion, func() error { return r.u.Update(r.release(bin), false) })
		})
	}
	t.Run("slow", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		r.u.VersionTimeout = 200 * time.Millisecond
		start := time.Now()
		r.unchanged(ErrVersion, func() error { return r.u.Update(r.release(script(`sleep 5; echo "pipesmith-registry 0.2.0"`)), false) })
		if time.Since(start) > 3*time.Second {
			t.Fatalf("the version timeout took %s", time.Since(start))
		}
	})
	// An empty environment: the script above saw no PSR_TEST_SECRET.
	r := newRig(t, "0.1.0")
	if err := r.u.Update(r.release(script(`[ -n "$PSR_TEST_SECRET" ] && { echo leaked; exit 0; }; echo "pipesmith-registry 0.2.0"; exit 0`)), false); err != nil {
		t.Fatalf("a binary that sees an empty environment is refused: %v", err)
	}
}

func TestUpdateVersionRule(t *testing.T) {
	for name, c := range map[string]struct{ current, next string }{
		"same version":     {"0.2.0", "0.2.0"},
		"lower version":    {"0.3.0", "0.2.0"},
		"lower minor":      {"0.10.0", "0.9.9"},
		"current no name":  {"local-build", "0.2.0"},
		"current with a v": {"v0.1.0", "0.2.0"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, c.current)
			if c.current == c.next {
				if err := os.WriteFile(filepath.Join(r.l.Release(c.current), InstalledBinary), versionScript(c.next), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			r.unchanged(ErrVersion, func() error { return r.u.Update(r.release(versionScript(c.next)), false) })

			r.svc.unhealthy = map[string]bool{}
			if err := r.u.Update(r.release(versionScript(c.next)), true); err != nil {
				t.Fatalf("--allow-downgrade: %v\n%s", err, r.out.String())
			}
			if r.link(CurrentLink) != "releases/"+c.next {
				t.Fatalf("current %q", r.link(CurrentLink))
			}
		})
	}
	r := newRig(t, "0.9.9")
	if err := r.u.Update(r.release(versionScript("0.10.0")), false); err != nil {
		t.Fatalf("0.9.9 to 0.10.0: %v", err)
	}
}

// The file that runs is the file that was checked.
func TestUpdateSwapAfterCheck(t *testing.T) {
	r := newRig(t, "0.1.0")
	dir := r.release(versionScript("0.2.0"))
	swap := func(point string) func(string) error {
		return func(p string) error {
			if p == point {
				return os.WriteFile(filepath.Join(dir, platformBinary()), versionScript("0.9.0"), 0o755)
			}
			return nil
		}
	}
	// Swapped before the copy: the copy is not the checked file.
	r.u.hook = swap("copy")
	r.unchanged(ErrDigest, func() error { return r.u.Update(dir, false) })

	// Swapped after the copy: the copy runs and is installed.
	dir = r.release(versionScript("0.2.0"))
	r.u.hook = swap("copied")
	if err := r.u.Update(dir, false); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(r.l.Release("0.2.0"), InstalledBinary))
	if !bytes.Equal(got, versionScript("0.2.0")) || r.link(CurrentLink) != "releases/0.2.0" {
		t.Fatalf("installed %q, current %q", got, r.link(CurrentLink))
	}
}

func TestUpdateHealthLate(t *testing.T) {
	r := newRig(t, "0.1.0")
	r.svc.late = 20
	r.u.HealthTimeout = 5 * time.Second
	if err := r.u.Update(r.release(versionScript("0.2.0")), false); err != nil {
		t.Fatalf("a late answer inside the time: %v", err)
	}
	if r.link(CurrentLink) != "releases/0.2.0" || r.svc.restartCount() != 1 {
		t.Fatalf("current %q, %d restarts", r.link(CurrentLink), r.svc.restartCount())
	}
}

// wayBack checks the state after an update that went back by itself.
func (r *rig) wayBack(err error, want error, before, previous string) {
	r.t.Helper()
	if !errors.Is(err, want) {
		r.t.Fatalf("error %v, want %v", err, want)
	}
	if r.link(CurrentLink) != "releases/0.1.0" || r.link(PreviousLink) != previous {
		r.t.Fatalf("current %q previous %q", r.link(CurrentLink), r.link(PreviousLink))
	}
	if after := tree(r.t, r.l.Root); after != before {
		r.t.Fatalf("the way back left another tree:\nbefore\n%s\nafter\n%s", before, after)
	}
	r.noLeftovers()
}

func TestUpdateWayBackHealth(t *testing.T) {
	for _, previous := range []string{"", "releases/0.0.9"} {
		t.Run("previous "+previous, func(t *testing.T) {
			r := newRig(t, "0.1.0", "0.0.9")
			if previous != "" {
				if err := os.Symlink(previous, filepath.Join(r.l.Root, PreviousLink)); err != nil {
					t.Fatal(err)
				}
			}
			r.u.HealthTimeout = 300 * time.Millisecond
			r.svc.unhealthy["0.2.0"] = true
			before := tree(t, r.l.Root)
			err := r.u.Update(r.release(versionScript("0.2.0")), false)
			r.wayBack(err, ErrHealth, before, previous)
			if !strings.Contains(err.Error(), "which answers again") || !strings.Contains(err.Error(), "health check failed") {
				t.Fatalf("message %q", err)
			}
			if r.svc.restartCount() != 2 || r.svc.running != "0.1.0" {
				t.Fatalf("%d restarts, running %q", r.svc.restartCount(), r.svc.running)
			}
			if !strings.Contains(r.out.String(), "switched back: current -> releases/0.1.0") {
				t.Fatalf("output:\n%s", r.out.String())
			}
		})
	}
}

func TestUpdateWayBackRestart(t *testing.T) {
	r := newRig(t, "0.1.0")
	r.svc.failNext = 1
	before := tree(t, r.l.Root)
	err := r.u.Update(r.release(versionScript("0.2.0")), false)
	r.wayBack(err, ErrRestart, before, "")
	if !strings.Contains(err.Error(), "which answers again") || r.svc.restartCount() != 2 {
		t.Fatalf("message %q, %d restarts", err, r.svc.restartCount())
	}
}

func TestUpdateWayBackOldDown(t *testing.T) {
	r := newRig(t, "0.1.0")
	r.u.HealthTimeout = 200 * time.Millisecond
	r.svc.unhealthy["0.2.0"], r.svc.unhealthy["0.1.0"] = true, true
	before := tree(t, r.l.Root)
	err := r.u.Update(r.release(versionScript("0.2.0")), false)
	r.wayBack(err, ErrHealth, before, "")
	if !strings.Contains(err.Error(), "which does not answer either") {
		t.Fatalf("message %q", err)
	}
}

// A server that answers the new version under the old name does not pass.
func TestUpdateHealthWantsVersion(t *testing.T) {
	r := newRig(t, "0.1.0")
	r.u.HealthTimeout = 200 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/health":
			fmt.Fprint(w, `{"status":"ok"}`)
		case "/api/status":
			fmt.Fprint(w, `{"version":"0.1.0"}`)
		}
	}))
	defer srv.Close()
	r.u.Address = func(*UpdateConfig) (string, error) { return srv.URL, nil }
	err := r.u.Update(r.release(versionScript("0.2.0")), false)
	if !errors.Is(err, ErrHealth) || r.link(CurrentLink) != "releases/0.1.0" {
		t.Fatalf("error %v, current %q", err, r.link(CurrentLink))
	}
}

func TestProbe(t *testing.T) {
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	answer := func(health, status string, code int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/api/health" {
				w.WriteHeader(code)
				fmt.Fprint(w, health)
				return
			}
			fmt.Fprint(w, status)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	if err := probe(client, answer(`{"status":"ok"}`, `{"version":"0.2.0"}`, 200), "0.2.0"); err != nil {
		t.Fatalf("good answers: %v", err)
	}
	redirect := httptest.NewServer(http.RedirectHandler(answer(`{"status":"ok"}`, `{"version":"0.2.0"}`, 200)+"/api/health", http.StatusFound))
	defer redirect.Close()
	for name, base := range map[string]string{
		"health 503":      answer(`{"status":"ok"}`, `{"version":"0.2.0"}`, 503),
		"health not ok":   answer(`{"status":"down"}`, `{"version":"0.2.0"}`, 200),
		"health not JSON": answer(`ok`, `{"version":"0.2.0"}`, 200),
		"status array":    answer(`{"status":"ok"}`, `["0.2.0"]`, 200),
		"other version":   answer(`{"status":"ok"}`, `{"version":"0.1.0"}`, 200),
		"version number":  answer(`{"status":"ok"}`, `{"version":2}`, 200),
		"too large":       answer(`{"status":"ok", "x":"`+strings.Repeat("a", maxAnswer)+`"}`, `{"version":"0.2.0"}`, 200),
		"redirect":        redirect.URL,
	} {
		if err := probe(client, base, "0.2.0"); err == nil {
			t.Errorf("%s: passed", name)
		}
	}
}

func TestUpdateOwner(t *testing.T) {
	r := newRig(t, "0.1.0")
	dir := r.release(versionScript("0.2.0"))
	owner, err := fileOwner(r.l.Root)
	if err != nil {
		t.Fatal(err)
	}
	r.u.euid = func() int { return owner + 1 }
	err = r.unchanged(ErrOwner, func() error { return r.u.Update(dir, false) })
	if !strings.Contains(err.Error(), fmt.Sprintf("uid %d", owner)) {
		t.Fatalf("message %q", err)
	}
	r.unchanged(ErrOwner, func() error { return r.u.Rollback() })
	r.u.euid = nil
	r.u.ownerOf = func(string) (int, error) { return os.Geteuid() + 7, nil }
	r.unchanged(ErrOwner, func() error { return r.u.Update(dir, false) })
	r.u.ownerOf = nil
	if err := r.u.Update(dir, false); err != nil {
		t.Fatalf("as the owner: %v", err)
	}
}

func TestUpdateLock(t *testing.T) {
	r := newRig(t, "0.1.0")
	dir := r.release(versionScript("0.2.0"))
	unlock, err := lockRoot(r.l.Root)
	if err != nil {
		t.Fatal(err)
	}
	r.unchanged(ErrLocked, func() error { return r.u.Update(dir, false) })
	r.unchanged(ErrLocked, func() error { return r.u.Rollback() })
	unlock()
	if err := r.u.Update(dir, false); err != nil {
		t.Fatalf("after the lock: %v", err)
	}
	// The lock is free again after the command.
	again, err := lockRoot(r.l.Root)
	if err != nil {
		t.Fatalf("lock after update: %v", err)
	}
	again()
}

func TestUpdateCurrentRefusals(t *testing.T) {
	for name, mk := range map[string]func(root string) error{
		"no current": func(string) error { return nil },
		"a file": func(root string) error {
			return os.WriteFile(filepath.Join(root, CurrentLink), []byte("releases/0.1.0"), 0o644)
		},
		"absolute": func(root string) error {
			return os.Symlink(filepath.Join(root, ReleasesDir, "0.1.0"), filepath.Join(root, CurrentLink))
		},
		"to nothing": func(root string) error { return os.Symlink("releases/0.0.1", filepath.Join(root, CurrentLink)) },
		"a directory": func(root string) error {
			return os.Mkdir(filepath.Join(root, CurrentLink), 0o755)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "0.1.0")
			if err := os.Remove(filepath.Join(r.l.Root, CurrentLink)); err != nil {
				t.Fatal(err)
			}
			if err := mk(r.l.Root); err != nil {
				t.Fatal(err)
			}
			err := r.unchanged(ErrLayout, func() error { return r.u.Update(r.release(versionScript("0.2.0")), false) })
			if !strings.Contains(err.Error(), "the first release is installed by hand") {
				t.Fatalf("message %q", err)
			}
		})
	}
}

// Failures injected at the copy, the install, the swap and the restart leave
// current at the old release and nothing of this run behind.
func TestUpdateInjectedFailures(t *testing.T) {
	for _, point := range []string{"copy", "copied", "install", "swap-previous", "swap-current"} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s existing %v", point, existing), func(t *testing.T) {
				r := newRig(t, "0.1.0")
				if existing {
					if err := os.MkdirAll(r.l.Release("0.2.0"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				r.u.hook = func(p string) error {
					if p == point {
						return errors.New("injected")
					}
					return nil
				}
				r.unchanged(ErrInstall, func() error { return r.u.Update(r.release(versionScript("0.2.0")), false) })
				r.noLeftovers()
			})
		}
	}
	t.Run("restart", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		r.svc.failNext = 2
		before := tree(t, r.l.Root)
		err := r.u.Update(r.release(versionScript("0.2.0")), false)
		r.wayBack(err, ErrRestart, before, "")
		if !strings.Contains(err.Error(), "which does not answer either") {
			t.Fatalf("message %q", err)
		}
	})
}

// A leftover .<version>.installing of a run that stopped is replaced.
func TestUpdateLeftoverPartial(t *testing.T) {
	r := newRig(t, "0.1.0")
	partial := filepath.Join(r.l.Releases(), ".0.2.0.installing")
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, InstalledBinary), []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.u.Update(r.release(versionScript("0.2.0")), false); err != nil {
		t.Fatal(err)
	}
	r.noLeftovers()
}

func TestUpdateExistingRelease(t *testing.T) {
	bin := versionScript("0.2.0")
	t.Run("same binary", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		if err := os.MkdirAll(r.l.Release("0.2.0"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.l.Release("0.2.0"), InstalledBinary), bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := r.u.Update(r.release(bin), false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.out.String(), "kept: releases/0.2.0") {
			t.Fatalf("output:\n%s", r.out.String())
		}
	})
	t.Run("other binary", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		if err := os.MkdirAll(r.l.Release("0.2.0"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.l.Release("0.2.0"), InstalledBinary), versionScript("0.2.0 other"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := r.unchanged(ErrInstall, func() error { return r.u.Update(r.release(bin), false) })
		if !strings.Contains(err.Error(), "another SHA-256") {
			t.Fatalf("message %q", err)
		}
	})
	t.Run("link as binary", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		if err := os.MkdirAll(r.l.Release("0.2.0"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../0.1.0/"+InstalledBinary, filepath.Join(r.l.Release("0.2.0"), InstalledBinary)); err != nil {
			t.Fatal(err)
		}
		r.unchanged(ErrInstall, func() error { return r.u.Update(r.release(bin), false) })
	})
	t.Run("a file as release directory", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		if err := os.WriteFile(r.l.Release("0.2.0"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		r.unchanged(ErrInstall, func() error { return r.u.Update(r.release(bin), false) })
	})
	// The fetch stage writes the release files into releases/<version>/.
	t.Run("release directory in releases", func(t *testing.T) {
		r := newRig(t, "0.1.0")
		dir := r.release(bin)
		target := r.l.Release("0.2.0")
		if err := os.Rename(dir, target); err != nil {
			t.Fatal(err)
		}
		if err := r.u.Update(target, false); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(target, InstalledBinary))
		if !bytes.Equal(got, bin) || r.link(CurrentLink) != "releases/0.2.0" {
			t.Fatalf("installed %q, current %q", got, r.link(CurrentLink))
		}
		r.noLeftovers()
	})
}

// A directory the update makes holds the binary under its platform name
// too, so it passes Check as a directory of the fetch stage does.
func TestUpdateMakesACheckedDirectory(t *testing.T) {
	r := newRig(t, "0.1.0")
	bin := versionScript("0.2.0")
	if err := r.u.Update(r.release(bin), false); err != nil {
		t.Fatal(err)
	}
	rel, err := Check(r.l.Release("0.2.0"), Options{TrustedKey: r.s.pub})
	if err != nil {
		t.Fatalf("the installed directory: %v", err)
	}
	a, err := os.Stat(rel.Binary)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(r.l.Release("0.2.0"), InstalledBinary))
	if err != nil || !os.SameFile(a, b) {
		t.Fatalf("%s is no hard link of %s: %v", rel.Name, InstalledBinary, err)
	}
	if err := r.u.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := r.u.Switch("0.2.0", UpdateOptions{}); err != nil {
		t.Fatalf("switch to the directory of the update: %v", err)
	}
}

// Leftover .staging-* directories of a run that was killed are removed at
// the start of an update.
func TestUpdateRemovesLeftoverStaging(t *testing.T) {
	r := newRig(t, "0.1.0")
	junk := filepath.Join(r.l.Releases(), ".staging-123", InstalledBinary)
	if err := os.MkdirAll(filepath.Dir(junk), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(junk, []byte("stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(r.l.Releases(), "staging-not-a-leftover")
	if err := os.Mkdir(keep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.u.Update(r.release(versionScript("0.2.0")), false); err != nil {
		t.Fatal(err)
	}
	r.noLeftovers()
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("another directory was removed: %v", err)
	}
	if !strings.Contains(r.out.String(), "removed: releases/.staging-123") {
		t.Fatalf("output:\n%s", r.out.String())
	}
}
