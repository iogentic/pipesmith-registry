package server

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// The switch upstream (decision 5.1 of the phase 5 plan) and the read path
// of a proxy of the formats raw and maven2 (phase 5, leg 5b).

// proxyDef is a proxy repository of an upstream test: its attributes are
// stored over the address of its remote, a folder of the scripted remote
// named like the proxy.
type proxyDef struct {
	name, format string
	attributes   map[string]any
}

// remoteBase is the folder of the scripted remote a proxy asks.
func remoteBase(name string) string { return "/" + name + "/" }

// upstreamApp is a server with security on over the store store-a of the old
// product, with the hosted repositories demo-raw and demo-maven and the
// proxies of a test, whose remote is one scripted remote.
type upstreamApp struct {
	*app
	remote *testkit.Remote
	sec    *fakeSecurity
}

// repoDef is a hosted repository or a group of an upstream test beyond
// demo-raw and demo-maven, with its members and its stored attributes; a
// hosted one takes the write policy ALLOW unless its attributes name
// storage.
type repoDef struct {
	name, format, kind string
	members            []string
	attributes         map[string]any
}

// upstreamSetup is what startUpstream builds a server from.
type upstreamSetup struct {
	upstream bool // the switch upstream
	writes   bool // the switch writes
	proxies  []proxyDef
	repos    []repoDef          // hosted repositories and groups after the proxies
	blobs    []testkit.BlobSpec // the blobs of store-a
	// options of the server beyond those above: the clock and ids of the
	// writer, the state store and the clock of the proxies, routing rules
	tune func(*appOptions)
}

func startUpstream(t *testing.T, setup upstreamSetup) *upstreamApp {
	t.Helper()
	remote := testkit.NewRemote(t)
	sec := newFakeSecurity()
	snap := *sec.snap
	snap.Repositories = nil
	defs := []*pyjson.Object{def("demo-raw", "raw", "hosted"), def("demo-maven", "maven2", "hosted")}
	for _, name := range []string{"demo-raw", "demo-maven"} {
		snap.Repositories = append(snap.Repositories, model.Repository{Name: name, Format: map[string]string{"demo-raw": "raw",
			"demo-maven": "maven2"}[name], Type: "hosted", Online: true, Attributes: storage("ALLOW")})
	}
	for _, p := range setup.proxies {
		attributes := map[string]any{"proxy": map[string]any{"remoteUrl": remote.URL() + remoteBase(p.name)}}
		for key, value := range p.attributes {
			if section, ok := value.(map[string]any); ok && key == "proxy" {
				merged := map[string]any{"remoteUrl": remote.URL() + remoteBase(p.name)}
				for k, v := range section {
					merged[k] = v
				}
				value = merged
			}
			attributes[key] = value
		}
		snap.Repositories = append(snap.Repositories, model.Repository{Name: p.name, Format: p.format, Type: "proxy", Online: true,
			Attributes: attributes})
		defs = append(defs, def(p.name, p.format, "proxy"))
	}
	for _, r := range setup.repos {
		attributes := r.attributes
		if _, named := attributes["storage"]; r.kind == "hosted" && !named {
			attributes = storage("ALLOW")
			for key, value := range r.attributes {
				attributes[key] = value
			}
		}
		snap.Repositories = append(snap.Repositories, model.Repository{Name: r.name, Format: r.format, Type: r.kind, Online: true,
			Attributes: attributes})
		defs = append(defs, def(r.name, r.format, r.kind, r.members...))
	}
	sec.snap = &snap
	base := t.TempDir()
	opts := appOptions{defs: defs, security: sec, writes: setup.writes, upstream: setup.upstream}
	if setup.tune != nil {
		setup.tune(&opts)
	}
	a := startApp(t, base, prepare(t, base, []config.StoreConfig{blobStore(t, base, "store-a", setup.blobs...)}, opts), opts)
	return &upstreamApp{app: a, remote: remote, sec: sec}
}

// get is a GET of an administrator below /repository/.
func (u *upstreamApp) get(repo, path string, headers ...string) *answer {
	u.t.Helper()
	return do(u.t, u.addr(), "GET", "/repository/"+repo+"/"+path, nil, append([]string{"basic", asAdmin}, headers...)...)
}

