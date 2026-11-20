package server

import (
	"context"
	"crypto/sha1" // #nosec G505 -- the SHA-1 of the index row, compared as the ETag of a file.
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobprops"
	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/scan"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Task 7 of the phase 5 plan: a proxy of the formats raw and maven2 fetches
// what it does not hold, stores it through ingest and answers from the
// stored file (decisions 5.7, 5.8, 5.9, 5.10, 5.11 and 5.20, rulings P5-3,
// P5-4 and P5-9 of the phase 5 session).

const (
	rawPath   = "docs/guide.txt"
	pomPath   = "com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom"
	pomBytes  = "<project><groupId>com.example</groupId></project>\n"
	rawBytes  = "the guide of the remote\n"
	fetchedBy = "demo-admin" // the user of asAdmin
)

// fetchProxies are a raw and a maven2 proxy with the defaults of decision
// 5.4.
var fetchProxies = []proxyDef{{name: "demo-raw-proxy", format: "raw"}, {name: "demo-maven-proxy", format: "maven2"}}

func (u *upstreamApp) root() string { return writeStoreRoot(u.cfg.DataDir) }

// row is the winning index row of a path, nil for none.
func (u *upstreamApp) row(repo, path string) *index.Asset {
	u.t.Helper()
	asset, err := u.srv.rp.index.Lookup(repo, path)
	if err != nil {
		u.t.Fatal(err)
	}
	return asset
}

// properties are the items of the properties file of a blob of the write
// store.
func (u *upstreamApp) properties(location string) map[string]string {
	u.t.Helper()
	data, err := os.ReadFile(filepath.Join(u.root(), strings.TrimSuffix(location, ".bytes")+".properties"))
	if err != nil {
		u.t.Fatal(err)
	}
	items, err := blobprops.Load(string(data))
	if err != nil {
		u.t.Fatal(err)
	}
	return items
}

// blobFiles are the files of the date tree of the write store, and the
// files of content/tmp.
func (u *upstreamApp) blobFiles() (blobs, work []string) {
	u.t.Helper()
	content := filepath.Join(u.root(), stores.ContentDir)
	err := filepath.WalkDir(content, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(content, path)
		if strings.HasPrefix(rel, blobwrite.TmpDir+string(filepath.Separator)) {
			work = append(work, rel)
		} else {
			blobs = append(blobs, rel)
		}
		return nil
	})
	if err != nil {
		u.t.Fatal(err)
	}
	return blobs, work
}

