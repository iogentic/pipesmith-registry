package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobprops"
	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// createdStoreRepo creates over REST the store store and the hosted raw
// repository repo that writes into it, the store in the same request when
// inline is set (SR5, SR6), and gives the root of the store.
func createdStoreRepo(t *testing.T, w *writing, store, repo string, inline bool) string {
	t.Helper()
	body := `{"name":"` + repo + `","online":true,"storage":{"blobStoreName":"` + store + `","writePolicy":"ALLOW"`
	if inline {
		body += `,"newBlobStore":{}`
	} else if r := do(t, w.addr(), "POST", "/service/rest/v1/blobstores/file", []byte(`{"name":"`+store+`"}`), "basic", asAdmin,
		"Content-Type", jsonType); r.status != 204 {
		t.Fatalf("store create: %d %q", r.status, r.body)
	}
	body += `}}`
	if r := do(t, w.addr(), "POST", repositoriesPath+"/raw/hosted", []byte(body), "basic", asAdmin, "Content-Type", jsonType); r.status != 201 {
		t.Fatalf("repository create: %d %q", r.status, r.body)
	}
	return filepath.Join(w.cfg.DataDir, "stores", store)
}

// propertiesIn are the properties of the blob at location of the store at
// root.
func propertiesIn(t *testing.T, root, location string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, strings.TrimSuffix(location, ".bytes")+".properties"))
	if err != nil {
		t.Fatal(err)
	}
	items, err := blobprops.Load(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// Rulings SR1 to SR3: an upload into a hosted repository lands in the
// store it names, the row names that store, a read serves it, a replace and
// a delete mark the blob in that store, and nothing lands in local-write.
func TestAHostedRepositoryWritesIntoItsStore(t *testing.T) {
	for _, inline := range []bool{false, true} {
		name := map[bool]string{false: "picked", true: "made in the create"}[inline]
		t.Run(name, func(t *testing.T) {
			w := startIngest(t, nil)
			root := createdStoreRepo(t, w, "demo-team", "demo-t", inline)
			if r := do(t, w.addr(), "PUT", "/repository/demo-t/docs/a.txt", []byte("one"), "basic", asAdmin); r.status != 201 {
				t.Fatalf("PUT: %d %q", r.status, r.body)
			}
			rows, err := w.srv.rp.index.Rows("demo-t", "docs/a.txt")
			if err != nil || len(rows) != 1 || rows[0].Store != "demo-team" {
				t.Fatalf("rows %+v %v", rows, err)
			}
			first := rows[0]
			if props := propertiesIn(t, root, first.Location); props["@BlobStore.blob-name"] != "/docs/a.txt" {
				t.Fatalf("the blob in demo-team: %v", props)
			}
			if r := do(t, w.addr(), "GET", "/repository/demo-t/docs/a.txt", nil, "basic", asAdmin); r.status != 200 || string(r.body) != "one" {
				t.Fatalf("GET: %d %q", r.status, r.body)
			}
			if r := do(t, w.addr(), "PUT", "/repository/demo-t/docs/a.txt", []byte("two"), "basic", asAdmin); r.status != 201 {
				t.Fatalf("second PUT: %d %q", r.status, r.body)
			}
			if props := propertiesIn(t, root, first.Location); props["deleted"] != "true" {
				t.Fatalf("the replaced blob has no mark in demo-team: %v", props)
			}
			rows, _ = w.srv.rp.index.Rows("demo-t", "docs/a.txt")
			if r := do(t, w.addr(), "DELETE", "/repository/demo-t/docs/a.txt", nil, "basic", asAdmin); r.status != 204 {
				t.Fatalf("DELETE: %d %q", r.status, r.body)
			}
			if props := propertiesIn(t, root, rows[0].Location); props["deleted"] != "true" {
				t.Fatalf("the deleted blob has no mark in demo-team: %v", props)
			}
			if _, live := w.scan(); len(live) != 0 {
				t.Fatalf("local-write holds %d live blobs", len(live))
			}
		})
	}
}

// Ruling SR3: a restart opens the created store again, serves its content
// and scans it like local-write; with writes off it is attached read only.
func TestACreatedStoreServesAfterARestart(t *testing.T) {
	w := startIngest(t, nil)
	createdStoreRepo(t, w, "demo-team", "demo-t", true)
	if r := do(t, w.addr(), "PUT", "/repository/demo-t/docs/a.txt", []byte("kept"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("PUT: %d %q", r.status, r.body)
	}
	for _, writes := range []bool{true, false} {
		w.restart(writes)
		if r := do(t, w.addr(), "GET", "/repository/demo-t/docs/a.txt", nil, "basic", asAdmin); r.status != 200 || string(r.body) != "kept" {
			t.Fatalf("writes %v: GET %d %q", writes, r.status, r.body)
		}
	}
	w.restart(true)
	if r := do(t, w.addr(), "PUT", "/repository/demo-t/docs/b.txt", []byte("after"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("PUT after the restart: %d %q", r.status, r.body)
	}
	if rows, _ := w.srv.rp.index.Rows("demo-t", "docs/b.txt"); len(rows) != 1 || rows[0].Store != "demo-team" {
		t.Fatalf("rows %+v", rows)
	}
}

// Rulings SR2 and SR5: write_store of an imported store, or of a store
// without a record, is refused at the write with 409 and nothing is
// written: an imported store is never written.
func TestAnImportedStoreTakesNoWrite(t *testing.T) {
	w := startIngest(t, nil)
	sec := w.opts.security.(*fakeSecurity)
	snap := *sec.Snapshot()
	snap.BlobStores = append(snap.BlobStores, model.BlobStore{Name: "store-a", Type: "File", Path: w.old})
	for _, target := range []string{"store-a", "demo-none"} {
		snap.Repositories = append(snap.Repositories, model.Repository{Name: "demo-" + target, Format: "raw", Type: "hosted", Online: true,
			Attributes: map[string]any{"storage": map[string]any{"blobStoreName": target, "writePolicy": "ALLOW"}, "write_store": target}})
	}
	sec.snap = &snap
	before := treeOf(t, w.old)
	for _, target := range []string{"store-a", "demo-none"} {
		_, err := w.ingest("demo-"+target, "docs/a.txt", "refused")
		var refusal *writeRefusal
		if !errors.As(err, &refusal) || refusal.status != 409 || refusal.rule != ruleWriteStore {
			t.Fatalf("%s: %v", target, err)
		}
	}
	if after := treeOf(t, w.old); after != before {
		t.Fatalf("the imported store changed:\n%s\n%s", before, after)
	}
}

// treeOf lists the files and folders below root.
func treeOf(t *testing.T, root string) string {
	t.Helper()
	var list []string
	err := filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		list = append(list, p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(list, "\n")
}

// Ruling SR3: the writable status asks every open write store; a created
// store whose folder takes no file makes it 503.
func TestTheWritableStatusAsksEveryStore(t *testing.T) {
	w := startIngest(t, nil)
	createdStoreRepo(t, w, "demo-team", "demo-t", true)
	if r := do(t, w.addr(), "PUT", "/repository/demo-t/docs/a.txt", []byte("one"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("PUT: %d %q", r.status, r.body)
	}
	team := filepath.Join(w.cfg.DataDir, "stores", "demo-team")
	refuse := func(path string, _ uint32) error {
		if strings.HasPrefix(path, team) {
			return os.ErrPermission
		}
		return nil
	}
	if !w.srv.writes.writableBy(func(string, uint32) error { return nil }) {
		t.Fatal("not writable with every folder open")
	}
	if w.srv.writes.writableBy(refuse) {
		t.Fatal("writable while demo-team takes no file")
	}
}

// Ruling SR8: the delete of a created store that no repository names any
// more closes its writer; the folder stays.
func TestADeletedStoreClosesItsWriter(t *testing.T) {
	w := startIngest(t, nil)
	root := createdStoreRepo(t, w, "demo-team", "demo-t", true)
	if r := do(t, w.addr(), "PUT", "/repository/demo-t/docs/a.txt", []byte("one"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("PUT: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "DELETE", "/service/rest/v1/blobstores/demo-team", nil, "basic", asAdmin); r.status != 409 {
		t.Fatalf("delete of a store in use: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "DELETE", repositoriesPath+"/demo-t", nil, "basic", asAdmin); r.status != 204 {
		t.Fatalf("repository delete: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "DELETE", "/service/rest/v1/blobstores/demo-team", nil, "basic", asAdmin); r.status != 204 {
		t.Fatalf("store delete: %d %q", r.status, r.body)
	}
	if w.srv.writes.isWriteStore("demo-team") {
		t.Fatal("the writer of the deleted store is open")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("the folder went: %v", err)
	}
}

// Ruling SR3: the mark of a replaced blob of a created store that fails
// waits with its store and is set there by the next try.
func TestAWaitingMarkKeepsItsStore(t *testing.T) {
	w := startIngest(t, nil)
	root := createdStoreRepo(t, w, "demo-team", "demo-t", true)
	if r := do(t, w.addr(), "PUT", "/repository/demo-t/docs/a.txt", []byte("one"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("PUT: %d %q", r.status, r.body)
	}
	rows, _ := w.srv.rp.index.Rows("demo-t", "docs/a.txt")
	w.srv.writes.failMarks(func(string, string) error { return errors.New("demo fault") })
	if r := do(t, w.addr(), "PUT", "/repository/demo-t/docs/a.txt", []byte("two"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("second PUT: %d %q", r.status, r.body)
	}
	if n := w.srv.writes.waiting(); n != 1 {
		t.Fatalf("%d marks wait", n)
	}
	w.srv.writes.failMarks(nil)
	if n := w.srv.writes.retryAll(); n != 0 {
		t.Fatalf("%d marks wait after the retry", n)
	}
	if props := propertiesIn(t, root, rows[0].Location); props["deleted"] != "true" {
		t.Fatalf("the replaced blob has no mark in demo-team: %v", props)
	}
}

// Review R2: the delete of a created store drops the marks that wait in it,
// so that the writable status does not answer 503 until a restart.
func TestADeletedStoreDropsItsWaitingMarks(t *testing.T) {
	w := startIngest(t, nil)
	createdStoreRepo(t, w, "demo-team", "demo-t", true)
	if r := do(t, w.addr(), "PUT", "/repository/demo-t/docs/a.txt", []byte("one"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("PUT: %d %q", r.status, r.body)
	}
	rows, _ := w.srv.rp.index.Rows("demo-t", "docs/a.txt")
	first := rows[0].Location
	w.srv.writes.failMarks(func(location, _ string) error {
		if location == first {
			return errors.New("demo fault")
		}
		return nil
	})
	if r := do(t, w.addr(), "PUT", "/repository/demo-t/docs/a.txt", []byte("two"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("second PUT: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "DELETE", repositoriesPath+"/demo-t", nil, "basic", asAdmin); r.status != 204 {
		t.Fatalf("repository delete: %d %q", r.status, r.body)
	}
	if n := w.srv.writes.waiting(); n != 1 {
		t.Fatalf("%d marks wait before the store delete", n)
	}
	if r := do(t, w.addr(), "DELETE", "/service/rest/v1/blobstores/demo-team", nil, "basic", asAdmin); r.status != 204 {
		t.Fatalf("store delete: %d %q", r.status, r.body)
	}
	if n := w.srv.writes.waiting(); n != 0 {
		t.Fatalf("%d marks of the deleted store still wait", n)
	}
}

// Review R3, R4: a Docker push into a hosted repository of a created store
// keeps the work file of its session in that store, and the blobs, the
// manifest and its tag, a link of the manifest blob, land there.
func TestADockerPushIntoACreatedStore(t *testing.T) {
	w := startIngest(t, nil)
	if r := do(t, w.addr(), "POST", "/service/rest/v1/blobstores/file", []byte(`{"name":"demo-team"}`), "basic", asAdmin,
		"Content-Type", jsonType); r.status != 204 {
		t.Fatalf("store create: %d %q", r.status, r.body)
	}
	body := strings.Replace(repositoryBody("demo-dk", "docker", "hosted"), `"blobStoreName":"local-write"`, `"blobStoreName":"demo-team"`, 1)
	if r := do(t, w.addr(), "POST", repositoriesPath+"/docker/hosted", []byte(body), "basic", asAdmin, "Content-Type", jsonType); r.status != 201 {
		t.Fatalf("repository create: %d %q", r.status, r.body)
	}
	team := filepath.Join(w.cfg.DataDir, "stores", "demo-team")
	base := "/repository/demo-dk/v2/demo/app/"
	layer, config := []byte("a layer of the team"), []byte(`{"architecture":"amd64","os":"linux"}`)
	push := func(blob []byte, chunked bool) {
		t.Helper()
		start := do(t, w.addr(), "POST", base+"blobs/uploads/", nil, "basic", asAdmin)
		location := start.header.Get("Location")
		if start.status != http.StatusAccepted || location == "" {
			t.Fatalf("start: %d %v", start.status, start.header)
		}
		end := blob
		if chunked {
			if r := do(t, w.addr(), "PATCH", location, blob[:4], "basic", asAdmin, "Content-Type", "application/octet-stream"); r.status != http.StatusAccepted {
				t.Fatalf("PATCH: %d %q", r.status, r.body)
			}
			work, _ := filepath.Glob(filepath.Join(team, "content", "tmp", "*"))
			local, _ := filepath.Glob(filepath.Join(w.root(), "content", "tmp", "*"))
			if len(work) == 0 || len(local) != 0 {
				t.Fatalf("work files: demo-team %v, local-write %v", work, local)
			}
			end = blob[4:]
		}
		if r := do(t, w.addr(), "PUT", location+"?digest="+digestOf(blob), end, "basic", asAdmin, "Content-Type", "application/octet-stream"); r.status != http.StatusCreated {
			t.Fatalf("end: %d %q", r.status, r.body)
		}
	}
	push(layer, true)
	push(config, false)
	manifest, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": dockerManifestType,
		"config": map[string]any{"mediaType": dockerConfigType, "size": len(config), "digest": digestOf(config)},
		"layers": []any{map[string]any{"mediaType": dockerLayerType, "size": len(layer), "digest": digestOf(layer)}}})
	if r := do(t, w.addr(), "PUT", base+"manifests/1.0", manifest, "basic", asAdmin, "Content-Type", dockerManifestType); r.status != http.StatusCreated {
		t.Fatalf("manifest: %d %q", r.status, r.body)
	}
	for _, path := range []string{"v2/-/blobs/" + digestOf(layer), "v2/demo/app/manifests/1.0", "v2/demo/app/manifests/" + digestOf(manifest)} {
		rows, err := w.srv.rp.index.Rows("demo-dk", path)
		if err != nil || len(rows) != 1 || rows[0].Store != "demo-team" {
			t.Fatalf("%s: rows %+v %v", path, rows, err)
		}
	}
	if r := do(t, w.addr(), "GET", base+"manifests/1.0", nil, "basic", asAdmin, "Accept", dockerManifestType); r.status != 200 || string(r.body) != string(manifest) {
		t.Fatalf("GET manifest: %d", r.status)
	}
}

// Review R4, ruling SR3: a start with writes on scans a created store, so
// that rows the index lost come back from its files.
func TestAStartScansACreatedStore(t *testing.T) {
	w := startIngest(t, nil)
	createdStoreRepo(t, w, "demo-team", "demo-t", true)
	if r := do(t, w.addr(), "PUT", "/repository/demo-t/docs/a.txt", []byte("kept"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("PUT: %d %q", r.status, r.body)
	}
	if _, err := w.srv.rp.index.ReplaceStore("demo-team", func(func(stores.Entry) error) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-t/docs/a.txt", nil, "basic", asAdmin); r.status != 404 {
		t.Fatalf("GET without the row: %d", r.status)
	}
	w.restart(true)
	if r := do(t, w.addr(), "GET", "/repository/demo-t/docs/a.txt", nil, "basic", asAdmin); r.status != 200 || string(r.body) != "kept" {
		t.Fatalf("GET after the start: %d %q", r.status, r.body)
	}
}

// Review R9: a created store that does not open for writes at a start, here
// because another writer holds its lock, is still served read only.
func TestALockedCreatedStoreIsServedReadOnly(t *testing.T) {
	w := startIngest(t, nil)
	root := createdStoreRepo(t, w, "demo-team", "demo-t", true)
	if r := do(t, w.addr(), "PUT", "/repository/demo-t/docs/a.txt", []byte("kept"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("PUT: %d %q", r.status, r.body)
	}
	w.ts.Close()
	if err := w.srv.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := stores.NewWriteStore("demo-team", root)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := blobwrite.Open(other, blobwrite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	w.app = startApp(w.t, w.base, w.cfg, w.opts)
	if r := do(t, w.addr(), "GET", "/repository/demo-t/docs/a.txt", nil, "basic", asAdmin); r.status != 200 || string(r.body) != "kept" {
		t.Fatalf("GET: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "PUT", "/repository/demo-t/docs/b.txt", []byte("refused"), "basic", asAdmin); r.status != 409 {
		t.Fatalf("PUT into the locked store: %d %q", r.status, r.body)
	}
}

// Ruling SR11 (finding F1 of the test host): a store config.json attaches
// whose content/ is empty is listed available with 0 blobs.
func TestAnEmptyAttachedStoreIsAvailable(t *testing.T) {
	sec := newFakeSecurity()
	base := t.TempDir()
	empty := filepath.Join(base, "demo-empty")
	if err := os.MkdirAll(filepath.Join(empty, "content"), 0o750); err != nil {
		t.Fatal(err)
	}
	storeCfgs := []config.StoreConfig{blobStore(t, base, "store-a"), {Name: "demo-empty", Kind: stores.KindBlobStore, Root: empty}}
	opts := appOptions{defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}, security: sec, writes: true}
	a := startApp(t, base, prepare(t, base, storeCfgs, opts), opts)
	r := do(t, a.addr(), "GET", "/service/rest/v1/blobstores", nil, "basic", asAdmin)
	var list []map[string]any
	if err := json.Unmarshal(r.body, &list); r.status != 200 || err != nil {
		t.Fatalf("%d %q %v", r.status, r.body, err)
	}
	found := false
	for _, s := range list {
		if s["name"] == "demo-empty" {
			found = true
			if s["unavailable"] != false || s["blobCount"] != float64(0) {
				t.Fatalf("demo-empty: %v", s)
			}
		}
	}
	if !found {
		t.Fatalf("demo-empty is not listed: %s", r.body)
	}
}

// Merge of leg 6-cleanup: a cleanup run over a repository of a created
// store marks its blobs in that store, and the compaction of that store
// removes them with its own writer.
func TestCleanupAndCompactionOfACreatedStore(t *testing.T) {
	w := startIngest(t, nil)
	root := createdStoreRepo(t, w, "demo-team", "demo-t", true)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	w.put(now.Add(-40*day), "demo-t", "docs/old.txt", "old")
	w.put(now.Add(-1*day), "demo-t", "docs/new.txt", "new")
	if _, err := w.srv.Rescan(ctx, "demo-team"); err != nil {
		t.Fatal(err)
	}
	old, err := w.srv.rp.index.Lookup("demo-t", "docs/old.txt")
	if err != nil || old == nil || old.Store != "demo-team" {
		t.Fatalf("the row %+v %v", old, err)
	}
	w.srv.side().setClock(func() time.Time { return now })
	w.at(now.Add(-10 * day))
	if c, err := w.srv.CleanupRun(ctx, agePolicy("raw", 30), "demo-t"); err != nil || c.Removed != 1 {
		t.Fatalf("cleanup %+v %v", c, err)
	}
	if props := propertiesIn(t, root, old.Location); props["deleted"] != "true" {
		t.Fatalf("the blob in demo-team has no mark: %v", props)
	}
	w.at(now)
	c, err := w.srv.CompactStore(ctx, "demo-team", 7*day)
	if err != nil || c.Removed != 1 {
		t.Fatalf("compaction %+v %v", c, err)
	}
	if _, err := os.Stat(filepath.Join(root, old.Location)); !os.IsNotExist(err) {
		t.Fatalf("the compacted blob is still there: %v", err)
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-t/docs/new.txt", nil, "basic", asAdmin); r.status != 200 || string(r.body) != "new" {
		t.Fatalf("the new file: %d %q", r.status, r.body)
	}
}
