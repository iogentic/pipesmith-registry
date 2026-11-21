package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// The deleting task kinds of leg 6-cleanup on the server (rulings CL1,
// CL4, CL7, CL9 and CL10): the cleanup over every policy, the compaction,
// and the removal of maven2 snapshots.

// withPolicies sets the cleanup policies of the state.
func (w *writing) withPolicies(policies ...model.CleanupPolicy) {
	sec := w.srv.sec.(*fakeSecurity)
	sec.mu.Lock()
	defer sec.mu.Unlock()
	snap := *sec.snap
	snap.CleanupPolicies = policies
	sec.snap = &snap
}

func cleanupNames(names ...any) map[string]any { return map[string]any{"policyNames": names} }

// Each policy runs over the repositories whose attributes name it, all of
// them for *, one when it is given; a policy of another format than a
// repository that names it is refused and the others run on.
func TestCleanupAllRunsEveryPolicyOverTheRepositoriesThatNameIt(t *testing.T) {
	raw := storage("ALLOW")
	raw["cleanup"] = cleanupNames("demo-age", "demo-maven")
	npm := storage("ALLOW")
	npm["cleanup"] = cleanupNames("demo-npm-age")
	w := startIngest(t, map[string]map[string]any{"demo-raw": raw, "demo-npm": npm})
	now := time.Now().UTC().Truncate(time.Millisecond)
	w.put(now.Add(-40*day), "demo-raw", "docs/old.txt", "old")
	w.put(now.Add(-1*day), "demo-raw", "docs/new.txt", "new")
	w.put(now.Add(-40*day), "demo-npm", "demo-pkg/-/demo-pkg-1.0.0.tgz", "tgz")
	w.catalogRescan()
	w.srv.side().setClock(func() time.Time { return now })
	age, npmAge, maven := agePolicy("raw", 30), agePolicy("npm", 30), agePolicy("maven2", 30)
	npmAge.Name, maven.Name = "demo-npm-age", "demo-maven"
	w.withPolicies(age, npmAge, maven, model.CleanupPolicy{Name: "demo-unused", Format: "raw", Criteria: age.Criteria})
	ctx := context.Background()
	one, err := w.srv.CleanupAll(ctx, "demo-npm")
	if err != nil || one != (tasks.CleanupCounts{Runs: 1, Selected: 1, Removed: 1}) {
		t.Fatalf("one repository %+v %v", one, err)
	}
	if live := w.livePaths(); !slices.Equal(live, []string{"demo-raw/docs/new.txt", "demo-raw/docs/old.txt"}) {
		t.Fatalf("live after one %v", live)
	}
	all, err := w.srv.CleanupAll(ctx, "*")
	if err != nil || all != (tasks.CleanupCounts{Runs: 2, Refused: 1, Selected: 1, Removed: 1}) {
		t.Fatalf("all %+v %v", all, err)
	}
	if live := w.livePaths(); !slices.Equal(live, []string{"demo-raw/docs/new.txt"}) {
		t.Fatalf("live after all %v", live)
	}
	w.put(now.Add(-40*day), "demo-raw", "docs/later.txt", "later")
	w.catalogRescan()
	if err := w.srv.readOnly.Freeze(); err != nil {
		t.Fatal(err)
	}
	_, err = w.srv.CleanupAll(ctx, "*")
	if releaseErr := w.srv.readOnly.Release(); releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if err == nil || err.Error() != "stopped: the server froze" {
		t.Fatalf("a frozen run %v", err)
	}
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := w.srv.CleanupAll(stopped, "*"); err != context.Canceled {
		t.Fatalf("a stopped run %v", err)
	}
}

// The compaction kind takes the age of its task; a store of the old
// product is refused.
func TestCompactOfTheTaskHost(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	now := time.Now().UTC().Truncate(time.Millisecond)
	w.put(now.Add(-40*day), "demo-raw", "docs/old.txt", "old")
	w.catalogRescan()
	w.at(now.Add(-10 * day))
	if _, err := w.srv.CleanupRun(context.Background(), agePolicy("raw", 30), "demo-raw"); err != nil {
		t.Fatal(err)
	}
	w.srv.side().setClock(func() time.Time { return now })
	if c, err := w.srv.Compact(context.Background(), "local-write", 30*day); err != nil || c != (tasks.CompactCounts{Kept: 1}) {
		t.Fatalf("young %+v %v", c, err)
	}
	if c, err := w.srv.Compact(context.Background(), "local-write", 7*day); err != nil || c != (tasks.CompactCounts{Removed: 1, Bytes: 3}) {
		t.Fatalf("old %+v %v", c, err)
	}
	if _, err := w.srv.Compact(context.Background(), "store-a", 7*day); err == nil || err.Error() != "the store of the task is not a store of this server" {
		t.Fatalf("a store of the old product %v", err)
	}
}

