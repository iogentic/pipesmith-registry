package rest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// storesB is the fixture of leg 6-stores-b: the identity store of
// repositoryStore with a created store demo-made below <data>/stores/ and an
// imported store demo-old outside it.
func storesB(t *testing.T) (*identityStore, *Env, StoreWrite) {
	t.Helper()
	store, env, w := repositoryStore(t)
	made := filepath.Join(w.DataDir, admin.StoresDir, "demo-made")
	old := filepath.Join(t.TempDir(), "demo-old")
	for _, dir := range []string{made, old} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	store.snap.BlobStores = append(store.snap.BlobStores, model.BlobStore{Name: "demo-made", Type: "File", Path: made},
		model.BlobStore{Name: "demo-old", Type: "File", Path: old})
	return store, env, w
}

func hostedBody(name, store, extra string) string {
	return `{"name":"` + name + `","online":true,"storage":{"blobStoreName":"` + store + `","writePolicy":"ALLOW"` + extra + `}}`
}

func recordNamed(s *model.Snapshot, name string) *model.Repository {
	for i := range s.Repositories {
		if s.Repositories[i].Name == name {
			return &s.Repositories[i]
		}
	}
	return nil
}

func storeNamed(s *model.Snapshot, name string) *model.BlobStore {
	for i := range s.BlobStores {
		if s.BlobStores[i].Name == name {
			return &s.BlobStores[i]
		}
	}
	return nil
}

// Ruling SR5: a hosted repository writes into the created store it names;
// its record holds write_store of that store. An imported store is read
// only, a name in another letter case unknown.
func TestACreatePicksAWriteStore(t *testing.T) {
	store, env, w := storesB(t)
	a, err := CreateRepository(env, w, "raw", "hosted", []byte(hostedBody("demo-into-made", "demo-made", "")))
	if err != nil || a.Status != http.StatusCreated {
		t.Fatalf("%+v %v", a, err)
	}
	rec := recordNamed(store.snap, "demo-into-made")
	if rec == nil || rec.Attributes["write_store"] != "demo-made" {
		t.Fatalf("the record: %+v", rec)
	}
	if got := repos.New(nil, nil, nil).WithAttributes(store.snap.Repositories); got != nil {
		r, _ := got.Get("demo-into-made")
		if r.WriteSettings().WriteStore != "demo-made" {
			t.Fatalf("the write settings name %s", r.WriteSettings().WriteStore)
		}
	}
	for _, c := range []struct{ store, code string }{{"demo-old", "is read only"}, {"Demo-Made", "the store is demo-made"}} {
		before, _ := json.Marshal(store.snap)
		a, err := CreateRepository(env, w, "raw", "hosted", []byte(hostedBody("demo-refused", c.store, "")))
		if err != nil || a.Status != http.StatusBadRequest || !strings.Contains(fmt.Sprint(a.Body), c.code) {
			t.Fatalf("%s: %+v %v", c.store, a, err)
		}
		if after, _ := json.Marshal(store.snap); string(after) != string(before) {
			t.Fatalf("%s: the refused create changed the state", c.store)
		}
	}
	a, err = CreateRepository(env, w, "raw", "hosted", []byte(hostedBody("demo-into-local", admin.WriteStore, "")))
	if err != nil || a.Status != http.StatusCreated {
		t.Fatalf("%+v %v", a, err)
	}
	if rec := recordNamed(store.snap, "demo-into-local"); rec == nil || rec.Attributes["write_store"] != admin.WriteStore {
		t.Fatalf("the record: %+v", rec)
	}
}

