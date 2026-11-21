package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// The fixes of the review of leg 6-cleanup: a docker layer that a manifest
// written since the plan names stays, and so does a fresh layer no manifest
// names yet; a child manifest that a list that stays names stays; a stop
// ends a run that waits for a held path; a failed mark still rebuilds the
// maven metadata of what was removed; compaction removes an old bytes file
// that a kill left without its properties file.

func TestADockerLayerThatAManifestNamesSinceThePlanStays(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-docker": storage("ALLOW")})
	now := time.Now().UTC().Truncate(time.Millisecond)
	oldLayer := []byte("layer of the old image")
	oldImage := dockerImage("demo-app", "1.0", oldLayer)
	for path, f := range oldImage {
		w.put(now.Add(-40*day), "demo-docker", path, string(f.data))
	}
	fresh := "v2/-/blobs/" + digestOf([]byte("a layer of a push that runs"))
	w.put(now.Add(-time.Hour), "demo-docker", fresh, "a layer of a push that runs")
	w.catalogRescan()
	w.srv.side().setClock(func() time.Time { return now })
	p := w.preview(agePolicy("docker", 30), "demo-docker")
	if slices.Contains(previewPaths(p), fresh) || !slices.Contains(previewPaths(p), "v2/-/blobs/"+digestOf(oldLayer)) {
		t.Fatalf("preview %v", previewPaths(p))
	}
	// a second image that uses the old layer is pushed while the run removes
	// the manifests of the first
	again := dockerImage("demo-app", "3.0", oldLayer)
	var pushed atomic.Bool
	beforeTaskUnit = func() {
		if pushed.CompareAndSwap(false, true) {
			for path, f := range again {
				if _, held := oldImage[path]; !held {
					w.put(now.Add(-time.Minute), "demo-docker", path, string(f.data))
				}
			}
		}
	}
	t.Cleanup(func() { beforeTaskUnit = nil })
	c, err := w.srv.CleanupRun(context.Background(), agePolicy("docker", 30), "demo-docker")
	beforeTaskUnit = nil
	if err != nil || c.Removed != 3 || c.Kept != 1 {
		t.Fatalf("run %+v %v", c, err)
	}
	for _, path := range []string{"v2/-/blobs/" + digestOf(oldLayer), fresh} {
		if !slices.Contains(w.livePaths(), "demo-docker/"+path) {
			t.Errorf("%s is gone", path)
		}
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-docker/v2/demo-app/manifests/3.0", nil, "basic", asAdmin); r.status != 200 {
		t.Errorf("the pushed manifest answers %d", r.status)
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-docker/v2/demo-app/manifests/1.0", nil, "basic", asAdmin); r.status != 404 {
		t.Errorf("the old manifest answers %d", r.status)
	}
}

func TestAChildManifestThatAListThatStaysNamesStays(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-docker": storage("ALLOW")})
	now := time.Now().UTC().Truncate(time.Millisecond)
	layer := []byte("layer of the child")
	child := dockerImage("demo-app", "child", layer)
	var childManifest []byte
	for path, f := range child {
		if path != "v2/demo-app/manifests/child" {
			w.put(now.Add(-40*day), "demo-docker", path, string(f.data))
		}
		if strings.HasSuffix(path, "/manifests/child") {
			childManifest = f.data
		}
	}
	list := fmt.Sprintf(`{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": [`+
		`{"mediaType": %q, "size": %d, "digest": %q}]}`, dockerManifestType, len(childManifest), digestOf(childManifest))
	w.put(now.Add(-1*day), "demo-docker", "v2/demo-app/manifests/multi", list)
	w.catalogRescan()
	w.srv.side().setClock(func() time.Time { return now })
	c, err := w.srv.CleanupRun(context.Background(), agePolicy("docker", 30), "demo-docker")
	if err != nil || c.Removed != 0 {
		t.Fatalf("run %+v %v", c, err)
	}
	childPath := "v2/demo-app/manifests/" + digestOf(childManifest)
	if !slices.Contains(w.livePaths(), "demo-docker/"+childPath) || !slices.Contains(w.livePaths(), "demo-docker/v2/-/blobs/"+digestOf(layer)) {
		t.Fatalf("live %v", w.livePaths())
	}
}

func TestAStopEndsARunThatWaitsForAHeldPath(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	now := time.Now().UTC().Truncate(time.Millisecond)
	w.put(now.Add(-40*day), "demo-raw", "docs/old.txt", "old")
	w.catalogRescan()
	w.srv.side().setClock(func() time.Time { return now })
	unlock := w.srv.writes.locks.lock("demo-raw", "docs/old.txt")
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := w.srv.CleanupRun(ctx, agePolicy("raw", 30), "demo-raw")
	if took := time.Since(start); !errors.Is(err, context.Canceled) || took > time.Second {
		t.Fatalf("a stopped run that waits %v after %s", err, took)
	}
	if !slices.Contains(w.livePaths(), "demo-raw/docs/old.txt") {
		t.Fatal("the held path is gone")
	}
}

