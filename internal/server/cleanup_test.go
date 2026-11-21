package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	adminrules "github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/cleanup"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// The cleanup run of leg 6-cleanup on the server (task 12 of the phase 6
// plan): invented repositories with fixed creation and download times, a
// preview that lists what a run removes and removes nothing, and per
// format raw by age, npm by usage, maven2 with the metadata rebuild, and
// docker with the layers of CL6.

const day = 24 * time.Hour

// at sets the clock of the writer: the creation time of the next blobs.
func (w *writing) at(t time.Time) {
	w.clock.mu.Lock()
	defer w.clock.mu.Unlock()
	w.clock.now = t
}

// put writes a blob at a creation time.
func (w *writing) put(created time.Time, repo, path, body string) {
	w.t.Helper()
	w.at(created)
	if _, err := w.ingest(repo, path, body); err != nil {
		w.t.Fatalf("%s/%s: %v", repo, path, err)
	}
}

func agePolicy(format string, days int) model.CleanupPolicy {
	return model.CleanupPolicy{Name: "demo-age", Format: format, Criteria: map[string]any{adminrules.CriterionLastBlobUpdatedDays: float64(days)}}
}

func usagePolicy(format string, days int) model.CleanupPolicy {
	return model.CleanupPolicy{Name: "demo-usage", Format: format, Criteria: map[string]any{adminrules.CriterionLastDownloadedDays: float64(days)}}
}

func previewPaths(p cleanup.Preview) []string {
	var paths []string
	for _, item := range p.Items {
		paths = append(paths, item.Path)
	}
	return paths
}

// catalogPaths are the asset paths catalog.sqlite holds for a repository.
func (w *writing) catalogPaths(repo string) []string {
	w.t.Helper()
	cat, err := catalog.Open(w.cfg.DataDir)
	if err != nil {
		w.t.Fatal(err)
	}
	defer cat.Close()
	rows, err := cat.AssetRows(repo)
	if err != nil {
		w.t.Fatal(err)
	}
	var paths []string
	for _, r := range rows {
		paths = append(paths, r.Path)
	}
	return paths
}

func (w *writing) preview(policy model.CleanupPolicy, repo string) cleanup.Preview {
	w.t.Helper()
	p, err := w.srv.CleanupPreview(context.Background(), policy, repo)
	if err != nil {
		w.t.Fatal(err)
	}
	return p
}