// Ruling SR6: storage.newBlobStore makes the store of storage.blobStoreName
// in the same change as the repository; the record keeps no newBlobStore.
func TestACreateMakesItsStore(t *testing.T) {
	store, env, w := storesB(t)
	a, err := CreateRepository(env, w, "raw", "hosted", []byte(hostedBody("demo-n", "demo-new", `,"newBlobStore":{}`)))
	if err != nil || a.Status != http.StatusCreated {
		t.Fatalf("%+v %v", a, err)
	}
	b := storeNamed(store.snap, "demo-new")
	if b == nil || b.Type != "File" {
		t.Fatalf("the store record: %+v", b)
	}
	if !admin.WritableStore(w.DataDir, *b, nil, store.snap.BlobStores) {
		t.Fatalf("the new store %s is no write store", b.Path)
	}
	if info, err := os.Stat(b.Path); err != nil || !info.IsDir() {
		t.Fatalf("the folder of the new store: %v", err)
	}
	rec := recordNamed(store.snap, "demo-n")
	if rec == nil || rec.Attributes["write_store"] != "demo-new" {
		t.Fatalf("the record: %+v", rec)
	}
	if storage, _ := rec.Attributes["storage"].(map[string]any); storage == nil || storage["newBlobStore"] != nil || storage["blobStoreName"] != "demo-new" {
		t.Fatalf("the storage of the record: %v", rec.Attributes["storage"])
	}
	a, err = CreateRepository(env, w, "raw", "hosted", []byte(hostedBody("demo-m", "demo-sub", `,"newBlobStore":{"path":"teams/sub"}`)))
	if err != nil || a.Status != http.StatusCreated {
		t.Fatalf("%+v %v", a, err)
	}
	if b := storeNamed(store.snap, "demo-sub"); b == nil || !strings.HasSuffix(b.Path, filepath.Join("teams", "sub")) {
		t.Fatalf("the store record: %+v", b)
	}
}

// Ruling SR6: both land or neither. A refused repository, a store name that
// is taken in any letter case, a path outside the stores folder and a state
// write that fails leave no store record and no folder.
func TestAFailedCreateLeavesNoStore(t *testing.T) {
	store, env, w := storesB(t)
	stores := filepath.Join(w.DataDir, admin.StoresDir)
	cases := []struct {
		label, body string
		status      int
	}{
		{"taken repository name", hostedBody("demo-h", "demo-x1", `,"newBlobStore":{}`), http.StatusConflict},
		{"store name in another case", hostedBody("demo-x2", "DEMO-MADE", `,"newBlobStore":{}`), http.StatusConflict},
		{"store name local-write", hostedBody("demo-x3", "Local-Write", `,"newBlobStore":{}`), http.StatusConflict},
		{"path outside", hostedBody("demo-x4", "demo-x4", `,"newBlobStore":{"path":"../../out"}`), http.StatusBadRequest},
		{"bad quota", hostedBody("demo-x5", "demo-x5", `,"newBlobStore":{"softQuota":{"type":"spaceUsedQuota","limit":5}}`), http.StatusBadRequest},
		{"server attribute", strings.Replace(hostedBody("demo-x6", "demo-x6", `,"newBlobStore":{}`), `"online":true`, `"online":true,"write_store":"elsewhere"`, 1), http.StatusBadRequest},
	}
	for _, c := range cases {
		before, _ := json.Marshal(store.snap)
		a, err := CreateRepository(env, w, "raw", "hosted", []byte(c.body))
		if err != nil || a.Status != c.status {
			t.Fatalf("%s: %+v %v", c.label, a, err)
		}
		if after, _ := json.Marshal(store.snap); string(after) != string(before) {
			t.Fatalf("%s: the refused create changed the state", c.label)
		}
	}
	w.Store = &racingStore{identityStore: store, failCommit: true}
	if _, err := CreateRepository(env, w, "raw", "hosted", []byte(hostedBody("demo-x7", "demo-x7", `,"newBlobStore":{}`))); err == nil {
		t.Fatal("a create whose state write failed answered")
	}
	// review R5: a nested path goes back to the topmost folder the create made
	if _, err := CreateRepository(env, w, "raw", "hosted", []byte(hostedBody("demo-x8", "demo-x8", `,"newBlobStore":{"path":"teams/deep/x8"}`))); err == nil {
		t.Fatal("a create whose state write failed answered")
	}
	entries, err := os.ReadDir(stores)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "demo-made" {
		t.Fatalf("the stores folder holds %v", names)
	}
}

