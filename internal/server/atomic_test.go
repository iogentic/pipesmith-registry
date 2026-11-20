package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/scan"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The writes of several blobs in one request (note N5 of the 0.2.0 plan).

// holdIndexFile takes the write lock of index.sqlite on a connection of its
// own, as a scan of another process does; release gives it back.
func holdIndexFile(t *testing.T, w *writing) (release func()) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: w.cfg.IndexPath()}).EscapedPath()+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK") // a test connection
			_ = conn.Close()
			_ = db.Close()
		})
	}
}

// holdingCatalog passes every call to its hook, and after the first Put
// (or Delete) of path, any path when it is empty, it takes the write lock of the index file; it gives
// the lock back after hold, or when the test calls free.
type holdingCatalog struct {
	CatalogHook
	t        *testing.T
	w        *writing
	path     string
	onDelete bool
	hold     time.Duration // zero: until free
	once     sync.Once
	mu       sync.Mutex
	release  func()
	taken    chan struct{}
}

func (w *writing) holdAfter(path string, onDelete bool, hold time.Duration) *holdingCatalog {
	c := &holdingCatalog{CatalogHook: w.srv.writes.catalog, t: w.t, w: w, path: path, onDelete: onDelete, hold: hold, taken: make(chan struct{})}
	w.srv.writes.catalog = c
	w.t.Cleanup(c.free)
	return c
}

func (c *holdingCatalog) take() {
	c.once.Do(func() {
		release := holdIndexFile(c.t, c.w)
		c.mu.Lock()
		c.release = release
		c.mu.Unlock()
		close(c.taken)
		if c.hold > 0 {
			time.AfterFunc(c.hold, c.free)
		}
	})
}

func (c *holdingCatalog) free() {
	c.mu.Lock()
	release := c.release
	c.release = nil
	c.mu.Unlock()
	if release != nil {
		release()
	}
}

func (c *holdingCatalog) Put(asset index.Asset, digests stores.Digests, uploader, address string) error {
	err := c.CatalogHook.Put(asset, digests, uploader, address)
	if !c.onDelete && asset.Path == c.path {
		c.take()
	}
	return err
}

func (c *holdingCatalog) Delete(repo, path string) error {
	err := c.CatalogHook.Delete(repo, path)
	if c.onDelete && (c.path == "" || path == c.path) {
		c.take()
	}
	return err
}

// liveBlobs are the paths of the live blobs of the write store.
func (w *writing) liveBlobs() map[string]bool {
	w.t.Helper()
	_, live := w.scan()
	out := map[string]bool{}
	for _, e := range live {
		out[e.Path] = true
	}
	return out
}

// N5 (a): a REST upload of several files whose second blob gets 503 takes
// back the first one it wrote: the first path of a new blob gets its delete
// mark, so neither the read path nor a scan serves it. The same upload
// again succeeds. When the index stays busy past the rollback too, the
// first blob stays live and a warning names it.
func TestARefusedUploadTakesBackItsFirstBlob(t *testing.T) {
	indexWait.Store(int64(400 * time.Millisecond))
	t.Cleanup(func() { indexWait.Store(0) })
	two := []formPart{formField("raw.directory", "up"), formFile("raw.asset1", "a", []byte("alpha")), formField("raw.asset1.filename", "a.txt"),
		formFile("raw.asset2", "b", []byte("beta")), formField("raw.asset2.filename", "b.txt")}

	t.Run("the index is free for the rollback", func(t *testing.T) {
		w := startREST(t, restAttributes())
		w.holdAfter("up/a.txt", false, 650*time.Millisecond)
		r := w.upload("demo-raw", asAdmin, two...)
		if r.status != http.StatusServiceUnavailable || r.header.Get("Retry-After") == "" {
			t.Fatalf("the upload with a busy index: %d %v %q", r.status, r.header, r.body)
		}
		for _, path := range []string{"up/a.txt", "up/b.txt"} {
			if got := w.fetch("demo-raw", path); got.status != http.StatusNotFound {
				t.Errorf("%s of the refused upload is served: %d %q", path, got.status, got.body)
			}
		}
		if live := w.liveBlobs(); len(live) != 0 {
			t.Errorf("the refused upload left live blobs %v", live)
		}
		status(t, w.upload("demo-raw", asAdmin, two...), 204)
		if a, b := w.fetch("demo-raw", "up/a.txt"), w.fetch("demo-raw", "up/b.txt"); string(a.body) != "alpha" || string(b.body) != "beta" {
			t.Fatalf("the upload again: %d %q, %d %q", a.status, a.body, b.status, b.body)
		}
	})

	t.Run("the index stays busy", func(t *testing.T) {
		w := startREST(t, restAttributes())
		logs := captureLog(t)
		hold := w.holdAfter("up/a.txt", false, 0)
		r := w.upload("demo-raw", asAdmin, two...)
		hold.free()
		if r.status != http.StatusServiceUnavailable {
			t.Fatalf("the upload with a busy index: %d %q", r.status, r.body)
		}
		if live := w.liveBlobs(); !live["up/a.txt"] || live["up/b.txt"] {
			t.Errorf("live blobs %v, want up/a.txt alone", live)
		}
		found := false
		for _, line := range logs.all() {
			if strings.Contains(line, "up/a.txt") && strings.Contains(line, "stays live") {
				found = true
			}
		}
		if !found {
			t.Errorf("no warning names the blob that stays: %q", logs.all())
		}
	})
}

