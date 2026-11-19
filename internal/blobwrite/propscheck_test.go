package blobwrite

import (
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobprops"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// TestPropsCheckFiles writes the files of tools/propscheck/run.sh into the
// directory PSR_PROPSCHECK_OUT names, and is skipped without it: every case
// of testdata/vectors/blobwrite.json, and the properties files of a store
// the writer wrote (live blobs, a blob with the delete mark, its copy, and
// metadata.properties). For each: <name>.properties, and <name>.input with
// one line hex(key):hex(value) per item it was written from. For the files
// of the store the items are what blobprops.Load reads, so the Java side
// checks the parser of the read path as well.
func TestPropsCheckFiles(t *testing.T) {
	out := os.Getenv("PSR_PROPSCHECK_OUT")
	if out == "" {
		t.Skip("PSR_PROPSCHECK_OUT is not set; tools/propscheck/run.sh sets it")
	}
	written := 0
	write := func(name string, items map[string]string, data []byte) {
		t.Helper()
		writePropsCase(t, out, name, items, data)
		written++
	}
	for i, c := range loadVectors(t).Properties {
		data, err := Properties(c.Items, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		write(fmt.Sprintf("vector-%02d", i+1), c.Items, data)
	}

	root := newStore(t)
	now := fixedNow
	w := openTestWriter(t, root, Options{Now: func() time.Time { return now }, NewID: idList(id1, id2, id3)})
	v := loadWriterVectors(t)
	var first stores.Written
	for i, b := range v.Blobs {
		got, err := w.Write(strings.NewReader(b.Content), stores.Attributes{Repo: b.Repo, Path: b.Path, ContentType: b.ContentType,
			CreatedBy: b.CreatedBy, CreatedByIP: b.CreatedByIP})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = got
		}
	}
	now = fixedNow.Add(26 * time.Hour)
	if err := w.MarkDeleted(first.Entry.Location, "Removed by the properties check"); err != nil {
		t.Fatal(err)
	}
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, stores.PropertiesSuffix) {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	if len(files) != 5 {
		t.Fatalf("%d properties files in the store, want 3 blobs, a copy and metadata.properties", len(files))
	}
	for i, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		items, err := blobprops.Load(string(data))
		if err != nil {
			t.Fatal(err)
		}
		write(fmt.Sprintf("store-%02d", i+1), items, data)
	}
	t.Logf("%d cases written to %s", written, out)
}

// writePropsCase writes one case of tools/propscheck/run.sh: <name>.properties
// and <name>.input with one line hex(key):hex(value) per item.
func writePropsCase(t *testing.T, out, name string, items map[string]string, data []byte) {
	t.Helper()
	var input strings.Builder
	for _, key := range slices.Sorted(maps.Keys(items)) {
		fmt.Fprintf(&input, "%s:%s\n", hex.EncodeToString([]byte(key)), hex.EncodeToString([]byte(items[key])))
	}
	for suffix, content := range map[string][]byte{".properties": data, ".input": []byte(input.String())} {
		if err := os.WriteFile(filepath.Join(out, name+suffix), content, 0o644); err != nil { // #nosec G306 -- read by the Java check in a container of its own.
			t.Fatal(err)
		}
	}
}

// TestPropsCheckStore writes the files of tools/propscheck/run.sh --store:
// one case for every properties file of the store PSR_PROPSCHECK_STORE names,
// metadata.properties and every file below content/ outside content/tmp,
// with the items blobprops.Load reads. It is skipped without the two
// variables. The run checks a store the server wrote (task 8 of the phase 4
// plan): Properties.load must read what the read path reads, and
// Properties.store must write the entry lines the server wrote.
func TestPropsCheckStore(t *testing.T) {
	store, out := os.Getenv("PSR_PROPSCHECK_STORE"), os.Getenv("PSR_PROPSCHECK_OUT")
	if store == "" || out == "" {
		t.Skip("PSR_PROPSCHECK_STORE and PSR_PROPSCHECK_OUT are not set; tools/propscheck/run.sh --store sets them")
	}
	var files []string
	err := filepath.WalkDir(store, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(store, path)
		if d.IsDir() && filepath.ToSlash(rel) == stores.ContentDir+"/"+TmpDir {
			return fs.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, stores.PropertiesSuffix) &&
			(rel == "metadata.properties" || strings.HasPrefix(filepath.ToSlash(rel), stores.ContentDir+"/")) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	if len(files) < 2 {
		t.Fatalf("%d properties files in %s", len(files), store)
	}
	for i, path := range files {
		data, err := os.ReadFile(path) // #nosec G304 -- a file of the store the run names, below the repository.
		if err != nil {
			t.Fatal(err)
		}
		text := make([]rune, len(data))
		for j, c := range data {
			text[j] = rune(c)
		}
		items, err := blobprops.Load(string(text))
		if err != nil {
			t.Fatal(err)
		}
		writePropsCase(t, out, fmt.Sprintf("store-%05d", i+1), items, data)
	}
	t.Logf("%d cases written to %s from %s", len(files), out, store)
}