// A policy by age over a raw repository: the preview lists the old blobs
// of the write store and removes nothing, also while the server is frozen;
// a frozen run stops with nothing removed; the run removes exactly the
// listed paths with the delete mark on their blobs and keeps the blob of
// the old store (CL1); a second run removes nothing.
func TestCleanupByAgeRemovesWhatThePreviewLists(t *testing.T) {
	old := spec("demo-raw", "old/kept.txt", []byte("kept by the old product"), "text/plain")
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")}, old)
	now := time.Now().UTC().Truncate(time.Millisecond)
	w.put(now.Add(-40*day), "demo-raw", "docs/old.txt", "old")
	w.put(now.Add(-35*day), "demo-raw", "docs/older-than-30.txt", "old too")
	w.put(now.Add(-1*day), "demo-raw", "docs/new.txt", "new")
	w.catalogRescan()
	w.srv.side().setClock(func() time.Time { return now })
	ctx := context.Background()
	policy := agePolicy("raw", 30)
	before := w.livePaths()
	catalogBefore := w.catalogPaths("demo-raw")
	locations := map[string]string{}
	for _, p := range []string{"docs/old.txt", "docs/older-than-30.txt"} {
		row, err := w.srv.rp.index.Lookup("demo-raw", p)
		if err != nil || row == nil {
			t.Fatal(p, err)
		}
		locations[p] = row.Location
	}
	want := []string{"docs/old.txt", "docs/older-than-30.txt"}
	dry, err := w.srv.RunCleanup(ctx, policy, "demo-raw", true)
	if err != nil || !slices.Equal(previewPaths(dry.Preview), want) || dry.Preview.Total != 2 || dry.Preview.Truncated ||
		dry.Counts != (CleanupCounts{Selected: 3, Kept: 1}) {
		t.Fatalf("dry run %+v %v", dry, err)
	}
	if err := w.srv.readOnly.Freeze(); err != nil {
		t.Fatal(err)
	}
	if p := w.preview(policy, "demo-raw"); !slices.Equal(previewPaths(p), want) {
		t.Fatalf("a preview while frozen: %+v", p)
	}
	if _, err := w.srv.CleanupRun(ctx, policy, "demo-raw"); err == nil || err.Error() != "stopped: the server froze" {
		t.Fatalf("a frozen run: %v", err)
	}
	if err := w.srv.readOnly.Release(); err != nil {
		t.Fatal(err)
	}
	if got := w.livePaths(); !slices.Equal(got, before) || !slices.Equal(w.catalogPaths("demo-raw"), catalogBefore) {
		t.Fatalf("a preview or a frozen run removed: %v", got)
	}
	got, err := w.srv.CleanupRun(ctx, policy, "demo-raw")
	if err != nil || got != (CleanupCounts{Selected: 3, Removed: 2, Kept: 1}) {
		t.Fatalf("run %+v %v", got, err)
	}
	if live := w.livePaths(); !slices.Equal(live, []string{"demo-raw/docs/new.txt"}) {
		t.Fatalf("live after the run %v", live)
	}
	for p, location := range locations {
		if props := w.properties(location); props["deleted"] != "true" || props["deletedReason"] != reasonDeleted {
			t.Errorf("%s has no delete mark: %q", p, props)
		}
	}
	if paths := w.catalogPaths("demo-raw"); !slices.Equal(paths, []string{"docs/new.txt", "old/kept.txt"}) {
		t.Fatalf("catalog after the run %v", paths)
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-raw/docs/old.txt", nil, "basic", asAdmin); r.status != 404 {
		t.Fatalf("a removed path answers %d", r.status)
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-raw/old/kept.txt", nil, "basic", asAdmin); r.status != 200 || string(r.body) != "kept by the old product" {
		t.Fatalf("the blob of the old store answers %d", r.status)
	}
	again, err := w.srv.CleanupRun(ctx, policy, "demo-raw")
	if err != nil || again != (CleanupCounts{Selected: 1, Kept: 1}) {
		t.Fatalf("a second run %+v %v", again, err)
	}
}

// A policy with a criterion this server does not know is refused (A9),
// never an empty list; a mark that fails stops the run with nothing
// removed; a stopped run ends; no message names a path or a repository.
func TestCleanupRefusalsAndStops(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	now := time.Now().UTC().Truncate(time.Millisecond)
	w.put(now.Add(-40*day), "demo-raw", "docs/old.txt", "old")
	w.put(now.Add(-40*day), "demo-raw", "docs/old-2.txt", "old")
	w.catalogRescan()
	ctx := context.Background()
	unknown := agePolicy("raw", 30)
	unknown.Criteria["regex"] = ".*"
	p, err := w.srv.CleanupPreview(ctx, unknown, "demo-raw")
	var refusals adminrules.Refusals
	if !errors.As(err, &refusals) || len(refusals) != 1 || refusals[0].Code != adminrules.CodeCriterionUnknown || p.Total != 0 {
		t.Fatalf("an unknown criterion: %+v %v", p, err)
	}
	if _, err := w.srv.CleanupPreview(ctx, agePolicy("maven2", 30), "demo-raw"); err == nil {
		t.Fatal("a policy of another format ran over the repository")
	}
	before := w.livePaths()
	w.srv.writes.failMarks(func(string, string) error { return errors.New("no mark at " + w.root()) })
	c, err := w.srv.CleanupRun(ctx, agePolicy("raw", 30), "demo-raw")
	w.srv.writes.failMarks(nil)
	if err == nil || err.Error() != "a delete mark failed, so the run stops" || c.Removed != 0 {
		t.Fatalf("a failed mark: %+v %v", c, err)
	}
	if got := w.livePaths(); !slices.Equal(got, before) || len(w.catalogPaths("demo-raw")) != 2 {
		t.Fatalf("a failed mark removed: %v", got)
	}
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := w.srv.CleanupRun(stopped, agePolicy("raw", 30), "demo-raw"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a stopped run: %v", err)
	}
	for _, e := range []error{err} {
		if e != nil && (strings.Contains(e.Error(), "docs/") || strings.Contains(e.Error(), "demo-raw") || strings.Contains(e.Error(), w.cfg.DataDir)) {
			t.Errorf("a message names a path: %v", e)
		}
	}
}

