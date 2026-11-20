package server

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// Leg F1b: serve opens catalog.sqlite once and the writes keep it through
// the catalog hook, so the catalog follows every write and delete between
// two scans. A scan of the files then finds the catalog as it is, apart from
// the columns only a request fills: the digests a write computes, the last
// modified and last downloaded times, and the time of the scan.

// startCataloged is a server with writes on and catalog.sqlite of its data
// directory open as serve opens it, with the hosted repositories demo-raw
// and demo-releases under ALLOW. ids, when given, are the blob ids of the
// writes in their order.
func startCataloged(t *testing.T, ids ...string) *writing {
	t.Helper()
	sec := newFakeSecurity()
	snap := *sec.snap
	snap.Repositories = []model.Repository{
		{Name: "demo-raw", Format: "raw", Type: "hosted", Online: true, Attributes: storage("ALLOW")},
		{Name: "demo-releases", Format: "maven2", Type: "hosted", Online: true, Attributes: storage("ALLOW")},
	}
	sec.snap = &snap
	base := t.TempDir()
	storeCfgs := []config.StoreConfig{blobStore(t, base, "store-a")}
	w := &writing{clock: &clock{now: time.Date(2026, 11, 18, 10, 15, 30, 123_000_000, time.UTC)}, old: storeCfgs[0].Root}
	options := blobwrite.Options{Now: w.clock.Now}
	if len(ids) > 0 {
		next := 0
		options.NewID = func() string {
			id := ids[next%len(ids)]
			next++
			return id
		}
	}
	w.opts = appOptions{defs: []*pyjson.Object{def("demo-raw", "raw", "hosted"), def("demo-releases", "maven2", "hosted")},
		security: sec, writes: true, writeOptions: options}
	cfg := prepare(t, base, storeCfgs, w.opts)
	file, err := catalog.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	w.opts.catalogFile = file
	w.app = startApp(t, base, cfg, w.opts)
	return w
}

// catalogRows are the rows of catalog.sqlite without the columns only a
// request fills, as lines, and the component of an asset by its
// coordinates rather than its id.
func (w *writing) catalogRows() []string {
	w.t.Helper()
	db, err := sql.Open("sqlite", "file:"+catalog.Path(w.cfg.DataDir)+"?mode=ro")
	if err != nil {
		w.t.Fatal(err)
	}
	defer db.Close()
	var lines []string
	for _, query := range []string{
		"SELECT 'blob', store, repo, path, size, coalesce(sha1, '-'), coalesce(content_type, '-'), created_ms," +
			" coalesce(uploader, '-'), coalesce(uploader_ip, '-') FROM blobs ORDER BY store, repo, path",
		"SELECT 'asset', a.repo, a.path, a.store, a.format, coalesce(c.namespace, '-'), coalesce(c.name, '-')," +
			" coalesce(c.version, '-'), coalesce(a.extension, '-'), coalesce(a.classifier, '-')" +
			" FROM assets a LEFT JOIN components c ON c.id = a.component ORDER BY a.repo, a.path",
		"SELECT 'component', repo, format, namespace, name, version, prerelease FROM components" +
			" ORDER BY repo, format, namespace, name, version",
		"SELECT 'scan', store, blobs, bytes FROM scans ORDER BY store",
	} {
		rows, err := db.Query(query)
		if err != nil {
			w.t.Fatal(err)
		}
		columns, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				w.t.Fatal(err)
			}
			lines = append(lines, strings.TrimSuffix(fmt.Sprintln(values...), "\n"))
		}
		if err := rows.Close(); err != nil {
			w.t.Fatal(err)
		}
	}
	return lines
}

// expectAsScanned checks that a scan of the write store leaves the catalog
// as the writes left it.
func (w *writing) expectAsScanned() {
	w.t.Helper()
	live := w.catalogRows()
	w.scanWrites()
	if scanned := w.catalogRows(); strings.Join(live, "\n") != strings.Join(scanned, "\n") {
		w.t.Fatalf("the catalog the writes kept:\n%s\nthe catalog a scan of the files fills:\n%s",
			strings.Join(live, "\n"), strings.Join(scanned, "\n"))
	}
}