// N5 (b): an npm publish is the tarball and then the document. A 503 at
// the document takes back the tarball of a new version, so no version can
// be built from it; the publish again succeeds. The rewrite of an unpublish
// marks the tarballs it drops and then writes the document; a 503 at the
// document leaves the tarballs marked and the stored document as it was:
// the read path no longer lists a version without its tarball, and the
// same rewrite again under the same revision finishes the document.
func TestANpmPublishAndRewriteWithABusyIndex(t *testing.T) {
	indexWait.Store(int64(400 * time.Millisecond))
	t.Cleanup(func() { indexWait.Store(0) })
	const tarball = "demo-pkg/-/demo-pkg-1.0.0.tgz"
	publish := func(w *writing, version string) *answer {
		data := npmTarballOf(t, "package "+version)
		return do(t, w.addr(), "PUT", "/repository/demo-npm/demo-pkg", npmPublishBody("demo-pkg", version, data), "basic", asAdmin)
	}

	t.Run("publish", func(t *testing.T) {
		w := startOCI(t, nil)
		w.holdAfter(tarball, false, 650*time.Millisecond)
		if r := publish(w, "1.0.0"); r.status != http.StatusServiceUnavailable {
			t.Fatalf("the publish with a busy index: %d %q", r.status, r.body)
		}
		if live := w.liveBlobs(); len(live) != 0 {
			t.Errorf("the refused publish left live blobs %v", live)
		}
		if r := w.fetch("demo-npm", tarball); r.status != http.StatusNotFound {
			t.Errorf("the tarball of the refused publish is served: %d", r.status)
		}
		if r := publish(w, "1.0.0"); r.status != http.StatusCreated {
			t.Fatalf("the publish again: %d %q", r.status, r.body)
		}
		if live := w.liveBlobs(); !live[tarball] || !live["demo-pkg"] {
			t.Fatalf("live blobs after the publish again %v", live)
		}
	})

	t.Run("rewrite", func(t *testing.T) {
		w := startOCI(t, nil)
		for _, v := range []string{"1.0.0", "1.1.0"} {
			if r := publish(w, v); r.status != http.StatusCreated {
				t.Fatalf("publish %s: %d %q", v, r.status, r.body)
			}
		}
		served := do(t, w.addr(), "GET", "/repository/demo-npm/demo-pkg", nil, "basic", asAdmin).object(t)
		rev := served["_rev"].(string)
		delete(served["versions"].(map[string]any), "1.0.0")
		body, _ := json.Marshal(served)
		hold := w.holdAfter(tarball, true, 0)
		r := do(t, w.addr(), "PUT", "/repository/demo-npm/demo-pkg/-rev/"+rev, body, "basic", asAdmin)
		hold.free()
		if r.status != http.StatusServiceUnavailable {
			t.Fatalf("the rewrite with a busy index: %d %q", r.status, r.body)
		}
		if live := w.liveBlobs(); live[tarball] || !live["demo-pkg/-/demo-pkg-1.1.0.tgz"] {
			t.Errorf("live blobs after the refused rewrite %v", live)
		}
		now := do(t, w.addr(), "GET", "/repository/demo-npm/demo-pkg", nil, "basic", asAdmin).object(t)
		if versions := now["versions"].(map[string]any); len(versions) != 1 || versions["1.1.0"] == nil || now["_rev"] != rev {
			t.Errorf("the document after the refused rewrite: %v", now)
		}
		if r := do(t, w.addr(), "PUT", "/repository/demo-npm/demo-pkg/-rev/"+rev, body, "basic", asAdmin); r.status != http.StatusOK {
			t.Fatalf("the rewrite again: %d %q", r.status, r.body)
		}
		now = do(t, w.addr(), "GET", "/repository/demo-npm/demo-pkg", nil, "basic", asAdmin).object(t)
		if versions := now["versions"].(map[string]any); len(versions) != 1 || versions["1.1.0"] == nil || now["_rev"] == rev {
			t.Errorf("the document after the rewrite again: %v", now)
		}
	})
}

