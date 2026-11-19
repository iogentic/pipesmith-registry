package server

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// writeData is a data directory with one store of the old product and a
// scan of it.
func writeData(t *testing.T) *config.Config {
	t.Helper()
	base := t.TempDir()
	return prepare(t, base, []config.StoreConfig{blobStore(t, base, "store-a", jarSpec("jar bytes"))}, appOptions{})
}

// Writes need security (decision E4): New refuses them without it.
func TestWritesNeedSecurity(t *testing.T) {
	cfg := writeData(t)
	if srv, err := New(cfg, nil, Options{Writes: true}); err == nil {
		srv.Close()
		t.Fatal("writes without security")
	} else if !strings.Contains(err.Error(), "state.sqlite") {
		t.Fatalf("error %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "stores")); err == nil {
		t.Fatal("the refused start made the write store")
	}
}

// With writes on the first start makes the write store local-write in the
// layout of the old product (W1), holds its lock and attaches it; Close
// gives the lock back.
func TestWritesOnMakeTheWriteStore(t *testing.T) {
	cfg := writeData(t)
	srv, err := New(cfg, nil, Options{Writes: true, Security: newFakeSecurity()})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(cfg.DataDir, "stores", "local-write")
	for _, name := range []string{"metadata.properties", "content/tmp", "reconciliation"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	st, err := srv.rp.sync()
	if err != nil {
		t.Fatal(err)
	}
	if s := st.stores["local-write"]; s == nil || !stores.Writable(s) || srv.writes.writer == nil || srv.writes.writer.Name() != "local-write" {
		t.Fatalf("write store %v, writer %v", s, srv.writes.writer)
	}
	if second, err := New(cfg, nil, Options{Writes: true, Security: newFakeSecurity()}); err == nil {
		second.Close()
		t.Fatal("a second server writes the same store")
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := New(cfg, nil, Options{Writes: true, Security: newFakeSecurity()})
	if err != nil {
		t.Fatalf("after the close: %v", err)
	}
	again.Close()
}

// With writes off a data directory without a write store stays as it is;
// one with a write store has it attached read only, so earlier writes are
// still served.
func TestWritesOffAttachTheWriteStoreReadOnly(t *testing.T) {
	cfg := writeData(t)
	srv, err := New(cfg, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := srv.rp.sync()
	if st.stores["local-write"] != nil || srv.writes.writer != nil {
		t.Fatal("a write store without writes")
	}
	srv.Close()
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "stores")); err == nil {
		t.Fatal("a start with writes off made the write store")
	}

	root := filepath.Join(cfg.DataDir, "stores", "local-write")
	if err := blobwrite.Create(root, time.Now()); err != nil {
		t.Fatal(err)
	}
	srv, err = New(cfg, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	st, _ = srv.rp.sync()
	if s := st.stores["local-write"]; s == nil || stores.Writable(s) || srv.writes.writer != nil {
		t.Fatalf("write store %v with writes off", s)
	}
}

// A store of config.json named local-write keeps its place with writes off
// and stops a start with writes on.
func TestAStoreNamedLikeTheWriteStore(t *testing.T) {
	base := t.TempDir()
	cfg := prepare(t, base, []config.StoreConfig{blobStore(t, base, "local-write", jarSpec("jar bytes"))}, appOptions{})
	if srv, err := New(cfg, nil, Options{Writes: true, Security: newFakeSecurity()}); err == nil {
		srv.Close()
		t.Fatal("writes on beside a configured store named local-write")
	}
	srv, err := New(cfg, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	st, _ := srv.rp.sync()
	if s := st.stores["local-write"]; s == nil || s.Root() != cfg.Stores[0].Root {
		t.Fatalf("%v", s)
	}
}

// A store of config.json named like the write store stops a start with writes
// on (TestAStoreNamedLikeTheWriteStore). One that appears in config.json while
// the server runs is refused at the reload in the same way (finding N6 of
// review 4): the attached stores stay, the write store among them, and what
// was written before and after is served.
func TestAStoreNamedLikeTheWriteStoreAtAReload(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	if _, err := w.ingest("demo-raw", "docs/a.txt", "written"); err != nil {
		t.Fatal(err)
	}
	named := blobStore(t, w.base, "elsewhere")
	named.Name = "local-write"
	changed := *w.cfg
	changed.Stores = append(slices.Clone(w.cfg.Stores), named)
	if err := config.Save(&changed); err != nil {
		t.Fatal(err)
	}
	if r := w.fetch("demo-raw", "docs/a.txt"); r.status != 200 || string(r.body) != "written" {
		t.Fatalf("after the reload: %d %q", r.status, r.body)
	}
	if _, err := w.ingest("demo-raw", "docs/b.txt", "after"); err != nil {
		t.Fatal(err)
	}
	if r := w.fetch("demo-raw", "docs/b.txt"); r.status != 200 || string(r.body) != "after" {
		t.Fatalf("a write after the reload: %d %q", r.status, r.body)
	}
}