// CL7: per snapshot version the newest minimumRetained builds stay, and of
// the others those older than the days go, with their checksum files; the
// metadata of the version is written again; a release and a build within
// the days stay; a repository that is no hosted maven2 one is refused.
func TestRemoveSnapshotsKeepsTheNewestBuilds(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-releases": storage("ALLOW")})
	now := time.Now().UTC().Truncate(time.Second)
	dir := "com/example/demo-lib/1.0-SNAPSHOT/"
	build := func(at time.Time, n int) string {
		return "demo-lib-1.0-" + at.Format("20060102.150405") + "-" + string(rune('0'+n))
	}
	b1, b2, b3, b4 := build(now.Add(-50*day), 1), build(now.Add(-40*day), 2), build(now.Add(-35*day), 3), build(now.Add(-2*day), 4)
	for _, b := range []string{b1, b2, b3, b4} {
		w.put(now.Add(-50*day), "demo-releases", dir+b+".jar", "jar "+b)
		w.put(now.Add(-50*day), "demo-releases", dir+b+".jar.sha1", "sha1")
		w.put(now.Add(-50*day), "demo-releases", dir+b+".pom", "<project/>")
	}
	w.put(now.Add(-50*day), "demo-releases", "com/example/demo-lib/0.9/demo-lib-0.9.jar", "release")
	other := "com/example/demo-app/2.0-SNAPSHOT/"
	appBuild := "demo-app-2.0-" + now.Add(-60*day).Format("20060102.150405") + "-1"
	w.put(now.Add(-60*day), "demo-releases", other+appBuild+".jar", "app")
	if _, err := w.srv.RebuildMaven(context.Background(), "demo-releases", tasks.MavenScope{}); err != nil {
		t.Fatal(err)
	}
	w.srv.side().setClock(func() time.Time { return now })
	ctx := context.Background()
	c, err := w.srv.RemoveSnapshots(ctx, "demo-releases", 2, 30*day)
	if err != nil || c != (tasks.SnapshotCounts{Selected: 6, Removed: 6}) {
		t.Fatalf("keep 2 %+v %v", c, err)
	}
	var live []string
	for _, p := range w.livePaths() {
		if !strings.Contains(p, "maven-metadata.xml") {
			live = append(live, strings.TrimPrefix(p, "demo-releases/"))
		}
	}
	want := []string{"com/example/demo-app/2.0-SNAPSHOT/" + appBuild + ".jar", "com/example/demo-lib/0.9/demo-lib-0.9.jar"}
	for _, b := range []string{b3, b4} {
		want = append(want, dir+b+".jar", dir+b+".jar.sha1", dir+b+".pom")
	}
	slices.Sort(want)
	if !slices.Equal(live, want) {
		t.Fatalf("live %v\nwant %v", live, want)
	}
	meta := do(t, w.addr(), "GET", "/repository/demo-releases/"+dir+"maven-metadata.xml", nil, "basic", asAdmin)
	if meta.status != 200 || strings.Contains(string(meta.body), strings.TrimPrefix(b1, "demo-lib-")) ||
		!strings.Contains(string(meta.body), strings.TrimPrefix(b4, "demo-lib-")) {
		t.Fatalf("metadata %d %s", meta.status, meta.body)
	}
	if c, err := w.srv.RemoveSnapshots(ctx, "demo-releases", 0, 0); err != nil || c.Removed != 7 {
		t.Fatalf("keep 0 %+v %v", c, err)
	}
	for _, repo := range []string{"demo-raw", "demo-none"} {
		if _, err := w.srv.RemoveSnapshots(ctx, repo, 1, 30*day); err == nil {
			t.Errorf("%s ran", repo)
		}
	}
}