// N5 (c): a delete of several paths (a REST delete of a component, an npm
// unpublish) erases path by path and stops at the first that fails: the
// paths before it are deleted, the rest stays, the answer is the failure,
// and a warning names where it stopped. The same delete again finishes it.
func TestADeleteOfSeveralPathsStopsAndFinishesAgain(t *testing.T) {
	indexWait.Store(int64(400 * time.Millisecond))
	t.Cleanup(func() { indexWait.Store(0) })
	w := startREST(t, restAttributes())
	status(t, w.upload("demo-releases", asAdmin, formField("maven2.groupId", "org.demo"), formField("maven2.artifactId", "lib"),
		formField("maven2.version", "1.0"), formField("maven2.generate-pom", "true"), formFile("maven2.asset1", "lib.jar", []byte("jar")),
		formField("maven2.asset1.extension", "jar")), 204)
	id := w.searchComponent("demo-releases", "lib", "1.0")
	component := []string{"org/demo/lib/1.0/lib-1.0.jar", "org/demo/lib/1.0/lib-1.0.jar.md5", "org/demo/lib/1.0/lib-1.0.jar.sha1",
		"org/demo/lib/1.0/lib-1.0.pom", "org/demo/lib/1.0/lib-1.0.pom.md5", "org/demo/lib/1.0/lib-1.0.pom.sha1"}
	live := w.liveBlobs()
	for _, path := range component {
		if !live[path] {
			t.Fatalf("the upload left no %s: %v", path, live)
		}
	}
	logs := captureLog(t)
	hold := w.holdAfter("", true, 0)
	r := w.deleteREST("/service/rest/v1/components/"+id, asAdmin)
	hold.free()
	if r.status != http.StatusServiceUnavailable {
		t.Fatalf("the delete with a busy index: %d %q", r.status, r.body)
	}
	live = w.liveBlobs()
	gone := 0
	for _, path := range component {
		if !live[path] {
			gone++
		}
	}
	if gone != 1 {
		t.Errorf("the stopped delete took %d of %d paths, want 1: %v", gone, len(component), live)
	}
	found := false
	for _, line := range logs.all() {
		if strings.Contains(line, "demo-releases") && strings.Contains(line, "stopped at") && strings.Contains(line, "1 of 6") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning says where the delete stopped: %q", logs.all())
	}
	status(t, w.deleteREST("/service/rest/v1/components/"+id, asAdmin), 204)
	live = w.liveBlobs()
	for _, path := range component {
		if live[path] {
			t.Errorf("%s is live after the delete again", path)
		}
	}
}

// missingStore is a walk that does not list one path, as a walk that began
// before the blob of the path came does, and that stops after its first
// entry until resume is closed; the scan holds the write lock of the index
// meanwhile.
type missingStore struct {
	stores.Store
	skip           string
	once           sync.Once
	paused, resume chan struct{}
}

func (m *missingStore) Entries(counts *stores.ScanCounts, yield func(stores.Entry) error) error {
	return m.Store.Entries(counts, func(e stores.Entry) error {
		if e.Path == m.skip {
			return nil
		}
		err := yield(e)
		m.once.Do(func() {
			close(m.paused)
			<-m.resume
		})
		return err
	})
}