// A policy by usage over an npm repository: the last download times of
// GETs at fixed times select; an asset never downloaded falls under the
// build time of the catalog file (A19); after the file is built new, the
// preview lists nothing until the criterion age has passed (CL3), and the
// download times of the old file are gone with it.
func TestCleanupByUsageAndTheBuildTimeOfTheCatalog(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-npm": storage("ALLOW")})
	now := time.Now().UTC().Truncate(time.Millisecond)
	paths := []string{"demo-other/-/demo-other-1.0.0.tgz", "demo-pkg/-/demo-pkg-1.0.0.tgz", "demo-pkg/-/demo-pkg-2.0.0.tgz"}
	for _, p := range paths {
		w.put(now.Add(-100*day), "demo-npm", p, "tarball of "+p)
	}
	w.catalogRescan()
	// the catalog file was built now; base is 60 days later
	base := now.Add(60 * day)
	side := w.srv.side()
	for p, ago := range map[string]time.Duration{paths[1]: 40 * day, paths[2]: 5 * day} {
		side.setClock(func() time.Time { return base.Add(-ago) })
		if r := do(t, w.addr(), "GET", "/repository/demo-npm/"+p, nil, "basic", asAdmin); r.status != 200 {
			t.Fatalf("GET %s: %d", p, r.status)
		}
		if _, err := side.downloads.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	policy := usagePolicy("npm", 30)
	side.setClock(func() time.Time { return base })
	if got := previewPaths(w.preview(policy, "demo-npm")); !slices.Equal(got, paths[:2]) {
		t.Fatalf("by usage %v", got)
	}
	side.setClock(func() time.Time { return base.Add(31 * day) })
	if got := previewPaths(w.preview(policy, "demo-npm")); !slices.Equal(got, paths) {
		t.Fatalf("31 days later %v", got)
	}
	// the catalog file built new: no download time, a new build time
	w.srv.closeSide()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(catalog.Path(w.cfg.DataDir) + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	for _, store := range []string{"store-a", "local-write"} {
		if _, err := w.srv.Rescan(context.Background(), store); err != nil {
			t.Fatal(err)
		}
	}
	side = w.srv.side()
	for _, offset := range []time.Duration{0, 29 * day} {
		side.setClock(func() time.Time { return now.Add(offset) })
		if p := w.preview(policy, "demo-npm"); p.Total != 0 {
			t.Fatalf("%v after the new build: %v", offset, previewPaths(p))
		}
	}
	side.setClock(func() time.Time { return time.Now().Add(31 * day) })
	if got := previewPaths(w.preview(policy, "demo-npm")); !slices.Equal(got, paths) {
		t.Fatalf("31 days after the new build %v", got)
	}
	c, err := w.srv.CleanupRun(context.Background(), policy, "demo-npm")
	if err != nil || c != (CleanupCounts{Selected: 3, Removed: 3}) {
		t.Fatalf("run %+v %v", c, err)
	}
	if live := w.livePaths(); len(live) != 0 {
		t.Fatalf("live %v", live)
	}
}

// A policy over a hosted maven2 repository rebuilds the metadata of the
// artifacts it touched: the removed version leaves maven-metadata.xml.
func TestCleanupOfMavenRebuildsTheMetadata(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-releases": storage("ALLOW")})
	now := time.Now().UTC().Truncate(time.Millisecond)
	base := "com/example/demo-lib/"
	w.put(now.Add(-40*day), "demo-releases", base+"1.0.0/demo-lib-1.0.0.pom", "<project/>")
	w.put(now.Add(-40*day), "demo-releases", base+"1.0.0/demo-lib-1.0.0.jar", "old jar")
	w.put(now.Add(-1*day), "demo-releases", base+"1.1.0/demo-lib-1.1.0.pom", "<project/>")
	if _, err := w.srv.RebuildMaven(context.Background(), "demo-releases", tasks.MavenScope{}); err != nil {
		t.Fatal(err)
	}
	meta := "/repository/demo-releases/" + base + "maven-metadata.xml"
	if r := do(t, w.addr(), "GET", meta, nil, "basic", asAdmin); r.status != 200 || !strings.Contains(string(r.body), "<version>1.0.0</version>") {
		t.Fatalf("metadata before %d %s", r.status, r.body)
	}
	w.catalogRescan()
	w.srv.side().setClock(func() time.Time { return now })
	c, err := w.srv.CleanupRun(context.Background(), agePolicy("maven2", 30), "demo-releases")
	if err != nil || c.Removed != 2 {
		t.Fatalf("run %+v %v", c, err)
	}
	r := do(t, w.addr(), "GET", meta, nil, "basic", asAdmin)
	if r.status != 200 || strings.Contains(string(r.body), "<version>1.0.0</version>") || !strings.Contains(string(r.body), "<version>1.1.0</version>") {
		t.Fatalf("metadata after %d %s", r.status, r.body)
	}
}

// CL6: a policy that removes a docker manifest also removes the layers and
// the config that no manifest that stays names; a layer that a manifest
// that stays names is kept even when the policy selects it.
func TestCleanupOfDockerFreesTheLayersNoManifestNames(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-docker": storage("ALLOW")})
	now := time.Now().UTC().Truncate(time.Millisecond)
	shared := []byte("layer shared by both images")
	oldImage := dockerImage("demo-app", "1.0", []byte("layer of the old image only"), shared)
	newImage := dockerImage("demo-app", "2.0", shared)
	for path, f := range oldImage {
		w.put(now.Add(-40*day), "demo-docker", path, string(f.data))
	}
	for path, f := range newImage {
		if _, held := oldImage[path]; !held {
			w.put(now.Add(-1*day), "demo-docker", path, string(f.data))
		}
	}
	orphan := "v2/-/blobs/" + digestOf([]byte("a blob no manifest names"))
	w.put(now.Add(-1*day), "demo-docker", orphan, "a blob no manifest names")
	w.catalogRescan()
	w.srv.side().setClock(func() time.Time { return now })
	policy := agePolicy("docker", 30)
	var want []string
	for path := range oldImage {
		if path != "v2/-/blobs/"+digestOf(shared) {
			want = append(want, path)
		}
	}
	slices.Sort(want)
	want = append(want, orphan)
	p := w.preview(policy, "demo-docker")
	if got := previewPaths(p); !slices.Equal(got, want) {
		t.Fatalf("preview\n%v\nwant\n%v", got, want)
	}
	c, err := w.srv.CleanupRun(context.Background(), policy, "demo-docker")
	if err != nil || c != (CleanupCounts{Selected: 5, Removed: 5, Kept: 1}) {
		t.Fatalf("run %+v %v", c, err)
	}
	var live []string
	for _, path := range w.livePaths() {
		live = append(live, strings.TrimPrefix(path, "demo-docker/"))
	}
	var stays []string
	for path := range newImage {
		stays = append(stays, path)
	}
	slices.Sort(stays)
	if !slices.Equal(live, stays) {
		t.Fatalf("live\n%v\nwant\n%v", live, stays)
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-docker/v2/demo-app/manifests/2.0", nil, "basic", asAdmin); r.status != 200 {
		t.Fatal(fmt.Sprintf("the manifest that stays answers %d", r.status))
	}
}