// tryGet is a GET of an administrator below /repository/ that returns its
// error, for a goroutine of a test, which must not stop the test itself.
func (u *upstreamApp) tryGet(repo, path string) (*answer, error) {
	r, err := http.NewRequest(http.MethodGet, "http://"+u.addr()+"/repository/"+repo+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	r.SetBasicAuth("demo-admin", "admin-pw")
	resp, err := noRedirect.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &answer{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

func sha1Of(data string) string {
	sum := sha1.Sum([]byte(data)) // #nosec G401 -- the SHA-1 of the index row, not a security use.
	return hex.EncodeToString(sum[:])
}

// A miss is fetched: the answer is the bytes of the remote with the
// headers of a stored file, the blob lies in the write store with its row
// in the index, records the user and the address of the request that
// caused it (decision 5.20), and a second request causes no request at the
// remote. HEAD of a miss fetches with GET as well (decision 5.7).
func TestProxyFetchesAMiss(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: fetchProxies})
	u.remote.Script(remoteBase("demo-raw-proxy")+rawPath, testkit.OK(rawBytes))
	u.remote.Script(remoteBase("demo-maven-proxy")+pomPath, testkit.OK(pomBytes))
	for _, c := range []struct{ repo, path, data, contentType string }{
		{"demo-raw-proxy", rawPath, rawBytes, "text/plain"},
		{"demo-maven-proxy", pomPath, pomBytes, "application/xml"},
	} {
		r := u.get(c.repo, c.path)
		if r.status != 200 || string(r.body) != c.data || r.header.Get("Content-Type") != c.contentType ||
			r.header.Get("ETag") != `"`+sha1Of(c.data)+`"` || r.header.Get("Last-Modified") == "" {
			t.Fatalf("%s/%s: %d %v %q", c.repo, c.path, r.status, r.header, r.body)
		}
		row := u.row(c.repo, c.path)
		if row == nil || row.Store != "local-write" || row.Size != int64(len(c.data)) || row.SHA1 == nil || *row.SHA1 != sha1Of(c.data) {
			t.Fatalf("%s/%s: row %+v", c.repo, c.path, row)
		}
		props := u.properties(row.Location)
		if props["@BlobStore.created-by"] != fetchedBy || props["@BlobStore.created-by-ip"] != "127.0.0.1" ||
			props["@Bucket.repo-name"] != c.repo || props["@BlobStore.blob-name"] != "/"+c.path {
			t.Fatalf("%s/%s: properties %v", c.repo, c.path, props)
		}
		again := u.get(c.repo, c.path)
		if again.status != 200 || string(again.body) != c.data {
			t.Fatalf("%s/%s again: %d %q", c.repo, c.path, again.status, again.body)
		}
		if n := u.remote.Count(remoteBase(c.repo) + c.path); n != 1 {
			t.Fatalf("%s/%s: %d requests at the remote", c.repo, c.path, n)
		}
	}
	const headPath = "docs/head.txt"
	u.remote.Script(remoteBase("demo-raw-proxy")+headPath, testkit.OK("asked with HEAD\n"))
	head := do(t, u.addr(), "HEAD", "/repository/demo-raw-proxy/"+headPath, nil, "basic", asAdmin)
	if head.status != 200 || len(head.body) != 0 || head.header.Get("Content-Length") != "16" {
		t.Fatalf("HEAD of a miss: %d %v %q", head.status, head.header, head.body)
	}
	if got := u.get("demo-raw-proxy", headPath); got.status != 200 || string(got.body) != "asked with HEAD\n" {
		t.Fatalf("GET after HEAD: %d %q", got.status, got.body)
	}
	var methods []string
	for _, req := range u.remote.Requests() {
		methods = append(methods, req.Method+" "+req.Target)
	}
	// after a new maven2 artifact its .sha1 (decision 5.10, leg 5c); the
	// remote holds none, so the pom is taken as it came
	want := []string{"GET " + remoteBase("demo-raw-proxy") + rawPath, "GET " + remoteBase("demo-maven-proxy") + pomPath,
		"GET " + remoteBase("demo-maven-proxy") + pomPath + ".sha1", "GET " + remoteBase("demo-raw-proxy") + headPath}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("requests at the remote %v, want %v", methods, want)
	}
}

// 50 requests at once for one path cause one request at the remote, and
// each gets the bytes (decision 5.8). The test runs three times.
func TestProxyFetchesOnce(t *testing.T) {
	for run := 1; run <= 3; run++ {
		u := startUpstream(t, upstreamSetup{upstream: true, proxies: fetchProxies})
		u.remote.Script(remoteBase("demo-raw-proxy")+rawPath, testkit.SlowHeader(300*time.Millisecond, testkit.OK(rawBytes)))
		var wg sync.WaitGroup
		answers := make([]*answer, 50)
		errs := make([]error, 50)
		for i := range answers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				answers[i], errs[i] = u.tryGet("demo-raw-proxy", rawPath)
			}()
		}
		wg.Wait()
		for i, r := range answers {
			if errs[i] != nil {
				t.Fatalf("run %d, request %d: %v", run, i, errs[i])
			}
			if r.status != 200 || string(r.body) != rawBytes {
				t.Fatalf("run %d, request %d: %d %q", run, i, r.status, r.body)
			}
		}
		if n := u.remote.Count(remoteBase("demo-raw-proxy") + rawPath); n != 1 {
			t.Fatalf("run %d: %d requests at the remote for 50 at once", run, n)
		}
		t.Logf("run %d: 50 requests, %d at the remote", run, u.remote.Count(remoteBase("demo-raw-proxy")+rawPath))
	}
}

