package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobprops"
	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// fakeCatalog records the calls of the catalog hook.
type fakeCatalog struct {
	mu      sync.Mutex
	puts    []string // repo/path uploader address sha256
	deletes []string // repo/path
}

func (f *fakeCatalog) Put(asset index.Asset, digests stores.Digests, uploader, address string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, fmt.Sprintf("%s/%s %s %s %s", asset.Repo, asset.Path, uploader, address, digests.SHA256))
	return nil
}

func (f *fakeCatalog) Delete(repo, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, repo+"/"+path)
	return nil
}

// clock is a writer clock a test moves.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// writing is a server with writes on over the store store-a of the old
// product, with hosted repositories of the four formats whose stored
// attributes are given.
type writing struct {
	*app
	catalog *fakeCatalog
	clock   *clock
	old     string     // the root of store-a
	opts    appOptions // the options of the server, for a restart
}

func startIngest(t *testing.T, attributes map[string]map[string]any, blobs ...testkit.BlobSpec) *writing {
	t.Helper()
	sec := newFakeSecurity()
	snap := *sec.snap
	snap.Repositories = nil
	var defs []*pyjson.Object
	for _, r := range [][2]string{{"demo-raw", "raw"}, {"demo-releases", "maven2"}, {"demo-docker", "docker"}, {"demo-npm", "npm"}} {
		snap.Repositories = append(snap.Repositories, model.Repository{Name: r[0], Format: r[1], Type: "hosted", Online: true,
			Attributes: attributes[r[0]]})
		defs = append(defs, def(r[0], r[1], "hosted"))
	}
	sec.snap = &snap
	base := t.TempDir()
	storeCfgs := []config.StoreConfig{blobStore(t, base, "store-a", blobs...)}
	w := &writing{catalog: &fakeCatalog{}, clock: &clock{now: time.Date(2026, 11, 18, 10, 15, 30, 123_000_000, time.UTC)},
		old: storeCfgs[0].Root}
	w.opts = appOptions{defs: defs, security: sec, writes: true, catalog: w.catalog, writeOptions: blobwrite.Options{Now: w.clock.Now}}
	w.app = startApp(t, base, prepare(t, base, storeCfgs, w.opts), w.opts)
	return w
}

func (w *writing) repo(name string) repos.Repo {
	w.t.Helper()
	st, err := w.srv.rp.sync()
	if err != nil {
		w.t.Fatal(err)
	}
	r, ok := st.repositories.Get(name)
	if !ok {
		w.t.Fatalf("no repository %s", name)
	}
	return r
}

func (w *writing) ingest(repo, path, body string, mutate ...func(*ingestRequest)) (*ingestResult, error) {
	w.t.Helper()
	in := ingestRequest{repo: w.repo(repo), path: path, body: strings.NewReader(body), size: int64(len(body)),
		subject: subject("demo-admin", "", "nexus:*"), address: "192.0.2.7"}
	for _, m := range mutate {
		m(&in)
	}
	return w.srv.ingest(in)
}

func (w *writing) root() string { return writeStoreRoot(w.cfg.DataDir) }

