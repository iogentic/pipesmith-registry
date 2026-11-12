package server

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/model"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/repos"
	"github.com/mirosso25/pipesmith-registry/internal/scan"
	"github.com/mirosso25/pipesmith-registry/internal/stores"
	"github.com/mirosso25/pipesmith-registry/internal/testkit"
)

const (
	jarPath   = "com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar"
	createdMS = int64(1_700_000_000_000)
)

// syncBuffer is a bytes.Buffer for the access log, written by the handler
// goroutines and read by the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// def is one repository definition, as repositories.json holds it.
func def(name, format, kind string, members ...string) *pyjson.Object {
	list := []any{}
	for _, m := range members {
		list = append(list, m)
	}
	return pyjson.NewObject().Set("name", name).Set("format", format).Set("type", kind).Set("members", list)
}

func dockerDef(name string, port int) *pyjson.Object {
	return def(name, "docker", "hosted").Set("http_port", port)
}

func spec(repo, path string, content []byte, contentType string) testkit.BlobSpec {
	s := testkit.NewBlobSpec(repo, path)
	s.Content = content
	s.CreatedMS = createdMS
	if contentType == "" {
		s.ContentType = nil
	} else {
		s.ContentType = &contentType
	}
	return s
}

func jarSpec(content string) testkit.BlobSpec {
	return spec("demo-releases", jarPath, []byte(content), "application/java-archive")
}

func blobStore(t *testing.T, base, name string, blobs ...testkit.BlobSpec) config.StoreConfig {
	t.Helper()
	root, err := testkit.MakeBlobStore(filepath.Join(base, name), blobs)
	if err != nil {
		t.Fatal(err)
	}
	return config.StoreConfig{Name: name, Kind: stores.KindBlobStore, Root: root}
}

func treeStore(t *testing.T, base, name string, files map[string][]byte) config.StoreConfig {
	t.Helper()
	root, err := testkit.MakeTree(filepath.Join(base, name), files)
	if err != nil {
		t.Fatal(err)
	}
	return config.StoreConfig{Name: name, Kind: stores.KindDirectory, Root: root}
}

// app is a data directory with stores, definitions and a scan, and a
// running server on it.
type app struct {
	t      *testing.T
	base   string
	cfg    *config.Config
	srv    *Server
	ts     *httptest.Server
	access *syncBuffer
	docker map[string]*httptest.Server
}

type appOptions struct {
	defs   []*pyjson.Object
	noScan bool
	// dockerRepos get a listener each, routed as a Docker port
	dockerRepos []string
	// security turns security on, as a state.sqlite does
	security model.Security
	// hostNames are the names the Host header may carry besides the local ones
	hostNames []string
}

func prepare(t *testing.T, base string, storeCfgs []config.StoreConfig, opts appOptions) *config.Config {
	t.Helper()
	cfg := config.New(filepath.Join(base, "data"))
	cfg.Stores = storeCfgs
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if opts.defs != nil {
		if err := repos.WriteDefinitions(cfg.RepositoriesPath(), opts.defs); err != nil {
			t.Fatal(err)
		}
	}
	if !opts.noScan {
		rescan(t, cfg)
	}
	return cfg
}

