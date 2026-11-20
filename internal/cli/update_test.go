package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/release"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The help texts argparse of Python 3.12.13 prints at 80 columns for the
// commands of the update.
var updateHelp = map[string]string{
	"update": "usage: pipesmith-registry update [-h] [--root ROOT] [--allow-downgrade]\n" +
		"                                 [--fetch] [--version VERSION]\n" +
		"                                 [--switch VERSION] [--no-catalog-backup]\n" +
		"                                 [--scheduled]\n" +
		"                                 [directory]\n\npositional arguments:\n  directory\n\n" +
		"options:\n  -h, --help           show this help message and exit\n  --root ROOT\n  --allow-downgrade\n" +
		"  --fetch\n  --version VERSION\n  --switch VERSION\n  --no-catalog-backup\n  --scheduled\n",
	"schema": "usage: pipesmith-registry schema [-h]\n\noptions:\n  -h, --help  show this help message and exit\n",
	"rollback": "usage: pipesmith-registry rollback [-h] [--root ROOT]\n\n" +
		"options:\n  -h, --help   show this help message and exit\n  --root ROOT\n",
}

func TestUpdateCommandHelp(t *testing.T) {
	dir := dataDir(t)
	for command, want := range updateHelp {
		code, out, errOut := run(t, command, "--help")
		if code != 0 || out != want || errOut != "" {
			t.Errorf("%s --help: %d\n%s\nwant\n%s", command, code, out, want)
		}
	}
	_, out, _ := run(t, "help")
	for _, line := range []string{"Updates\n", "  update [--root DIR] [--allow-downgrade] <release directory>\n",
		"  update --fetch [--version V [--allow-downgrade]] [--root DIR]\n", "  update --fetch --scheduled  ", "  rollback [--root DIR]  ",
		"  update --switch <version> [--allow-downgrade] [--root DIR]\n", "  schema                               print the schema"} {
		if !strings.Contains(out, line) {
			t.Errorf("help lacks %q", line)
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("help created the data directory")
	}
}

func TestUpdateCommandUsage(t *testing.T) {
	dataDir(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"update"}, "error: pipesmith-registry update: give a release directory, --fetch or --switch <version>\n"},
		{[]string{"update", "--fetch", "dir"}, "error: pipesmith-registry update: give one of a release directory, --fetch and --switch <version>\n"},
		{[]string{"update", "--switch", "0.2.0", "dir"}, "error: pipesmith-registry update: give one of a release directory, --fetch and --switch <version>\n"},
		{[]string{"update", "--switch", "0.2.0", "--fetch"}, "error: pipesmith-registry update: give one of a release directory, --fetch and --switch <version>\n"},
		{[]string{"update", "--version", "0.2.0", "dir"}, "error: pipesmith-registry update: --version needs --fetch\n"},
		{[]string{"update", "--version", "0.2.0"}, "error: pipesmith-registry update: --version needs --fetch\n"},
		{[]string{"update", "--version", "0.2.0", "--switch", "0.2.0"}, "error: pipesmith-registry update: --version needs --fetch\n"},
		{[]string{"update", "--fetch", "--no-catalog-backup"}, "error: pipesmith-registry update: --no-catalog-backup needs a release directory or --switch\n"},
		{[]string{"update", "--no-catalog-backup"}, "error: pipesmith-registry update: give a release directory, --fetch or --switch <version>\n"},
		{[]string{"update", "--switch"}, "error: pipesmith-registry update: argument --switch: expected one argument\n"},
		{[]string{"schema", "extra"}, "error: pipesmith-registry: unrecognized arguments: extra\n"},
		{[]string{"update", "--fetch", "--version"}, "error: pipesmith-registry update: argument --version: expected one argument\n"},
	} {
		code, out, errOut := run(t, c.args...)
		if code != 2 || out != "" || !strings.HasPrefix(errOut, c.want) {
			t.Errorf("%v: %d %q %q", c.args, code, out, errOut)
		}
	}
	code, _, errOut := run(t, "rollback", "extra")
	if code != 2 || !strings.HasPrefix(errOut, "error: pipesmith-registry: unrecognized arguments: extra\n") {
		t.Errorf("rollback extra: %d %q", code, errOut)
	}
}

// cliService is the service manager of the tests: a restart runs the release
// current points to, and the server answers its version.
type cliService struct {
	mu       sync.Mutex
	root     string
	running  string
	restarts int
	sick     map[string]bool
	during   func() // runs at each restart
}