// workFiles are the files in content/tmp of the write store.
func (w *writing) workFiles() []string {
	w.t.Helper()
	entries, err := os.ReadDir(filepath.Join(w.root(), "content", "tmp"))
	if err != nil {
		w.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func (w *writing) scan() (stores.ScanCounts, []stores.Entry) {
	w.t.Helper()
	s, err := stores.NewBlobStore("scan", w.root())
	if err != nil {
		w.t.Fatal(err)
	}
	var counts stores.ScanCounts
	var live []stores.Entry
	if err := s.Entries(&counts, func(e stores.Entry) error { live = append(live, e); return nil }); err != nil {
		w.t.Fatal(err)
	}
	return counts, live
}

func (w *writing) properties(location string) map[string]string {
	w.t.Helper()
	data, err := os.ReadFile(filepath.Join(w.root(), strings.TrimSuffix(location, ".bytes")+".properties"))
	if err != nil {
		w.t.Fatal(err)
	}
	items, err := blobprops.Load(string(data))
	if err != nil {
		w.t.Fatal(err)
	}
	return items
}

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func storage(policy string, more ...any) map[string]any {
	s := map[string]any{"writePolicy": policy}
	for i := 0; i+1 < len(more); i += 2 {
		s[more[i].(string)] = more[i+1]
	}
	return map[string]any{"storage": s}
}

// One write: the blob in the write store with its properties, the row in
// the index, the catalog hook, and the read path serves it.
func TestIngestWritesABlobAndItsRow(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	if got := w.repo("demo-raw").WriteSettings().WritePolicy; got != "ALLOW" {
		t.Fatalf("the attributes of the state do not reach the repository: %q", got)
	}
	result, err := w.ingest("demo-raw", "docs/hello.txt", "hello world", func(in *ingestRequest) {
		in.digests = map[string]string{"sha256": sha256Hex("hello world")}
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.asset.Store != "local-write" || result.asset.Size != 11 || result.replaced != nil ||
		result.digests.SHA256 != sha256Hex("hello world") || !strings.HasPrefix(result.asset.Location, "content/2026/11/18/10/15/") {
		t.Fatalf("%+v", result)
	}
	props := w.properties(result.asset.Location)
	if props["@BlobStore.created-by"] != "demo-admin" || props["@BlobStore.created-by-ip"] != "192.0.2.7" ||
		props["@BlobStore.content-type"] != "text/plain" || props["@Bucket.repo-name"] != "demo-raw" || props["@BlobStore.blob-name"] != "/docs/hello.txt" {
		t.Fatalf("%q", props)
	}
	r := do(t, w.addr(), "GET", "/repository/demo-raw/docs/hello.txt", nil, "basic", asAdmin)
	if r.status != 200 || string(r.body) != "hello world" {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if want := "demo-raw/docs/hello.txt demo-admin 192.0.2.7 " + sha256Hex("hello world"); len(w.catalog.puts) != 1 || w.catalog.puts[0] != want {
		t.Fatalf("catalog %q", w.catalog.puts)
	}
	if len(w.workFiles()) != 0 {
		t.Fatalf("work files %v", w.workFiles())
	}
}

// Every refusal of the rules and limits comes before a byte reaches the
// store: no work file, no blob, no row.
func TestIngestRefusals(t *testing.T) {
	old := spec("demo-raw", "docs/readme.txt", []byte("readme"), "text/plain")
	attrs := map[string]map[string]any{
		"demo-raw":      storage("ALLOW_ONCE", "strictContentTypeValidation", true),
		"demo-releases": {"storage": map[string]any{"writePolicy": "ALLOW"}, "maven": map[string]any{"versionPolicy": "RELEASE", "layoutPolicy": "STRICT"}},
		"demo-npm":      storage("DENY"),
		"demo-docker":   {"storage": map[string]any{"writePolicy": "ALLOW"}, "max_body_bytes": 4},
	}
	w := startIngest(t, attrs, old)
	withSize := func(n int64) func(*ingestRequest) { return func(in *ingestRequest) { in.size = n } }
	for _, c := range []struct {
		name, repo, path, body string
		mutate                 func(*ingestRequest)
		status                 int
		rule                   string
	}{
		{"write policy DENY", "demo-npm", "demo-widget", "{}", nil, 400, "read-only"},
		{"second write under ALLOW_ONCE", "demo-raw", "docs/readme.txt", "again", nil, 400, "redeploy"},
		{"snapshot into a release repository", "demo-releases", "com/example/demo-lib/1.1.0-SNAPSHOT/demo-lib-1.1.0-SNAPSHOT.pom", "<project/>", nil, 400, "version"},
		{"no Maven path under STRICT", "demo-releases", "docs/readme.txt", "text", nil, 400, "layout"},
		{"content against its extension", "demo-raw", "dist/demo.jar", "hello", nil, 400, "content"},
		{"declared length differs", "demo-raw", "docs/a.txt", "hello world", withSize(5), 400, "size"},
		{"digest differs", "demo-raw", "docs/b.txt", "hello world", func(in *ingestRequest) {
			in.digests = map[string]string{"sha1": strings.Repeat("0", 40)}
		}, 400, "digest"},
		{"declared length above the limit", "demo-docker", "v2/-/blobs/sha256:00", "hello", nil, 413, "too-large"},
		{"body above the limit", "demo-docker", "v2/-/blobs/sha256:01", "hello", withSize(-1), 413, "too-large"},
	} {
		_, err := w.ingest(c.repo, c.path, c.body, func(in *ingestRequest) {
			if c.mutate != nil {
				c.mutate(in)
			}
		})
		var refusal *writeRefusal
		if !errors.As(err, &refusal) || refusal.status != c.status || refusal.rule != c.rule || refusal.message == "" {
			t.Errorf("%s: %v (%+v), want %d %s", c.name, err, refusal, c.status, c.rule)
			continue
		}
		if row, _ := w.srv.rp.index.Lookup(c.repo, c.path); row != nil && row.Store == "local-write" {
			t.Errorf("%s: a row %+v", c.name, row)
		}
		if files := w.workFiles(); len(files) != 0 {
			t.Errorf("%s: work files %v", c.name, files)
		}
	}
	if counts, _ := w.scan(); counts != (stores.ScanCounts{}) {
		t.Fatalf("the write store holds %+v", counts)
	}
	// maven-metadata.xml passes ALLOW_ONCE again
	meta := "com/example/demo-lib/maven-metadata.xml"
	once := startIngest(t, map[string]map[string]any{"demo-releases": storage("ALLOW_ONCE")})
	for i := range 2 {
		if _, err := once.ingest("demo-releases", meta, "<metadata/>"); err != nil {
			t.Fatalf("metadata write %d: %v", i, err)
		}
	}
}

// Free space below the floor (507) and a write store this server does not
// write (409).
func TestIngestStoreRefusals(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{
		"demo-raw":      storage("ALLOW"),
		"demo-releases": {"storage": map[string]any{"writePolicy": "ALLOW"}, "write_store": "elsewhere"},
	})
	w.srv.writes.freeSpace = func(string) (uint64, error) { return 1 << 20, nil }
	_, err := w.ingest("demo-raw", "docs/a.txt", "hello")
	var refusal *writeRefusal
	if !errors.As(err, &refusal) || refusal.status != 507 || refusal.rule != "no-space" {
		t.Fatalf("%v", err)
	}
	w.srv.writes.freeSpace = freeBytes
	_, err = w.ingest("demo-releases", "com/example/demo-lib/1.0/demo-lib-1.0.pom", "<project/>")
	if !errors.As(err, &refusal) || refusal.status != 409 || refusal.rule != "write-store" {
		t.Fatalf("%v", err)
	}
	if _, err := w.ingest("demo-raw", "docs/a.txt", "hello"); err != nil {
		t.Fatalf("with the real free space: %v", err)
	}
}

// A write over a path of the write store marks the blob it replaced (W4):
// the store keeps one live blob per path, and a scan agrees with the index.
func TestIngestMarksTheReplacedBlob(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	first, err := w.ingest("demo-raw", "docs/a.txt", "first")
	if err != nil {
		t.Fatal(err)
	}
	w.clock.add(time.Minute)
	second, err := w.ingest("demo-raw", "docs/a.txt", "second")
	if err != nil {
		t.Fatal(err)
	}
	if second.replaced == nil || second.replaced.Location != first.asset.Location {
		t.Fatalf("replaced %+v", second.replaced)
	}
	if props := w.properties(first.asset.Location); props["deleted"] != "true" || props["deletedReason"] == "" {
		t.Fatalf("the replaced blob %q", props)
	}
	counts, live := w.scan()
	if counts.Live != 1 || counts.Deleted != 1 || len(live) != 1 || live[0].Location != second.asset.Location {
		t.Fatalf("scan %+v %+v", counts, live)
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-raw/docs/a.txt", nil, "basic", asAdmin); string(r.body) != "second" {
		t.Fatalf("%d %q", r.status, r.body)
	}
}

// Review Focus 3 of the execution plan: two writes of one path in the same
// millisecond. The later one is served and the earlier blob carries the
// delete mark, although both carry the same creationTime.
func TestIngestTwoWritesInOneMillisecond(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	first, err := w.ingest("demo-raw", "docs/a.txt", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.ingest("demo-raw", "docs/a.txt", "second")
	if err != nil {
		t.Fatal(err)
	}
	if first.asset.CreatedMS != second.asset.CreatedMS {
		t.Fatalf("creation times %d and %d", first.asset.CreatedMS, second.asset.CreatedMS)
	}
	if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/a.txt"); row == nil || row.Location != second.asset.Location {
		t.Fatalf("lookup %+v", row)
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-raw/docs/a.txt", nil, "basic", asAdmin); string(r.body) != "second" {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if props := w.properties(first.asset.Location); props["deleted"] != "true" {
		t.Fatalf("the earlier blob %q", props)
	}
	counts, live := w.scan()
	if counts.Live != 1 || counts.Deleted != 1 || live[0].Location != second.asset.Location {
		t.Fatalf("scan %+v %+v", counts, live)
	}
}

// A path that a store of the old product holds with a newer creation time
// than the clock gives: the new blob gets a later one, so it is served, and
// the old store stays as it is.
func TestIngestOverARowOfAnOldStore(t *testing.T) {
	future := spec("demo-raw", "docs/readme.txt", []byte("old"), "text/plain")
	future.CreatedMS = 4_102_444_800_000
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")}, future)
	before, err := testkit.TreeDigest(w.old)
	if err != nil {
		t.Fatal(err)
	}
	result, err := w.ingest("demo-raw", "docs/readme.txt", "new")
	if err != nil {
		t.Fatal(err)
	}
	if result.replaced != nil || result.asset.CreatedMS != future.CreatedMS+1 {
		t.Fatalf("%+v", result)
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-raw/docs/readme.txt", nil, "basic", asAdmin); string(r.body) != "new" {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if after, _ := testkit.TreeDigest(w.old); after != before {
		t.Fatal("the store of the old product changed")
	}
}

// The second write of such a path: the row it replaces lies in the write
// store, and the new blob must still pass the row of the old store, else the
// old bytes come back. With the clock behind both rows it also passes the
// first write (finding S5 of review 4), so the scan of the write store
// orders the two by creationTime.
func TestASecondWriteOverARowOfAnOldStore(t *testing.T) {
	future := spec("demo-raw", "docs/readme.txt", []byte("old"), "text/plain")
	future.CreatedMS = 4_102_444_800_000
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")}, future)
	first, err := w.ingest("demo-raw", "docs/readme.txt", "new")
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.ingest("demo-raw", "docs/readme.txt", "newer")
	if err != nil {
		t.Fatal(err)
	}
	if first.asset.CreatedMS != future.CreatedMS+1 || second.asset.CreatedMS != first.asset.CreatedMS+1 || second.replaced == nil ||
		second.replaced.Location != first.asset.Location {
		t.Fatalf("%+v", second)
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-raw/docs/readme.txt", nil, "basic", asAdmin); string(r.body) != "newer" {
		t.Fatalf("%d %q", r.status, r.body)
	}
	w.scanWrites()
	if r := do(t, w.addr(), "GET", "/repository/demo-raw/docs/readme.txt", nil, "basic", asAdmin); string(r.body) != "newer" {
		t.Fatalf("after a scan of the write store: %d %q", r.status, r.body)
	}
}

// A delete marks the blob, removes the row and calls the catalog hook; a
// path a store of the old product holds is refused with 409 and nothing
// changes.
func TestErase(t *testing.T) {
	old := spec("demo-raw", "docs/readme.txt", []byte("readme"), "text/plain")
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")}, old)
	written, err := w.ingest("demo-raw", "docs/new.txt", "new")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := w.srv.erase(w.repo("demo-raw"), "docs/new.txt")
	if err != nil || removed == nil || removed.Location != written.asset.Location {
		t.Fatalf("%+v %v", removed, err)
	}
	if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/new.txt"); row != nil {
		t.Fatalf("row %+v", row)
	}
	if props := w.properties(written.asset.Location); props["deleted"] != "true" {
		t.Fatalf("%q", props)
	}
	if len(w.catalog.deletes) != 1 || w.catalog.deletes[0] != "demo-raw/docs/new.txt" {
		t.Fatalf("catalog %q", w.catalog.deletes)
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-raw/docs/new.txt", nil, "basic", asAdmin); r.status != 404 {
		t.Fatalf("%d", r.status)
	}
	if removed, err := w.srv.erase(w.repo("demo-raw"), "docs/never.txt"); removed != nil || err != nil {
		t.Fatalf("%+v %v", removed, err)
	}
	before, _ := testkit.TreeDigest(w.old)
	for _, p := range []string{"docs/readme.txt"} {
		_, err := w.srv.erase(w.repo("demo-raw"), p)
		var refusal *writeRefusal
		if !errors.As(err, &refusal) || refusal.status != 409 || refusal.rule != "read-only-store" || !strings.Contains(refusal.message, "store-a") {
			t.Fatalf("%s: %v", p, err)
		}
	}
	// a newer blob of the write store over the path changes nothing: the
	// old store still holds it
	if _, err := w.ingest("demo-raw", "docs/readme.txt", "newer"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.erase(w.repo("demo-raw"), "docs/readme.txt"); err == nil {
		t.Fatal("a delete of a path the old store holds")
	}
	if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/readme.txt"); row == nil || row.Store != "local-write" {
		t.Fatalf("%+v", row)
	}
	if after, _ := testkit.TreeDigest(w.old); after != before {
		t.Fatal("the store of the old product changed")
	}
}

// 20 writes of one new path at once under ALLOW_ONCE: the lock of
// repository and path lets exactly one through.
func TestIngestParallelWritesOfOnePath(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW_ONCE")})
	repo := w.repo("demo-raw")
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf("body %d", i)
			_, err := w.srv.ingest(ingestRequest{repo: repo, path: "docs/once.txt", body: strings.NewReader(body), size: int64(len(body)),
				subject: subject("demo-admin", "", "nexus:*"), address: "192.0.2.7"})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok, refused := 0, 0
	for err := range results {
		var refusal *writeRefusal
		switch {
		case err == nil:
			ok++
		case errors.As(err, &refusal) && refusal.rule == "redeploy":
			refused++
		default:
			t.Errorf("%v", err)
		}
	}
	if ok != 1 || refused != 19 {
		t.Fatalf("%d written, %d refused", ok, refused)
	}
	if counts, _ := w.scan(); counts.Live != 1 || counts.Deleted != 0 {
		t.Fatalf("scan %+v", counts)
	}
	if files := w.workFiles(); len(files) != 0 {
		t.Fatalf("work files %v", files)
	}
}