// N5 (d): a DELETE reads the rows of its path and then waits for the index,
// which a scan of the write store holds. When the scan drops the row (its
// walk did not list the blob), the delete still sets the mark under the
// lock of the index; it answers 204 from the mark it set, not 404 from the
// row the scan took away.
func TestADeleteThatRacesAScanAnswersFromItsMark(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	w.clock.add(-24 * time.Hour)
	if _, err := w.ingest("demo-raw", "docs/early.txt", "early"); err != nil {
		t.Fatal(err)
	}
	w.clock.add(24 * time.Hour)
	target, err := w.ingest("demo-raw", "docs/x.txt", "x")
	if err != nil {
		t.Fatal(err)
	}
	st, err := w.srv.rp.sync()
	if err != nil {
		t.Fatal(err)
	}
	walk := &missingStore{Store: st.stores["local-write"], skip: "docs/x.txt", paused: make(chan struct{}), resume: make(chan struct{})}
	var resumed sync.Once
	resume := func() { resumed.Do(func() { close(walk.resume) }) }
	t.Cleanup(resume)
	scanned := make(chan error, 1)
	go func() {
		_, err := scan.Run(walk, w.srv.rp.index, nil)
		scanned <- err
	}()
	<-walk.paused
	answered := make(chan *answer, 1)
	go func() {
		answered <- do(t, w.addr(), "DELETE", "/repository/demo-raw/docs/x.txt", nil, "basic", asAdmin)
	}()
	time.Sleep(300 * time.Millisecond) // the delete has read the row and waits for the index
	if len(answered) != 0 {
		t.Fatal("the delete ended while the scan held the index")
	}
	resume()
	if err := <-scanned; err != nil {
		t.Fatal(err)
	}
	r := <-answered
	if r.status != http.StatusNoContent {
		t.Errorf("the delete that raced the scan: %d %q", r.status, r.body)
	}
	if props := w.properties(target.asset.Location); props["deleted"] != "true" {
		t.Errorf("the blob of the delete has no mark: %q", props)
	}
	if live := w.liveBlobs(); live["docs/x.txt"] || !live["docs/early.txt"] {
		t.Errorf("live blobs %v", live)
	}
	if got := w.fetch("demo-raw", "docs/x.txt"); got.status != http.StatusNotFound {
		t.Errorf("the deleted path is served: %d", got.status)
	}
}

// pausingCatalog stops the first Put of path until the test closes goOn.
type pausingCatalog struct {
	CatalogHook
	path          string
	once          sync.Once
	reached, goOn chan struct{}
}

func (c *pausingCatalog) Put(asset index.Asset, digests stores.Digests, uploader, address string) error {
	err := c.CatalogHook.Put(asset, digests, uploader, address)
	if asset.Path == c.path {
		c.once.Do(func() {
			close(c.reached)
			<-c.goOn
		})
	}
	return err
}

// N5 (e): the metadata rebuild of an artifact reads its files and then
// writes the documents. A deploy that sends maven-metadata.xml meanwhile
// waits until the rebuild is done, so its document is the last one and is
// not overwritten by documents that do not know its version.
func TestAMetadataWriteWaitsForTheRebuild(t *testing.T) {
	w := startREST(t, restAttributes())
	status(t, w.upload("demo-releases", asAdmin, formField("maven2.groupId", "org.demo"), formField("maven2.artifactId", "lib"),
		formField("maven2.version", "1.0"), formField("maven2.generate-pom", "true"), formFile("maven2.asset1", "lib.jar", []byte("jar")),
		formField("maven2.asset1.extension", "jar")), 204)
	status(t, do(t, w.addr(), "PUT", "/repository/demo-releases/org/demo/lib/2.0/lib-2.0.jar", []byte("jar 2"), "basic", asAdmin), 201)
	pause := &pausingCatalog{CatalogHook: w.srv.writes.catalog, path: "org/demo/lib/maven-metadata.xml.md5", reached: make(chan struct{}),
		goOn: make(chan struct{})}
	w.srv.writes.catalog = pause
	var resumed sync.Once
	resume := func() { resumed.Do(func() { close(pause.goOn) }) }
	t.Cleanup(resume)
	rebuilt := make(chan error, 1)
	go func() {
		_, err := w.srv.RebuildMavenMetadata("demo-releases", maven.Scope{GroupID: "org.demo", ArtifactID: "lib"},
			subject("demo-admin", "", "nexus:*"), "192.0.2.7")
		rebuilt <- err
	}()
	<-pause.reached
	client := []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<metadata><groupId>org.demo</groupId><artifactId>lib</artifactId>" +
		"<versioning><latest>2.0</latest><release>2.0</release><versions><version>1.0</version><version>2.0</version><version>3.0</version>" +
		"</versions></versioning></metadata>\n")
	answered := make(chan *answer, 1)
	go func() {
		answered <- do(t, w.addr(), "PUT", "/repository/demo-releases/org/demo/lib/maven-metadata.xml", client, "basic", asAdmin)
	}()
	time.Sleep(300 * time.Millisecond)
	if len(answered) != 0 {
		r := <-answered
		t.Errorf("the PUT of the metadata ended while the rebuild ran: %d %q", r.status, r.body)
		answered <- r
	}
	resume()
	if err := <-rebuilt; err != nil {
		t.Fatal(err)
	}
	if r := <-answered; r.status != http.StatusCreated {
		t.Fatalf("the PUT of the metadata: %d %q", r.status, r.body)
	}
	if got := w.fetch("demo-releases", "org/demo/lib/maven-metadata.xml"); string(got.body) != string(client) {
		t.Errorf("the metadata after both: %d %q", got.status, got.body)
	}
}