func (s *cliService) Restart(_ context.Context, unit string) error {
	if s.during != nil {
		s.during()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if unit != release.DefaultUnit {
		return fmt.Errorf("unit %q", unit)
	}
	target, err := os.Readlink(filepath.Join(s.root, release.CurrentLink))
	if err != nil {
		return err
	}
	s.running, s.restarts = strings.TrimPrefix(target, "releases/"), s.restarts+1
	return nil
}

func (s *cliService) Stop(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = ""
	return nil
}

// Active reports a unit that runs a release.
func (s *cliService) Active(context.Context, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running != "", nil
}

func (s *cliService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	running, sick := s.running, s.sick[s.running]
	s.mu.Unlock()
	switch {
	case r.URL.Path == "/api/health" && !sick:
		fmt.Fprint(w, `{"status":"ok"}`)
	case r.URL.Path == "/api/status":
		fmt.Fprintf(w, `{"name":"Pipesmith Registry","version":%q}`, running)
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

type updateFixture struct {
	root, data string
	svc        *cliService
	priv       *ecdsa.PrivateKey
}

// newUpdateFixture is an install root running 0.1.0, a data directory whose
// config.json names the port of a fake server on loopback, and the fake
// service manager in updateService.
func newUpdateFixture(t *testing.T) *updateFixture {
	t.Helper()
	dataDir(t)
	base := t.TempDir()
	f := &updateFixture{root: filepath.Join(base, "opt"), data: filepath.Join(base, "data")}
	if err := os.MkdirAll(filepath.Join(f.root, "releases", "0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := []byte("#!/bin/sh\n[ \"$1\" = version ] && { echo \"pipesmith-registry 0.1.0\"; exit 0; }\n" +
		"[ \"$1\" = schema ] && { echo '{\"catalog\":2,\"state\":1}'; exit 0; }\nexit 3\n")
	if err := os.WriteFile(filepath.Join(f.root, "releases", "0.1.0", release.InstalledBinary), old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/0.1.0", filepath.Join(f.root, release.CurrentLink)); err != nil {
		t.Fatal(err)
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.priv = priv
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(base, "trusted.pub")
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	f.svc = &cliService{root: f.root, running: "0.1.0", sick: map[string]bool{}}
	srv := httptest.NewServer(f.svc)
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.data, "config.json"), []byte(`{"bind": "127.0.0.1", "port": `+port+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]string{"trusted_key": key, "data_dir": f.data})
	if err := os.WriteFile(filepath.Join(f.root, release.ConfigFile), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	service, timeout := updateService, updateHealthTimeout
	updateService, updateHealthTimeout = f.svc, 2*time.Second
	t.Cleanup(func() { updateService, updateHealthTimeout = service, timeout })
	return f
}

// release writes a release directory whose binary answers version v.
func (f *updateFixture) release(t *testing.T, v string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "release-"+v)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := release.BinaryName(runtime.GOOS, runtime.GOARCH)
	bin := []byte("#!/bin/sh\n[ \"$1\" = version ] && { echo \"pipesmith-registry " + v + "\"; exit 0; }\n" +
		"[ \"$1\" = schema ] && { echo '{\"catalog\":2,\"state\":1}'; exit 0; }\nexit 3\n")
	if err := os.WriteFile(filepath.Join(dir, name), bin, 0o755); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(bin)
	sums := []byte(hex.EncodeToString(digest[:]) + "  " + name + "\n")
	sum := sha256.Sum256(sums)
	sig, err := ecdsa.SignASN1(rand.Reader, f.priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	bundle, _ := json.Marshal(map[string]any{
		"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json",
		"messageSignature": map[string]any{
			"messageDigest": map[string]any{"algorithm": "SHA2_256", "digest": sum[:]},
			"signature":     sig,
		},
	})
	if err := os.WriteFile(filepath.Join(dir, release.SumsFile), sums, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, release.BundleFile), bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (f *updateFixture) links(t *testing.T) (string, string) {
	t.Helper()
	current, _ := os.Readlink(filepath.Join(f.root, release.CurrentLink))
	previous, _ := os.Readlink(filepath.Join(f.root, release.PreviousLink))
	return current, previous
}

func TestUpdateAndRollbackCommands(t *testing.T) {
	f := newUpdateFixture(t)
	code, out, errOut := run(t, "update", "--root", f.root, f.release(t, "0.2.0"))
	if code != 0 || errOut != "" {
		t.Fatalf("update: %d %q %q", code, out, errOut)
	}
	if current, previous := f.links(t); current != "releases/0.2.0" || previous != "releases/0.1.0" {
		t.Fatalf("after update: current %q previous %q", current, previous)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 9 || !strings.HasPrefix(lines[8], "healthy: http://127.0.0.1:") {
		t.Fatalf("update output:\n%s", out)
	}

	code, out, errOut = run(t, "rollback", "--root", f.root)
	if code != 0 || errOut != "" {
		t.Fatalf("rollback: %d %q %q", code, out, errOut)
	}
	if current, previous := f.links(t); current != "releases/0.1.0" || previous != "releases/0.2.0" {
		t.Fatalf("after rollback: current %q previous %q", current, previous)
	}
	if !strings.Contains(out, "switched: current -> releases/0.1.0, previous -> releases/0.2.0\n") || f.svc.restarts != 2 {
		t.Fatalf("rollback output, %d restarts:\n%s", f.svc.restarts, out)
	}

	code, _, errOut = run(t, "update", "--root", f.root, f.release(t, "0.1.0"))
	if code != 1 || !strings.HasPrefix(errOut, "error: version: 0.1.0 is the version current runs") {
		t.Fatalf("same version: %d %q", code, errOut)
	}
	code, _, errOut = run(t, "update", "--root", f.root, "--allow-downgrade", f.release(t, "0.0.9"))
	if code != 0 {
		t.Fatalf("--allow-downgrade: %d %q", code, errOut)
	}
	if current, _ := f.links(t); current != "releases/0.0.9" {
		t.Fatalf("after the downgrade: current %q", current)
	}
}

func TestUpdateCommandRefusals(t *testing.T) {
	f := newUpdateFixture(t)
	good := f.release(t, "0.2.0")

	other := newUpdateFixture(t)
	foreign := other.release(t, "0.2.0")
	code, _, errOut := run(t, "update", "--root", f.root, foreign)
	if code != 1 || errOut != "error: "+release.ErrSignature.Error()+"\n" {
		t.Fatalf("bundle of another key: %d %q", code, errOut)
	}

	code, _, errOut = run(t, "update", "--root", filepath.Join(t.TempDir(), "none"), good)
	if code != 1 || !strings.HasPrefix(errOut, "error: not the owner of the install root: ") {
		t.Fatalf("no root: %d %q", code, errOut)
	}

	empty := t.TempDir()
	code, _, errOut = run(t, "update", "--root", empty, good)
	if code != 1 || !strings.Contains(errOut, filepath.Join(empty, release.ConfigFile)) {
		t.Fatalf("no update.json: %d %q", code, errOut)
	}

	code, _, errOut = run(t, "rollback", "--root", f.root)
	if code != 1 || !strings.Contains(errOut, "there is no release to go back to") {
		t.Fatalf("rollback without previous: %d %q", code, errOut)
	}

	f.svc.sick["0.2.0"] = true
	updateHealthTimeout = 300 * time.Millisecond
	code, out, errOut := run(t, "update", "--root", f.root, good)
	if code != 1 || !strings.Contains(errOut, "health check failed") || !strings.Contains(errOut, "went back to releases/0.1.0, which answers again") {
		t.Fatalf("failed health: %d %q", code, errOut)
	}
	if !strings.Contains(out, "switched back: current -> releases/0.1.0\n") {
		t.Fatalf("output:\n%s", out)
	}
	if current, previous := f.links(t); current != "releases/0.1.0" || previous != "" {
		t.Fatalf("after the way back: current %q previous %q", current, previous)
	}
}

func TestHealthAddress(t *testing.T) {
	dataDir(t)
	for bind, want := range map[string]string{"127.0.0.1": "http://127.0.0.1:14771", "localhost": "http://127.0.0.1:14771",
		"::1": "http://[::1]:14771"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"bind": "`+bind+`", "port": 14771}`), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := healthAddress(&release.UpdateConfig{DataDir: dir})
		if err != nil || got != want {
			t.Errorf("bind %s: %q %v, want %q", bind, got, err, want)
		}
	}
}

// The health wait speaks TLS with tls_cert of config.json when tls_key is
// set too, and health_tls_cert of update.json wins.
func TestHealthTargetTLS(t *testing.T) {
	dataDir(t)
	for name, c := range map[string]struct{ config, update, base, cert string }{
		"cert and key": {`"tls_cert": "/etc/psr/server.pem", "tls_key": "/etc/psr/server.key"`, `{}`,
			"https://127.0.0.1:14771", "/etc/psr/server.pem"},
		"cert alone": {`"tls_cert": "/etc/psr/server.pem"`, `{}`, "http://127.0.0.1:14771", ""},
		"update.json wins": {`"tls_cert": "/etc/psr/server.pem", "tls_key": "/etc/psr/server.key"`,
			`{"health_tls_cert": "/etc/unit/cert.pem"}`, "https://127.0.0.1:14771", "/etc/unit/cert.pem"},
		"relative": {``, `{"health_tls_cert": "tls/cert.pem"}`, "https://127.0.0.1:14771", "ROOT/tls/cert.pem"},
	} {
		root, data := t.TempDir(), t.TempDir()
		text := `{"bind": "127.0.0.1", "port": 14771`
		if c.config != "" {
			text += ", " + c.config
		}
		if err := os.WriteFile(filepath.Join(data, "config.json"), []byte(text+"}"), 0o644); err != nil {
			t.Fatal(err)
		}
		update := strings.Replace(c.update, "{", fmt.Sprintf(`{"data_dir": %q, `, data), 1)
		update = strings.Replace(update, ", }", "}", 1)
		if err := os.WriteFile(filepath.Join(root, release.ConfigFile), []byte(update), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := release.LoadUpdateConfig(root)
		if err != nil {
			t.Fatal(err)
		}
		u := &release.Updater{Root: root, Address: healthAddress, HealthCert: healthCert}
		base, cert, _, err := u.HealthTarget(cfg)
		want := strings.Replace(c.cert, "ROOT", root, 1)
		if err != nil || base != c.base || cert != want {
			t.Errorf("%s: %q %q %v, want %q %q", name, base, cert, err, c.base, want)
		}
	}
}

func TestSchemaCommand(t *testing.T) {
	dir := dataDir(t)
	code, out, errOut := run(t, "schema")
	want := fmt.Sprintf("{\"catalog\":%d,\"state\":%d}\n", catalog.SchemaVersion, state.SchemaVersion)
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("schema: %d %q %q, want %q", code, out, errOut, want)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("schema created the data directory")
	}
}

// The fetch stage stages 0.2.0, the switch stage installs it from there.
func TestUpdateSwitchCommand(t *testing.T) {
	f := newFetchFixture(t, "0.2.0")
	svc := &cliService{root: f.root, running: "0.1.0", sick: map[string]bool{}}
	srv := httptest.NewServer(svc)
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.data, "config.json"), []byte(`{"bind": "127.0.0.1", "port": `+port+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	service, timeout := updateService, updateHealthTimeout
	updateService, updateHealthTimeout = svc, 2*time.Second
	t.Cleanup(func() { updateService, updateHealthTimeout = service, timeout })

	code, out, errOut := run(t, "update", "--switch", "0.2.0", "--root", f.root)
	if code != 1 || !strings.Contains(errOut, "releases/0.2.0") || svc.restarts != 0 {
		t.Fatalf("switch before the fetch: %d %q\n%s", code, errOut, out)
	}
	if code, out, errOut = run(t, "update", "--fetch", "--root", f.root); code != 0 {
		t.Fatalf("fetch: %d %q\n%s", code, errOut, out)
	}
	code, out, errOut = run(t, "update", "--switch", "0.2.0", "--root", f.root)
	if code != 0 || errOut != "" {
		t.Fatalf("switch: %d %q\n%s", code, errOut, out)
	}
	current, _ := os.Readlink(filepath.Join(f.root, release.CurrentLink))
	previous, _ := os.Readlink(filepath.Join(f.root, release.PreviousLink))
	if current != "releases/0.2.0" || previous != "releases/0.1.0" || svc.restarts != 1 {
		t.Fatalf("current %q previous %q, %d restarts", current, previous, svc.restarts)
	}
	if !strings.Contains(out, "schema: state.sqlite none, the release writes 1, catalog.sqlite none, the release writes 2\n") {
		t.Fatalf("output:\n%s", out)
	}
}

// dropDataDir writes update.json of root again without data_dir.
func dropDataDir(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, release.ConfigFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	delete(cfg, "data_dir")
	data, _ = json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The forms that switch take the data directory from update.json only, not
// from the environment of the caller.
func TestUpdateNeedsDataDir(t *testing.T) {
	f := newUpdateFixture(t)
	dropDataDir(t, f.root)
	for _, args := range [][]string{{"update", "--root", f.root, f.release(t, "0.2.0")}, {"rollback", "--root", f.root}} {
		code, _, errOut := run(t, args...)
		if code != 1 || !strings.Contains(errOut, "data_dir") || f.svc.restarts != 0 {
			t.Fatalf("%v: %d %q, %d restarts", args, code, errOut, f.svc.restarts)
		}
	}

	s, svc := newScheduledFixture(t, "true", "0.2.0")
	dropDataDir(t, s.root)
	code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", s.root)
	if code != 1 || !strings.Contains(errOut, "data_dir") || s.staged(t, "0.2.0") || svc.restarts != 0 || len(s.stub.Requests()) != 0 {
		t.Fatalf("scheduled: %d %q, staged %v, %d restarts\n%s", code, errOut, s.staged(t, "0.2.0"), svc.restarts, out)
	}
	// The fetch stage without a switch needs no data directory without a token.
	code, out, errOut = run(t, "update", "--fetch", "--root", s.root)
	if code != 0 || !s.staged(t, "0.2.0") || svc.restarts != 0 {
		t.Fatalf("fetch: %d %q\n%s", code, errOut, out)
	}
}