// A body shorter than its declared length leaves no blob, no row and no work
// file, and the miss answers 502 (decision 5.9).
func TestProxyShortBodyStoresNothing(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: fetchProxies})
	u.remote.Script(remoteBase("demo-raw-proxy")+rawPath, testkit.ShortBody(4096, "the first bytes only"))
	r := u.get("demo-raw-proxy", rawPath)
	if r.status != http.StatusBadGateway {
		t.Fatalf("short body: %d %q", r.status, r.body)
	}
	if row := u.row("demo-raw-proxy", rawPath); row != nil {
		t.Fatalf("a row of a short body: %+v", row)
	}
	if blobs, work := u.blobFiles(); len(blobs) != 0 || len(work) != 0 {
		t.Fatalf("blobs %v, work files %v", blobs, work)
	}
}

// A scan of the write store from its files alone gives the rows the live
// index holds for what the proxies fetched (the task row of task 7).
func TestProxyRescanEqualsTheLiveIndex(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: fetchProxies})
	fetched := map[string][]string{
		"demo-raw-proxy": {rawPath, "docs/nested/deeper/notes.txt", "space name.txt"},
		// a .sha1 of a cached file comes from its index row, as for a hosted file; the one of a jar the cache lacks is fetched
		"demo-maven-proxy": {pomPath, "com/example/demo-lib/maven-metadata.xml", "com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar.sha1"},
	}
	for repo, paths := range fetched {
		for _, path := range paths {
			u.remote.Script(remoteBase(repo)+strings.ReplaceAll(path, " ", "%20"), testkit.OK("bytes of "+path+"\n"))
			target := strings.ReplaceAll(path, " ", "%20")
			if r := u.get(repo, target); r.status != 200 || string(r.body) != "bytes of "+path+"\n" {
				t.Fatalf("%s/%s: %d %q", repo, path, r.status, r.body)
			}
		}
	}
	fresh, err := index.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	store, err := stores.NewBlobStore("local-write", u.root())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scan.Run(store, fresh, nil); err != nil {
		t.Fatal(err)
	}
	for repo, paths := range fetched {
		for _, path := range paths {
			live, err := u.srv.rp.index.Rows(repo, path)
			if err != nil {
				t.Fatal(err)
			}
			scanned, err := fresh.Rows(repo, path)
			if err != nil {
				t.Fatal(err)
			}
			if len(live) != 1 || !reflect.DeepEqual(live, scanned) {
				t.Fatalf("%s/%s: live %+v, scanned %+v", repo, path, live, scanned)
			}
		}
	}
}

// A range request and a conditional request that cause the fetch answer as
// the same requests on a hosted file of the same bytes, written at the same
// time (the task row of task 7).
func TestProxyAnswersLikeAHostedFile(t *testing.T) {
	clock := testkit.NewClock(time.Date(2026, 11, 19, 6, 30, 15, 250_000_000, time.UTC))
	u := startUpstream(t, upstreamSetup{upstream: true, writes: true, proxies: fetchProxies, tune: func(o *appOptions) {
		o.writeOptions = blobwrite.Options{Now: clock.Now}
	}})
	const data = "0123456789abcdefghijklmnopqrstuvwxyz\n"
	etag := `"` + sha1Of(data) + `"`
	modified := clock.Now().UTC().Format(http.TimeFormat)
	cases := [][]string{
		{"Range", "bytes=2-9"},
		{"Range", "bytes=-5"},
		{"Range", "bytes=30-"},
		{"Range", "bytes=500-"},
		{"Range", "bytes=1-3,5-7"},
		{"Range", "bytes=2-9", "If-Range", etag},
		{"Range", "bytes=2-9", "If-Range", `"another"`},
		{"If-None-Match", etag},
		{"If-None-Match", `"another"`},
		{"If-Modified-Since", modified},
		{"If-Modified-Since", "Mon, 02 Jan 2006 15:04:05 GMT"},
		{},
	}
	for i, headers := range cases {
		path := fmt.Sprintf("files/case-%d.bin", i)
		if r := do(t, u.addr(), "PUT", "/repository/demo-raw/"+path, []byte(data), "basic", asAdmin); r.status != 201 {
			t.Fatalf("PUT %s: %d %q", path, r.status, r.body)
		}
		u.remote.Script(remoteBase("demo-raw-proxy")+path, testkit.OK(data))
		hosted := u.get("demo-raw", path, headers...)
		proxied := u.get("demo-raw-proxy", path, headers...)
		hosted.header.Del("Date")
		proxied.header.Del("Date")
		if hosted.status != proxied.status || string(hosted.body) != string(proxied.body) || !reflect.DeepEqual(hosted.header, proxied.header) {
			t.Errorf("%v: hosted %d %v %q, proxy %d %v %q", headers, hosted.status, hosted.header, hosted.body,
				proxied.status, proxied.header, proxied.body)
		}
		if n := u.remote.Count(remoteBase("demo-raw-proxy") + path); n != 1 {
			t.Errorf("%v: %d requests at the remote", headers, n)
		}
	}
}