func hasLine(lines []string, prefix string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func TestTheCatalogFollowsWritesAsAScanFillsIt(t *testing.T) {
	w := startCataloged(t)
	for _, write := range [][3]string{
		{"demo-raw", "docs/a.txt", "one"},
		{"demo-raw", "docs/b.txt", "bee"},
		{"demo-releases", "com/example/lib/1.0/lib-1.0.pom", "<project/>"},
		{"demo-releases", "com/example/lib/1.0/lib-1.0.jar", "jar bytes"},
		{"demo-raw", "docs/a.txt", "two, a second blob"},
	} {
		w.clock.add(time.Second)
		if _, err := w.ingest(write[0], write[1], write[2]); err != nil {
			t.Fatalf("%v: %v", write, err)
		}
	}
	w.clock.add(time.Second)
	if removed, err := w.srv.erase(w.repo("demo-raw"), "docs/b.txt"); err != nil || removed == nil {
		t.Fatalf("%+v %v", removed, err)
	}
	live := w.catalogRows()
	for _, want := range []string{
		"blob local-write demo-raw docs/a.txt 18 ",
		"asset demo-raw docs/a.txt local-write raw",
		"asset demo-releases com/example/lib/1.0/lib-1.0.jar local-write maven2 com.example lib 1.0 jar",
		"component demo-releases maven2 com.example lib 1.0 0",
		"scan local-write 3 ",
	} {
		if !hasLine(live, want) {
			t.Fatalf("no row %q after the writes:\n%s", want, strings.Join(live, "\n"))
		}
	}
	if hasLine(live, "blob local-write demo-raw docs/b.txt") || hasLine(live, "asset demo-raw docs/b.txt") {
		t.Fatalf("the deleted path keeps its rows:\n%s", strings.Join(live, "\n"))
	}
	for _, line := range live {
		if strings.HasPrefix(line, "blob local-write") && !strings.HasSuffix(line, " demo-admin 192.0.2.7") {
			t.Fatalf("a blob of a write without its uploader and address: %q", line)
		}
	}
	w.expectAsScanned()
}

// recordedHook is the catalog hook of a server with the errors it returned.
type recordedHook struct {
	CatalogHook
	mu   sync.Mutex
	errs []error
}

func (r *recordedHook) note(err error) error {
	if err != nil {
		r.mu.Lock()
		r.errs = append(r.errs, err)
		r.mu.Unlock()
	}
	return err
}

func (r *recordedHook) Put(asset index.Asset, digests stores.Digests, uploader, address string) error {
	return r.note(r.CatalogHook.Put(asset, digests, uploader, address))
}

func (r *recordedHook) Delete(repo, path string) error {
	return r.note(r.CatalogHook.Delete(repo, path))
}

// The scan job and the hook of the writes write catalog.sqlite in one
// process, one after the other: no write of the hook fails while scans run,
// and the catalog ends as a scan of the files fills it.
func TestTheScanJobAndTheHookWriteTheCatalogTogether(t *testing.T) {
	w := startCataloged(t)
	if w.srv.writes.catalog == nil {
		t.Fatal("serve with a catalog file sets no catalog hook")
	}
	hook := &recordedHook{CatalogHook: w.srv.writes.catalog}
	w.srv.writes.catalog = hook
	st, err := w.srv.rp.sync()
	if err != nil {
		t.Fatal(err)
	}
	store := st.stores["local-write"]
	const writes = 40
	done := make(chan struct{})
	scans := 0
	go func() {
		defer close(done)
		for i := range writes {
			w.clock.add(time.Millisecond)
			if _, err := w.ingest("demo-raw", fmt.Sprintf("docs/%02d.txt", i), fmt.Sprintf("body %d", i)); err != nil {
				hook.note(err)
			}
			if i%10 == 9 {
				if _, err := w.srv.erase(w.repo("demo-raw"), fmt.Sprintf("docs/%02d.txt", i-5)); err != nil {
					hook.note(err)
				}
			}
		}
	}()
	for busy := true; busy; {
		select {
		case <-done:
			busy = false
		default:
			if w.srv.job.start(store) {
				scans++
			}
			time.Sleep(time.Millisecond)
		}
	}
	for deadline := time.Now().Add(30 * time.Second); running(w.srv.job.snapshot()); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the scan did not end")
		}
	}
	if len(hook.errs) > 0 {
		t.Fatalf("%d writes failed beside %d scans, the first: %v", len(hook.errs), scans, hook.errs[0])
	}
	if scans == 0 {
		t.Fatal("no scan ran beside the writes")
	}
	live := w.catalogRows()
	if !hasLine(live, "scan local-write 36 ") {
		t.Fatalf("40 writes and 4 deletes beside %d scans:\n%s", scans, strings.Join(live, "\n"))
	}
	w.expectAsScanned()
}