func rescan(t *testing.T, cfg *config.Config) {
	t.Helper()
	ix, err := index.Open(cfg.IndexPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	for _, sc := range cfg.Stores {
		s, err := stores.Make(sc, cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := scan.Run(s, ix, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func newApp(t *testing.T, storeCfgs []config.StoreConfig, opts appOptions) *app {
	t.Helper()
	base := t.TempDir()
	return startApp(t, base, prepare(t, base, storeCfgs, opts), opts)
}

func startApp(t *testing.T, base string, cfg *config.Config, opts appOptions) *app {
	t.Helper()
	a := &app{t: t, base: base, cfg: cfg, access: &syncBuffer{}, docker: map[string]*httptest.Server{}}
	ports := map[int]string{}
	listeners := map[string]net.Listener{}
	for _, repo := range opts.dockerRepos {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports[l.Addr().(*net.TCPAddr).Port] = repo
		listeners[repo] = l
	}
	// the main listener first: the server guards POST /api/scan with its port
	main, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = main.Addr().(*net.TCPAddr).Port
	srv, err := New(cfg, ports, Options{AccessLog: a.access, Security: opts.security, HostNames: opts.hostNames})
	if err != nil {
		t.Fatal(err)
	}
	a.srv = srv
	a.ts = httptest.NewUnstartedServer(srv)
	a.ts.Listener.Close()
	a.ts.Listener = main
	a.ts.Start()
	for repo, l := range listeners {
		ts := httptest.NewUnstartedServer(srv)
		ts.Listener.Close()
		ts.Listener = l
		ts.Start()
		a.docker[repo] = ts
	}
	t.Cleanup(func() {
		a.ts.Close()
		for _, ts := range a.docker {
			ts.Close()
		}
		srv.Close()
	})
	return a
}

// response is one answer read from a raw connection.
type response struct {
	status int
	header http.Header
	body   []byte
}

func (r *response) String() string {
	return fmt.Sprintf("%d %v %q", r.status, r.header, r.body)
}

// raw sends one request with the target exactly as given and reads the
// answer; Host is the address of the server unless headers name another.
func raw(t *testing.T, addr, method, target string, headers ...string) *response {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", method, target)
	hasHost := false
	for i := 0; i+1 < len(headers); i += 2 {
		if strings.EqualFold(headers[i], "Host") {
			hasHost = true
			if headers[i+1] == "\x00none" {
				continue
			}
		}
		fmt.Fprintf(&b, "%s: %s\r\n", headers[i], headers[i+1])
	}
	if !hasHost {
		fmt.Fprintf(&b, "Host: %s\r\n", addr)
	}
	b.WriteString("Connection: close\r\n\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(method, "http://"+addr+"/", nil)
	answer, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	defer answer.Body.Close()
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatal(err)
	}
	return &response{status: answer.StatusCode, header: answer.Header, body: body}
}

func (a *app) addr() string { return a.ts.Listener.Addr().String() }

func (a *app) get(target string, headers ...string) *response {
	a.t.Helper()
	return raw(a.t, a.addr(), "GET", target, headers...)
}

func (a *app) head(target string, headers ...string) *response {
	a.t.Helper()
	return raw(a.t, a.addr(), "HEAD", target, headers...)
}

func (a *app) on(repo, method, target string, headers ...string) *response {
	a.t.Helper()
	return raw(a.t, a.docker[repo].Listener.Addr().String(), method, target, headers...)
}

func expect(t *testing.T, r *response, status int) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r)
	}
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// file is a stored file of a builder: bytes and content type.
type file struct {
	data        []byte
	contentType string
}

const (
	dockerManifestType = "application/vnd.docker.distribution.manifest.v2+json"
	dockerConfigType   = "application/vnd.docker.container.image.v1+json"
	dockerLayerType    = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	ociIndexType       = "application/vnd.oci.image.index.v1+json"
	schema1Type        = "application/vnd.docker.distribution.manifest.v1+prettyjws"
)

// dockerImage is a schema 2 image: config and layers below v2/-/blobs/, the
// manifest by tag and by digest.
func dockerImage(name, tag string, layers ...[]byte) map[string]file {
	files := map[string]file{}
	config := []byte(fmt.Sprintf(`{"architecture": "amd64", "os": "linux", "name": %q}`, name+":"+tag))
	files["v2/-/blobs/"+digestOf(config)] = file{config, "application/octet-stream"}
	var refs []string
	for _, layer := range layers {
		files["v2/-/blobs/"+digestOf(layer)] = file{layer, "application/octet-stream"}
		refs = append(refs, fmt.Sprintf(`{"mediaType": %q, "size": %d, "digest": %q}`, dockerLayerType, len(layer), digestOf(layer)))
	}
	manifest := []byte(fmt.Sprintf(`{"schemaVersion": 2, "mediaType": %q, "config": {"mediaType": %q, "size": %d, "digest": %q}, "layers": [%s]}`,
		dockerManifestType, dockerConfigType, len(config), digestOf(config), strings.Join(refs, ", ")))
	files["v2/"+name+"/manifests/"+tag] = file{manifest, dockerManifestType}
	files["v2/"+name+"/manifests/"+digestOf(manifest)] = file{manifest, dockerManifestType}
	return files
}

func blobsOf(repo string, files map[string]file) []testkit.BlobSpec {
	var out []testkit.BlobSpec
	for path, f := range files {
		out = append(out, spec(repo, path, f.data, f.contentType))
	}
	return out
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