// The gate of a rebuild: a rebuild blocks a metadata write within its
// scope, a whole repository scope blocks every one, a write of another
// artifact or repository passes, and writes do not block each other.
func TestTheMetadataGate(t *testing.T) {
	var g metadataGate
	leave := g.enter("demo-releases", "org/demo/lib", true)
	for _, c := range []struct {
		repo, at string
		rebuild  bool
		blocked  bool
	}{
		{"demo-releases", "org/demo/lib/maven-metadata.xml", false, true},
		{"demo-releases", "org/demo/lib/1.0-SNAPSHOT/maven-metadata.xml.sha1", false, true},
		{"demo-releases", "org/demo/library/maven-metadata.xml", false, false},
		{"demo-releases", "org/demo/maven-metadata.xml", false, false},
		{"other", "org/demo/lib/maven-metadata.xml", false, false},
		{"demo-releases", "", true, true},
		{"demo-releases", "org/demo", true, true},
		{"demo-releases", "org/other", true, false},
	} {
		if got := g.blocked(c.repo, c.at, c.rebuild); got != c.blocked {
			t.Errorf("%s %s rebuild %t: blocked %t, want %t", c.repo, c.at, c.rebuild, got, c.blocked)
		}
	}
	leave()
	write := g.enter("demo-releases", "org/demo/lib/maven-metadata.xml", false)
	if !g.blocked("demo-releases", "", true) || g.blocked("demo-releases", "org/demo/lib/maven-metadata.xml.md5", false) {
		t.Error("a running write: a rebuild of the repository must wait, a second write must not")
	}
	write()
	if g.blocked("demo-releases", "", true) {
		t.Error("the gate holds a write that left")
	}
}

// Item f of the N5 work: a REST create that sends the default limits keeps
// no limit as text in the record; the record reads with the defaults.
func TestACreateKeepsNoLimitAsText(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	sec := w.opts.security.(*fakeSecurity)
	var body map[string]any
	_ = json.Unmarshal([]byte(repositoryBody("demo-made", "raw", "hosted")), &body)
	body["max_body_bytes"], body["free_space_floor_bytes"] = 10737418240, 2147483648
	data, _ := json.Marshal(body)
	if r := do(t, w.addr(), "POST", repositoriesPath+"/raw/hosted", data, "basic", asAdmin, "Content-Type", jsonType); r.status != 201 {
		t.Fatalf("create: %d %q", r.status, r.body)
	}
	found := false
	for _, r := range sec.Snapshot().Repositories {
		if r.Name != "demo-made" {
			continue
		}
		found = true
		for _, key := range []string{"max_body_bytes", "free_space_floor_bytes"} {
			if v, held := r.Attributes[key]; held {
				if _, text := v.(string); text {
					t.Errorf("%s is stored as text: %#v", key, v)
				}
			}
		}
	}
	if !found {
		t.Fatal("no record of the create")
	}
	settings := w.repo("demo-made").WriteSettings()
	if settings.MaxBodyBytes != repos.DefaultMaxBodyBytes || settings.FreeSpaceFloor != repos.DefaultFreeSpaceFloor {
		t.Errorf("%+v", settings)
	}
}