// Ruling SR6: a new store needs nexus:blobstores:create besides the right
// to create the repository; without it the answer is 403 and nothing
// changes.
func TestANewStoreNeedsTheStorePermission(t *testing.T) {
	store, _, w := storesB(t)
	env, _ := as(store, "demo-repo-admin", "nexus:*")
	env.Permits = func(p string) bool { return !strings.HasPrefix(p, "nexus:blobstores:") }
	env.Repositories = repos.New(nil, nil, nil).WithAttributes(store.snap.Repositories)
	before, _ := json.Marshal(store.snap)
	a, err := CreateRepository(env, w, "raw", "hosted", []byte(hostedBody("demo-n", "demo-new", `,"newBlobStore":{}`)))
	if err != nil || a.Status != http.StatusForbidden || !strings.Contains(fmt.Sprint(a.Body), "nexus:blobstores:create") {
		t.Fatalf("%+v %v", a, err)
	}
	if after, _ := json.Marshal(store.snap); string(after) != string(before) {
		t.Fatal("the refused create changed the state")
	}
	if _, err := os.Stat(filepath.Join(w.DataDir, admin.StoresDir, "demo-new")); err == nil {
		t.Fatal("the refused create made the folder")
	}
	a, err = CreateRepository(env, w, "raw", "hosted", []byte(hostedBody("demo-o", "demo-made", "")))
	if err != nil || a.Status != http.StatusCreated {
		t.Fatalf("a store that exists needs no store right: %+v %v", a, err)
	}
}

// Ruling SR7: the store of a repository is fixed; a save that names another
// one is 400 store-fixed, and newBlobStore is offered on a create only.
func TestASaveKeepsTheStore(t *testing.T) {
	store, env, w := storesB(t)
	if a, err := CreateRepository(env, w, "raw", "hosted", []byte(hostedBody("demo-s", "demo-made", ""))); err != nil || a.Status != http.StatusCreated {
		t.Fatalf("%+v %v", a, err)
	}
	env.Repositories = repos.New(nil, nil, nil).WithAttributes(store.snap.Repositories)
	for _, c := range []struct{ body, code string }{
		{hostedBody("demo-s", admin.WriteStore, ""), "fixed when it is created"},
		{hostedBody("demo-s", "demo-made", `,"newBlobStore":{}`), "on a create only"},
	} {
		a, err := SaveRepository(env, w, "raw", "hosted", "demo-s", []byte(c.body))
		if err != nil || a.Status != http.StatusBadRequest || !strings.Contains(fmt.Sprint(a.Body), c.code) {
			t.Fatalf("%s: %+v %v", c.code, a, err)
		}
	}
	a, err := SaveRepository(env, w, "raw", "hosted", "demo-s", []byte(hostedBody("demo-s", "demo-made", "")))
	if err != nil || a.Status != http.StatusNoContent {
		t.Fatalf("%+v %v", a, err)
	}
	if rec := recordNamed(store.snap, "demo-s"); rec == nil || rec.Attributes["write_store"] != "demo-made" {
		t.Fatalf("the save lost write_store: %+v", rec)
	}
}

// Ruling SR9: the listing says which store takes writes.
func TestTheListingNamesTheWriteStores(t *testing.T) {
	data := t.TempDir()
	made := filepath.Join(data, admin.StoresDir, "demo-made")
	old := filepath.Join(data, "old", "demo-old")
	for _, dir := range []string{made, old} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	env := &Env{Index: noIndex{}, StoresRoot: filepath.Join(data, admin.StoresDir), Snapshot: &model.Snapshot{BlobStores: []model.BlobStore{
		{Name: "demo-made", Type: "File", Path: made}, {Name: "demo-old", Type: "File", Path: old}}},
		Stores: []Store{{Name: admin.WriteStore, Kind: "blobstore", Root: filepath.Join(data, admin.StoresDir, admin.WriteStore), Available: true}}}
	list, err := env.blobStoreXOs()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, xo := range list {
		got[xo.Name] = xo.Writable
	}
	if !got[admin.WriteStore] || !got["demo-made"] || got["demo-old"] || len(got) != 3 {
		t.Fatalf("writable: %v", got)
	}
	env.Attached = []string{"demo-made"}
	list, _ = env.blobStoreXOs()
	for _, xo := range list {
		if xo.Name == "demo-made" && xo.Writable {
			t.Fatal("a store config.json attaches is writable")
		}
	}
}

// statIndex is an index without rows but with the counts of its stores.
type statIndex struct{ stats []index.StoreStat }