// The query of a client request never reaches the remote, and a directory
// request causes no request (decision P5-9, the rulings of brief 5b).
func TestProxyQueryAndFolder(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: fetchProxies})
	u.remote.Script(remoteBase("demo-raw-proxy")+rawPath, testkit.OK(rawBytes))
	if r := u.get("demo-raw-proxy", rawPath+"?version=2&token=client-value"); r.status != 200 || string(r.body) != rawBytes {
		t.Fatalf("with a query: %d %q", r.status, r.body)
	}
	for _, folder := range []string{"docs/", "elsewhere/", ""} {
		u.get("demo-raw-proxy", folder)
	}
	var targets []string
	for _, req := range u.remote.Requests() {
		targets = append(targets, req.Target)
	}
	if want := []string{remoteBase("demo-raw-proxy") + rawPath}; !reflect.DeepEqual(targets, want) {
		t.Fatalf("requests at the remote %v, want %v", targets, want)
	}
}

// A routing rule decides before the cache and before any request (decision
// 5.15, ruling P5-4): a blocked path answers 403 although it is cached and
// causes no request, with upstream on and off; another path is fetched. The
// repository carries the name of its rule, and a rule that does not compile
// blocks every path of its repositories.
func TestProxyRoutingRules(t *testing.T) {
	rules := &model.Routing{
		Rules: []model.RoutingRule{
			{Name: "demo-block-docs", Mode: "BLOCK", Matchers: []string{"^/docs/blocked/.*"}},
			{Name: "demo-broken", Mode: "BLOCK", Matchers: []string{"(unclosed"}},
		},
		Links: map[string]string{"demo-raw-proxy": "demo-block-docs", "demo-maven-proxy": "demo-broken"},
	}
	cached := spec("demo-raw-proxy", "docs/blocked/cached.txt", []byte("cached under a blocked path"), "text/plain")
	for _, upstream := range []bool{false, true} {
		u := startUpstream(t, upstreamSetup{upstream: upstream, proxies: fetchProxies, blobs: []testkit.BlobSpec{cached},
			tune: func(o *appOptions) { o.routing = rules }})
		u.remote.Script(remoteBase("demo-raw-proxy")+rawPath, testkit.OK(rawBytes))
		for _, target := range [][2]string{{"demo-raw-proxy", "docs/blocked/cached.txt"}, {"demo-raw-proxy", "docs/blocked/missing.txt"},
			{"demo-raw-proxy", "docs/blocked/"}, {"demo-maven-proxy", pomPath}} {
			if r := u.get(target[0], target[1]); r.status != http.StatusForbidden {
				t.Errorf("upstream %v, %s/%s: %d %q", upstream, target[0], target[1], r.status, r.body)
			}
		}
		want := 0
		if upstream {
			want = 1
		}
		if r := u.get("demo-raw-proxy", rawPath); upstream && (r.status != 200 || string(r.body) != rawBytes) || !upstream && r.status != 404 {
			t.Errorf("upstream %v, an allowed path: %d %q", upstream, r.status, r.body)
		}
		if n := len(u.remote.Requests()); n != want {
			t.Errorf("upstream %v: %d requests at the remote: %+v", upstream, n, u.remote.Requests())
		}
		st, err := u.srv.rp.sync()
		if err != nil {
			t.Fatal(err)
		}
		repo, _ := st.repositories.Get("demo-raw-proxy")
		if settings, err := repo.ProxySettings(); err != nil || settings.RoutingRule != "demo-block-docs" {
			t.Errorf("the rule of the repository: %+v %v", settings, err)
		}
	}
}

