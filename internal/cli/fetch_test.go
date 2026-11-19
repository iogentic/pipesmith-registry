package cli

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/release"
	"github.com/iogentic/pipesmith-registry/internal/release/releasetest"
	"github.com/iogentic/pipesmith-registry/tools/forgestub/forge"
)

// TestMain lets a copy of the test executable answer `version`, so it can
// stand as the binary of a release the fetch stage downloads.
func TestMain(m *testing.M) {
	releasetest.AnswerVersion()
	os.Exit(m.Run())
}

type fetchFixture struct {
	root, data, forge string
	signer            *releasetest.Signer
	stub              *forge.Stub
}

// newFetchFixture is an install root running 0.1.0 whose update.json names
// a stub of the forge on loopback, serving the versions given.
func newFetchFixture(t *testing.T, versions ...string) *fetchFixture {
	t.Helper()
	dataDir(t)
	base := t.TempDir()
	f := &fetchFixture{root: filepath.Join(base, "opt"), data: filepath.Join(base, "data"), forge: filepath.Join(base, "forge"),
		signer: releasetest.NewSigner(t)}
	for _, dir := range []string{filepath.Join(f.root, "releases", "0.1.0"), f.data, f.forge} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/0.1.0", filepath.Join(f.root, release.CurrentLink)); err != nil {
		t.Fatal(err)
	}
	name := release.BinaryName(runtime.GOOS, runtime.GOARCH)
	for _, v := range versions {
		bin := releasetest.Answering(t, v)
		files := map[string][]byte{name: bin, "sbom.cdx.json": releasetest.BillJSON(t, releasetest.BillOf(t, bin)),
			release.PublicKeyFile: []byte("not the trusted key\n")}
		releasetest.WriteSigned(t, filepath.Join(f.forge, v), f.signer, files, releasetest.SumsOf(files, "sbom.cdx.json", name))
	}
	f.stub = forge.New(forge.Options{Dir: f.forge, Owner: "acme"})
	srv := httptest.NewServer(f.stub)
	t.Cleanup(srv.Close)
	cfg, _ := json.Marshal(map[string]any{"trusted_key": f.signer.Pub, "data_dir": f.data,
		"channel": map[string]any{"server": srv.URL, "owner": "acme"}})
	if err := os.WriteFile(filepath.Join(f.root, release.ConfigFile), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestUpdateFetchCommand(t *testing.T) {
	f := newFetchFixture(t, "0.2.0")
	code, out, errOut := run(t, "update", "--fetch", "--root", f.root)
	if code != 0 || errOut != "" || !strings.HasSuffix(out, "staged: releases/0.2.0\n") {
		t.Fatalf("fetch: %d %q\n%s", code, errOut, out)
	}
	if _, err := release.Check(filepath.Join(f.root, "releases", "0.2.0"), release.Options{TrustedKey: f.signer.Pub}); err != nil {
		t.Fatalf("staged release: %v", err)
	}
	if current, _ := os.Readlink(filepath.Join(f.root, release.CurrentLink)); current != "releases/0.1.0" {
		t.Fatalf("the fetch stage switched current to %s", current)
	}
	code, out, _ = run(t, "update", "--fetch", "--root", f.root)
	if code != 0 || !strings.HasSuffix(out, "staged: releases/0.2.0 exists, nothing fetched\n") {
		t.Fatalf("second fetch: %d\n%s", code, out)
	}
}

func TestUpdateFetchNothingToDo(t *testing.T) {
	f := newFetchFixture(t, "0.1.0", "0.0.9")
	code, out, errOut := run(t, "update", "--fetch", "--root", f.root)
	if code != 0 || errOut != "" || !strings.Contains(out, "nothing to do: no complete version above 0.1.0") {
		t.Fatalf("%d %q\n%s", code, errOut, out)
	}
}

func TestUpdateFetchRefusals(t *testing.T) {
	f := newFetchFixture(t, "0.1.0", "0.0.9")
	code, _, errOut := run(t, "update", "--fetch", "--version", "0.0.9", "--root", f.root)
	if code != 1 || !strings.HasPrefix(errOut, "error: version: v0.0.9 is not higher than 0.1.0") {
		t.Fatalf("lower version: %d %q", code, errOut)
	}
	code, out, errOut := run(t, "update", "--fetch", "--version", "v0.0.9", "--allow-downgrade", "--root", f.root)
	if code != 0 || !strings.HasSuffix(out, "staged: releases/0.0.9\n") {
		t.Fatalf("--allow-downgrade: %d %q\n%s", code, errOut, out)
	}
	if err := os.WriteFile(filepath.Join(f.root, release.ConfigFile), []byte(`{"trusted_key": "`+f.signer.Pub+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	requests := len(f.stub.Requests())
	code, _, errOut = run(t, "update", "--fetch", "--root", f.root)
	if code != 1 || !strings.Contains(errOut, "has no key channel") || len(f.stub.Requests()) != requests {
		t.Fatalf("no channel: %d %q", code, errOut)
	}
}

// A directory made by update <release directory> counts as staged, and the
// switch to it passes; an empty releases/<version>/ is refused with exit 1.
func TestUpdateDirectoryThenFetchAndSwitch(t *testing.T) {
	f, svc := newScheduledFixture(t, "", "0.2.0")
	if err := os.WriteFile(filepath.Join(f.root, "releases", "0.1.0", release.InstalledBinary), releasetest.Answering(t, "0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	name := release.BinaryName(runtime.GOOS, runtime.GOARCH)
	bin := releasetest.Answering(t, "0.2.0")
	dir := filepath.Join(t.TempDir(), "release")
	releasetest.WriteSigned(t, dir, f.signer, map[string][]byte{name: bin}, releasetest.SumsOf(map[string][]byte{name: bin}, name))
	for _, args := range [][]string{{"update", "--root", f.root, dir}, {"rollback", "--root", f.root}} {
		if code, out, errOut := run(t, args...); code != 0 {
			t.Fatalf("%v: %d %q\n%s", args, code, errOut, out)
		}
	}
	code, out, errOut := run(t, "update", "--fetch", "--root", f.root)
	if code != 0 || !strings.HasSuffix(out, "staged: releases/0.2.0 exists, nothing fetched\n") {
		t.Fatalf("fetch: %d %q\n%s", code, errOut, out)
	}
	if code, out, errOut = run(t, "update", "--switch", "0.2.0", "--root", f.root); code != 0 || svc.restarts != 3 {
		t.Fatalf("switch: %d %q, %d restarts\n%s", code, errOut, svc.restarts, out)
	}

	g := newFetchFixture(t, "0.2.0")
	empty := filepath.Join(g.root, "releases", "0.2.0")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = run(t, "update", "--fetch", "--root", g.root)
	if code != 1 || !strings.Contains(errOut, empty) {
		t.Fatalf("empty directory: %d %q", code, errOut)
	}
	if _, err := os.Stat(empty); err != nil {
		t.Fatal(err)
	}
}
