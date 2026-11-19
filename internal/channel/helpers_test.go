package channel

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	// The test executable is the release binary of these tests; this import
	// puts one dependency module into its build information.
	_ "github.com/google/uuid"

	"github.com/iogentic/pipesmith-registry/internal/release"
	"github.com/iogentic/pipesmith-registry/internal/release/releasetest"
	"github.com/iogentic/pipesmith-registry/tools/forgestub/forge"
)

// TestMain lets a copy of the test executable answer `version`.
func TestMain(m *testing.M) {
	releasetest.AnswerVersion()
	os.Exit(m.Run())
}

type component = releasetest.Component

func testExecutable(t *testing.T) []byte { return releasetest.Executable(t) }

func binaryAnswering(t *testing.T, v string) []byte { return releasetest.Answering(t, v) }

func billOf(t *testing.T, bin []byte) []component { return releasetest.BillOf(t, bin) }

func billJSON(t *testing.T, comps []component) []byte { return releasetest.BillJSON(t, comps) }

// published is a release folder of the stub as a test writes it.
type published struct {
	version string
	answer  string              // the version the binary names; empty is version
	noPub   bool                // no cosign.pub: an incomplete version
	signer  *releasetest.Signer // nil is the trusted signer
	bin     []byte              // nil is binaryAnswering(answer)
	bill    []component         // nil is billOf(bin)
	edit    func(files map[string][]byte)
	// sumsEdit changes SHA256SUMS before it is signed.
	sumsEdit func(sums string) string
}

// rig is an install root running 0.1.0 with the trusted key and a channel
// to a stub on loopback.
type rig struct {
	t       *testing.T
	root    string
	data    string
	stubDir string
	trusted *releasetest.Signer
	stub    *forge.Stub
	extra   []forge.Entry
	wrap    func(http.Handler) http.Handler
	prefix  string // path of the forge address after the host
	token   string // the token the stub requires; empty requires none
	tls     bool   // serve the stub over TLS
	srv     *httptest.Server
	out     bytes.Buffer
	f       *Fetcher
}

func newOther(t *testing.T) *releasetest.Signer { return releasetest.NewSigner(t) }

func platformBinary() string { return release.BinaryName(runtime.GOOS, runtime.GOARCH) }

func newRig(t *testing.T) *rig {
	t.Helper()
	base := t.TempDir()
	r := &rig{t: t, root: filepath.Join(base, "opt"), data: filepath.Join(base, "data"), stubDir: filepath.Join(base, "forge"),
		trusted: releasetest.NewSigner(t)}
	for _, dir := range []string{filepath.Join(r.root, release.ReleasesDir, "0.1.0"), r.data, r.stubDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(r.root, release.ReleasesDir, "0.1.0", release.InstalledBinary), []byte("old\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release.ReleasesDir+"/0.1.0", filepath.Join(r.root, release.CurrentLink)); err != nil {
		t.Fatal(err)
	}
	r.f = &Fetcher{Root: r.root, Out: &r.out}
	return r
}

// start serves the stub and writes update.json with channel to it; extra
// keys of channel and of the file come from more.
func (r *rig) start(more ...map[string]any) {
	r.t.Helper()
	r.stub = forge.New(forge.Options{Dir: r.stubDir, Owner: "acme", Extra: r.extra, Token: r.token})
	var h http.Handler = r.stub
	if r.wrap != nil {
		h = r.wrap(h)
	}
	if r.tls {
		r.srv = httptest.NewTLSServer(h)
		r.f.tls = &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}
		r.f.tls.RootCAs.AddCert(r.srv.Certificate())
	} else {
		r.srv = httptest.NewServer(h)
	}
	r.t.Cleanup(r.srv.Close)
	channel := map[string]any{"server": r.srv.URL + r.prefix, "owner": "acme"}
	cfg := map[string]any{"trusted_key": r.trusted.Pub, "data_dir": r.data, "channel": channel}
	if len(more) > 0 {
		for k, v := range more[0] {
			channel[k] = v
		}
	}
	if len(more) > 1 {
		for k, v := range more[1] {
			cfg[k] = v
		}
	}
	r.config(cfg)
}

func (r *rig) config(cfg map[string]any) {
	r.t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.root, release.ConfigFile), data, 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// publish writes a release folder <stubDir>/<version>/ and returns its files.
func (r *rig) publish(p published) map[string][]byte {
	r.t.Helper()
	answer := p.answer
	if answer == "" {
		answer = p.version
	}
	bin := p.bin
	if bin == nil {
		bin = binaryAnswering(r.t, answer)
	}
	bill := p.bill
	if bill == nil {
		bill = billOf(r.t, bin)
	}
	signer := p.signer
	if signer == nil {
		signer = r.trusted
	}
	other := releasetest.NewSigner(r.t)
	files := map[string][]byte{platformBinary(): bin, BillFile: billJSON(r.t, bill), "sbom.spdx.json": []byte("{}\n")}
	if !p.noPub {
		files[release.PublicKeyFile] = other.PEM
	}
	if p.edit != nil {
		p.edit(files)
	}
	sums := releasetest.SumsOf(files, "sbom.spdx.json", BillFile, platformBinary())
	if p.sumsEdit != nil {
		sums = p.sumsEdit(sums)
	}
	releasetest.WriteSigned(r.t, filepath.Join(r.stubDir, p.version), signer, files, sums)
	files[release.SumsFile] = []byte(sums)
	return files
}

// folder makes an empty version folder: a listed version without files.
func (r *rig) folder(version string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Join(r.stubDir, version), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

// requested lists the paths of the files the stub was asked for.
func (r *rig) requested() []string {
	var out []string
	for _, req := range r.stub.Requests() {
		if strings.HasPrefix(req.Path, "/api/packages/") {
			out = append(out, strings.TrimPrefix(req.Path, "/api/packages/acme/generic/"+Package+"/"))
		}
	}
	return out
}

// releases lists the entries of releases/ of the root.
func (r *rig) releases() []string {
	r.t.Helper()
	entries, err := os.ReadDir(filepath.Join(r.root, release.ReleasesDir))
	if err != nil {
		r.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// refused runs the fetch stage, wants an error wrapping want, and no entry
// below releases/ other than those before.
func (r *rig) refused(want error) error {
	r.t.Helper()
	before := strings.Join(r.releases(), ",")
	res, err := r.f.Fetch(Options{})
	if err == nil {
		r.t.Fatalf("fetched %+v, want %v\n%s", res, want, r.out.String())
	}
	if !errors.Is(err, want) {
		r.t.Fatalf("error %v, want %v\n%s", err, want, r.out.String())
	}
	if after := strings.Join(r.releases(), ","); after != before {
		r.t.Fatalf("releases/ held %s and holds %s after the refusal", before, after)
	}
	return err
}

// fetched runs the fetch stage and wants a staged version.
func (r *rig) fetched(version string) *Result {
	r.t.Helper()
	res, err := r.f.Fetch(Options{})
	if err != nil {
		r.t.Fatalf("fetch: %v\n%s", err, r.out.String())
	}
	if res.Nothing || res.Version != version {
		r.t.Fatalf("result %+v, want %s\n%s", res, version, r.out.String())
	}
	return res
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