// The start log names each problem of the routing rules (ruling P5-4) and
// each proxy whose settings are refused, with repository and key.
func TestProxyStartLog(t *testing.T) {
	log := captureLog(t)
	rules := &model.Routing{Rules: []model.RoutingRule{{Name: "demo-broken", Mode: "BLOCK", Matchers: []string{"(unclosed"}}},
		Links: map[string]string{"demo-raw-proxy": "demo-broken", "demo-maven-proxy": "demo-missing-rule"}}
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{{name: "demo-raw-proxy", format: "raw"},
		{name: "demo-maven-proxy", format: "maven2", attributes: map[string]any{"proxy": map[string]any{"contentMaxAge": "a day"}}}},
		tune: func(o *appOptions) { o.routing = rules }})
	u.srv.logUpstream()
	text := strings.Join(log.all(), "\n")
	for _, want := range []string{`routing rule "demo-broken" blocks every path`, `repository "demo-maven-proxy" names the routing rule "demo-missing-rule"`,
		`proxy demo-maven-proxy serves its cache only and asks no remote: repository "demo-maven-proxy": proxy.contentMaxAge`} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in the log:\n%s", want, text)
		}
	}
	if strings.Contains(text, "a day") {
		t.Fatalf("the log names the value of a refused setting:\n%s", text)
	}
}

// A maven2 path outside the version or layout policy of the proxy answers
// 404 and causes no request (ruling P5-3).
func TestProxyMavenPolicies(t *testing.T) {
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{
		{name: "demo-releases-proxy", format: "maven2", attributes: map[string]any{"maven": map[string]any{"versionPolicy": "RELEASE",
			"layoutPolicy": "STRICT"}}},
		{name: "demo-snapshots-proxy", format: "maven2", attributes: map[string]any{"maven": map[string]any{"versionPolicy": "SNAPSHOT"}}},
	}})
	const snapshotPath = "com/example/demo-lib/1.1.0-SNAPSHOT/demo-lib-1.1.0-20261119.063015-1.jar"
	for _, c := range [][2]string{{"demo-releases-proxy", snapshotPath}, {"demo-releases-proxy", "not/a/maven/path.txt"},
		{"demo-snapshots-proxy", pomPath}} {
		if r := u.get(c[0], c[1]); r.status != 404 {
			t.Errorf("%s/%s: %d %q", c[0], c[1], r.status, r.body)
		}
	}
	u.remote.Script(remoteBase("demo-releases-proxy")+pomPath, testkit.OK(pomBytes))
	if r := u.get("demo-releases-proxy", pomPath); r.status != 200 {
		t.Fatalf("a release path: %d %q", r.status, r.body)
	}
	// the pom and, after it, its .sha1 (decision 5.10, leg 5c)
	var targets []string
	for _, req := range u.remote.Requests() {
		targets = append(targets, req.Target)
	}
	if want := []string{remoteBase("demo-releases-proxy") + pomPath, remoteBase("demo-releases-proxy") + pomPath + ".sha1"}; !reflect.DeepEqual(targets, want) {
		t.Fatalf("requests at the remote %v, want %v", targets, want)
	}
}