func TestAFailedMarkStillRebuildsTheMavenMetadata(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-releases": storage("ALLOW")})
	now := time.Now().UTC().Truncate(time.Millisecond)
	base := "com/example/demo-lib/"
	w.put(now.Add(-40*day), "demo-releases", base+"1.0.0/demo-lib-1.0.0.jar", "old jar")
	w.put(now.Add(-40*day), "demo-releases", base+"1.0.0/demo-lib-1.0.0.pom", "<project/>")
	w.put(now.Add(-40*day), "demo-releases", base+"1.0.1/demo-lib-1.0.1.pom", "<project/>")
	w.put(now.Add(-1*day), "demo-releases", base+"1.1.0/demo-lib-1.1.0.pom", "<project/>")
	if _, err := w.srv.RebuildMaven(context.Background(), "demo-releases", tasks.MavenScope{}); err != nil {
		t.Fatal(err)
	}
	w.catalogRescan()
	w.srv.side().setClock(func() time.Time { return now })
	var marks atomic.Int32
	w.srv.writes.failMarks(func(string, string) error {
		if marks.Add(1) == 3 {
			return errors.New("no mark")
		}
		return nil
	})
	c, err := w.srv.CleanupRun(context.Background(), agePolicy("maven2", 30), "demo-releases")
	w.srv.writes.failMarks(nil)
	if err == nil || err.Error() != "a delete mark failed, so the run stops" || c.Removed != 2 {
		t.Fatalf("run %+v %v", c, err)
	}
	r := do(t, w.addr(), "GET", "/repository/demo-releases/"+base+"maven-metadata.xml", nil, "basic", asAdmin)
	if r.status != 200 || strings.Contains(string(r.body), "<version>1.0.0</version>") || !strings.Contains(string(r.body), "<version>1.0.1</version>") {
		t.Fatalf("metadata after a failed mark %d %s", r.status, r.body)
	}
}

func TestCompactionRemovesAnOldLooseBytesFile(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	now := time.Now().UTC().Truncate(time.Millisecond)
	w.put(now.Add(-40*day), "demo-raw", "docs/old.txt", "old")
	w.put(now.Add(-40*day), "demo-raw", "docs/young.txt", "young!")
	w.catalogRescan()
	rows := map[string]string{}
	for _, p := range []string{"docs/old.txt", "docs/young.txt"} {
		row, err := w.srv.rp.index.Lookup("demo-raw", p)
		if err != nil || row == nil {
			t.Fatal(p, err)
		}
		rows[p] = row.Location
	}
	w.at(now.Add(-10 * day))
	if c, err := w.srv.CleanupRun(context.Background(), agePolicy("raw", 30), "demo-raw"); err != nil || c.Removed != 2 {
		t.Fatalf("cleanup %+v %v", c, err)
	}
	// a kill after the properties file of each went: no properties and no
	// copy are left, the bytes stay; one bytes file was written 10 days ago
	for p, location := range rows {
		bytesFile := filepath.Join(w.root(), filepath.FromSlash(location))
		props := strings.TrimSuffix(bytesFile, ".bytes") + ".properties"
		copies, _ := filepath.Glob(filepath.Join(w.root(), "content", "*", "*", "*", "*", "*", filepath.Base(props)))
		for _, f := range slices.Compact(slices.Sorted(slices.Values(append(copies, props)))) {
			if err := os.Remove(f); err != nil {
				t.Fatal(err)
			}
		}
		at := now.Add(-10 * day)
		if p == "docs/young.txt" {
			at = now
		}
		if err := os.Chtimes(bytesFile, at, at); err != nil {
			t.Fatal(err)
		}
	}
	w.srv.side().setClock(func() time.Time { return now })
	c, err := w.srv.CompactStore(context.Background(), "local-write", 7*day)
	if err != nil || c != (CompactCounts{Removed: 1, Bytes: 3}) {
		t.Fatalf("compaction %+v %v", c, err)
	}
	for p, location := range rows {
		_, err := os.Lstat(filepath.Join(w.root(), filepath.FromSlash(location)))
		if gone := errors.Is(err, os.ErrNotExist); gone != (p == "docs/old.txt") {
			t.Errorf("%s gone %v", p, gone)
		}
	}
	if counts, live := w.scan(); counts.Deleted != 0 || len(live) != 0 {
		t.Fatalf("scan %+v %v", counts, live)
	}
}