func (statIndex) Lookup(string, string) (*index.Asset, error) { return nil, nil }
func (s statIndex) StoreStats() ([]index.StoreStat, error)    { return s.stats, nil }

// Ruling SR11 (finding F1 of the test host): a readable store without
// blobs is available with 0 blobs and its free space; a folder that is
// missing or cannot be read, and an empty folder of a store the index holds
// rows of (a disk not mounted), stay unavailable.
func TestAnEmptyStoreIsAvailable(t *testing.T) {
	data := t.TempDir()
	empty := filepath.Join(data, "old", "demo-empty")
	unmounted := filepath.Join(data, "old", "demo-unmounted")
	recorded := filepath.Join(data, "old", "demo-record")
	full := filepath.Join(data, "old", "demo-full")
	madeFull := filepath.Join(data, admin.StoresDir, "demo-made-full")
	gone := filepath.Join(data, "old", "demo-gone")
	for _, dir := range []string{filepath.Join(empty, "content"), filepath.Join(unmounted, "content"), recorded, filepath.Join(full, "content", "vol-01"), filepath.Join(madeFull, "content", "vol-01"), gone} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	env := &Env{Index: statIndex{stats: []index.StoreStat{{Store: "demo-unmounted", Files: 7, Bytes: 70}, {Store: "demo-gone", Files: 3, Bytes: 30}}},
		StoresRoot: filepath.Join(data, admin.StoresDir),
		Snapshot: &model.Snapshot{BlobStores: []model.BlobStore{
			{Name: "demo-record", Type: "File", Path: recorded}, {Name: "demo-full", Type: "File", Path: full},
			{Name: "demo-lost", Type: "File", Path: filepath.Join(data, "old", "demo-lost")},
			{Name: "demo-made-full", Type: "File", Path: madeFull}, {Name: "demo-gone", Type: "File", Path: gone}}},
		Stores: []Store{
			{Name: "demo-empty", Kind: "blobstore", Root: empty, Available: false, Readable: true},
			{Name: "demo-unmounted", Kind: "blobstore", Root: unmounted, Available: false, Readable: true},
			{Name: "demo-missing", Kind: "blobstore", Root: filepath.Join(data, "old", "demo-missing"), Available: false, Readable: false},
		}}
	list, err := env.blobStoreXOs()
	if err != nil {
		t.Fatal(err)
	}
	// review H4: demo-gone, a record config.json does not attach, is empty while the index holds rows of it;
	// review H9: demo-made-full, a created store with files, is available by its folder
	want := map[string]bool{"demo-empty": false, "demo-unmounted": true, "demo-missing": true, "demo-record": false, "demo-full": true, "demo-lost": true,
		"demo-made-full": false, "demo-gone": true}
	for _, xo := range list {
		if xo.Unavailable != want[xo.Name] {
			t.Errorf("%s: unavailable %v, want %v", xo.Name, xo.Unavailable, want[xo.Name])
		}
		if !xo.Unavailable && (xo.BlobCount != 0 || xo.AvailableSpaceInBytes <= 0) && xo.Name != "demo-made-full" {
			t.Errorf("%s: %d blobs, %d bytes free", xo.Name, xo.BlobCount, xo.AvailableSpaceInBytes)
		}
	}
	if len(list) != len(want) {
		t.Fatalf("%d stores listed, want %d", len(list), len(want))
	}
}

// Review H2: a blob store whose root opens but whose content/ cannot be
// read is not readable; a missing content/ is.
func TestAStoreWithAnUnreadableContentIsNotReadable(t *testing.T) {
	root := t.TempDir()
	if !ReadableStore(root, true) {
		t.Fatal("a store without content/ is not readable")
	}
	content := filepath.Join(root, "content")
	if err := os.WriteFile(content, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ReadableStore(root, true) {
		t.Fatal("a content/ that is a file is readable")
	}
	if err := os.Remove(content); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "nowhere"), content); err != nil {
		t.Fatal(err)
	}
	if ReadableStore(root, true) {
		t.Fatal("a content/ that is a dead link is readable")
	}
	if !ReadableStore(root, false) {
		t.Fatal("a directory store with a dead link in it is not readable")
	}
}