// What the store of a repository refuses discards the download, stores
// nothing and answers 502 (ruling P5-3): a content type the strict check
// refuses, a body over the limit, a body the free space floor has no room
// for. The write policy of a hosted repository does not apply to a fetch.
func TestProxyStoreRefusals(t *testing.T) {
	strict := map[string]any{"storage": map[string]any{"strictContentTypeValidation": true}}
	u := startUpstream(t, upstreamSetup{upstream: true, proxies: []proxyDef{
		{name: "demo-strict-proxy", format: "maven2", attributes: strict},
		{name: "demo-small-proxy", format: "raw", attributes: map[string]any{"max_body_bytes": 16}},
		{name: "demo-full-proxy", format: "raw", attributes: map[string]any{"free_space_floor_bytes": int64(1) << 62}},
		{name: "demo-deny-proxy", format: "raw", attributes: map[string]any{"storage": map[string]any{"writePolicy": "DENY"}}},
	}})
	const jarPathOfProxy = "com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar"
	u.remote.Script(remoteBase("demo-strict-proxy")+jarPathOfProxy, testkit.OK("not a zip archive"))
	u.remote.Script(remoteBase("demo-small-proxy")+rawPath, testkit.OK("seventeen bytes!!"))
	u.remote.Script(remoteBase("demo-small-proxy")+"docs/chunked.txt", testkit.Chunked("a chunked body over sixteen bytes"))
	u.remote.Script(remoteBase("demo-full-proxy")+rawPath, testkit.OK(rawBytes))
	u.remote.Script(remoteBase("demo-deny-proxy")+rawPath, testkit.OK(rawBytes))
	for _, c := range [][2]string{{"demo-strict-proxy", jarPathOfProxy}, {"demo-small-proxy", rawPath}, {"demo-small-proxy", "docs/chunked.txt"},
		{"demo-full-proxy", rawPath}} {
		if r := u.get(c[0], c[1]); r.status != http.StatusBadGateway {
			t.Errorf("%s/%s: %d %q", c[0], c[1], r.status, r.body)
		}
		if row := u.row(c[0], c[1]); row != nil {
			t.Errorf("%s/%s: a row %+v", c[0], c[1], row)
		}
	}
	if blobs, work := u.blobFiles(); len(blobs) != 0 || len(work) != 0 {
		t.Fatalf("blobs %v, work files %v", blobs, work)
	}
	if r := u.get("demo-deny-proxy", rawPath); r.status != 200 || string(r.body) != rawBytes {
		t.Fatalf("a proxy with the write policy DENY: %d %q", r.status, r.body)
	}
}

// A fetch goes on when its client leaves, and a waiting client that leaves
// does not cancel it (decision 5.8). The test runs three times.
func TestProxyFetchOutlivesItsClients(t *testing.T) {
	for run := 1; run <= 3; run++ {
		u := startUpstream(t, upstreamSetup{upstream: true, proxies: fetchProxies})
		u.remote.Script(remoteBase("demo-raw-proxy")+rawPath, testkit.SlowHeader(400*time.Millisecond, testkit.OK(rawBytes)))
		u.remote.Script(remoteBase("demo-raw-proxy")+"docs/second.txt", testkit.SlowHeader(400*time.Millisecond, testkit.OK("second\n")))
		leave := func(path string, after time.Duration) {
			ctx, cancel := context.WithTimeout(context.Background(), after)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "GET", "http://"+u.addr()+"/repository/demo-raw-proxy/"+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.SetBasicAuth("demo-admin", "admin-pw")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
				t.Errorf("run %d: the client that leaves got %d", run, resp.StatusCode)
			}
		}
		// the first client leaves while the fetch waits for the remote
		leave(rawPath, 100*time.Millisecond)
		deadline := time.Now().Add(10 * time.Second)
		for u.row("demo-raw-proxy", rawPath) == nil {
			if time.Now().After(deadline) {
				t.Fatalf("run %d: the fetch stopped with its client", run)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if r := u.get("demo-raw-proxy", rawPath); r.status != 200 || string(r.body) != rawBytes {
			t.Fatalf("run %d: after the fetch: %d %q", run, r.status, r.body)
		}
		// a waiting client leaves, the first one gets the file
		type got struct {
			r   *answer
			err error
		}
		first := make(chan got, 1)
		go func() {
			r, err := u.tryGet("demo-raw-proxy", "docs/second.txt")
			first <- got{r, err}
		}()
		time.Sleep(50 * time.Millisecond)
		leave("docs/second.txt", 100*time.Millisecond)
		if g := <-first; g.err != nil || g.r.status != 200 || string(g.r.body) != "second\n" {
			t.Fatalf("run %d: the client that stayed: %+v", run, g)
		}
		for _, path := range []string{rawPath, "docs/second.txt"} {
			if n := u.remote.Count(remoteBase("demo-raw-proxy") + path); n != 1 {
				t.Fatalf("run %d, %s: %d requests at the remote", run, path, n)
			}
		}
	}
}