// Upstream needs security (decision P5-2, as E4 for writes): New refuses it
// without, with an error that names the value, and makes no write store.
func TestUpstreamNeedsSecurity(t *testing.T) {
	cfg := writeData(t)
	if srv, err := New(cfg, nil, Options{Upstream: true}); err == nil {
		srv.Close()
		t.Fatal("upstream without security")
	} else if !strings.Contains(err.Error(), "state.sqlite") || !strings.Contains(err.Error(), "upstream") {
		t.Fatalf("error %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "stores")); err == nil {
		t.Fatal("the refused start made the write store")
	}
}

// With upstream on and writes off the write store is made and its writer
// holds its lock, so that a proxy can store what it fetches (decision 5.1),
// while client writes keep every answer of writes off: the route table, the
// refusals of GET /api/areas and the writable status (decision P5-2).
func TestUpstreamOpensTheWriteStore(t *testing.T) {
	cfg := writeData(t)
	srv, err := New(cfg, nil, Options{Upstream: true, Security: newFakeSecurity()})
	if err != nil {
		t.Fatal(err)
	}
	st, err := srv.rp.sync()
	if err != nil {
		t.Fatal(err)
	}
	if s := st.stores["local-write"]; s == nil || !stores.Writable(s) || srv.writes.writer == nil || srv.writes.writer.Name() != "local-write" {
		t.Fatalf("write store %v, writer %v", s, srv.writes.writer)
	}
	if srv.writes.clients {
		t.Fatal("client writes are on with writes off")
	}
	if srv.table() != securedTable {
		t.Fatal("the route table is not the one of writes off")
	}
	if !reflect.DeepEqual(srv.refusedList(), refusedWritesOff) {
		t.Fatal("the refusals are not those of writes off")
	}
	if srv.writes.writable() {
		t.Fatal("the writable status with writes off")
	}
	if second, err := New(cfg, nil, Options{Upstream: true, Security: newFakeSecurity()}); err == nil {
		second.Close()
		t.Fatal("a second server writes the same store")
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	named := prepare(t, base, []config.StoreConfig{blobStore(t, base, "local-write", jarSpec("jar bytes"))}, appOptions{})
	if srv, err := New(named, nil, Options{Upstream: true, Security: newFakeSecurity()}); err == nil {
		srv.Close()
		t.Fatal("upstream on beside a configured store named local-write")
	} else if !strings.Contains(err.Error(), "before upstream is switched on") {
		t.Fatalf("error %v", err)
	}
}

// The reads that answer only while writes are on (the dist tags of npm, the
// state of a Docker upload session) and the writable status answer with
// upstream on and writes off as with both off.
func TestUpstreamKeepsTheReadsOfWritesOff(t *testing.T) {
	off := startSwitched(t, newFakeSecurity(), false, false)
	up := startSwitched(t, newFakeSecurity(), false, true)
	for _, target := range []string{"/repository/demo-npm/-/package/widget/dist-tags", "/repository/demo-docker/v2/app/blobs/uploads/0123",
		"/service/rest/v1/status/writable", "/api/areas"} {
		before := do(t, off.addr(), "GET", target, nil, "basic", asAdmin)
		after := do(t, up.addr(), "GET", target, nil, "basic", asAdmin)
		before.header.Del("Date")
		after.header.Del("Date")
		if before.status != after.status || string(before.body) != string(after.body) || !reflect.DeepEqual(before.header, after.header) {
			t.Errorf("%s: upstream %d %q %v, off %d %q %v", target, after.status, after.body, after.header, before.status, before.body, before.header)
		}
	}
}

// A miss of a proxy causes no request at its remote while upstream is off
// and answers 404 as in 0.1.1; with upstream on the same miss causes one
// request, and the 404 of the remote is the answer (decision 5.9); a
// blocked proxy causes none and serves its cache (decision 5.2).
func TestProxyMissAsksTheRemote(t *testing.T) {
	cached := spec("demo-raw-blocked", "docs/cached.txt", []byte("cached bytes"), "text/plain")
	proxies := []proxyDef{{name: "demo-raw-proxy", format: "raw"}, {name: "demo-maven-proxy", format: "maven2"},
		{name: "demo-raw-blocked", format: "raw", attributes: map[string]any{"httpclient": map[string]any{"blocked": true}}}}
	misses := [][2]string{{"demo-raw-proxy", "docs/missing.txt"}, {"demo-maven-proxy", "com/example/demo-gone/1.0.0/demo-gone-1.0.0.jar"},
		{"demo-raw-blocked", "docs/missing.txt"}}
	for _, upstream := range []bool{false, true} {
		u := startUpstream(t, upstreamSetup{upstream: upstream, proxies: proxies, blobs: []testkit.BlobSpec{cached}})
		for _, miss := range misses {
			r := u.get(miss[0], miss[1])
			if r.status != 404 || string(r.body) != "Not found\n" {
				t.Errorf("upstream %v, %s/%s: %d %q", upstream, miss[0], miss[1], r.status, r.body)
			}
			want := 0
			if upstream && miss[0] != "demo-raw-blocked" {
				want = 1
			}
			if got := u.remote.Count(remoteBase(miss[0]) + miss[1]); got != want {
				t.Errorf("upstream %v, %s/%s: %d requests at the remote, want %d", upstream, miss[0], miss[1], got, want)
			}
		}
		if r := u.get("demo-raw-blocked", "docs/cached.txt"); r.status != 200 || string(r.body) != "cached bytes" {
			t.Errorf("upstream %v, the cache of a blocked proxy: %d %q", upstream, r.status, r.body)
		}
		if n := len(u.remote.Requests()); upstream && n != 2 || !upstream && n != 0 {
			t.Errorf("upstream %v: %d requests at the remote: %+v", upstream, n, u.remote.Requests())
		}
	}
}
