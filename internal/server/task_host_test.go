package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/scan"
	statefile "github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// The kinds of leg 6-kinds on the server: each against a known listing
// before and after, a second run that changes nothing, a freeze that stops
// it with nothing changed (ruling KN8), and errors that name no path of
// the data directory (ruling KN9).

// livePaths are the paths of the live blobs of the write store.
func (w *writing) livePaths() []string {
	w.t.Helper()
	_, live := w.scan()
	var paths []string
	for _, e := range live {
		paths = append(paths, e.Repo+"/"+e.Path)
	}
	slices.Sort(paths)
	return paths
}

func noDataPath(t *testing.T, w *writing, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), w.cfg.DataDir) {
		t.Errorf("the error names the data directory: %v", err)
	}
}

func TestTaskRebuildsMavenMetadata(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-releases": {"storage": map[string]any{"writePolicy": "ALLOW_ONCE"},
		"maven": map[string]any{"versionPolicy": "RELEASE", "layoutPolicy": "STRICT"}}, "demo-raw": storage("ALLOW")})
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if _, err := w.ingest("demo-releases", "com/example/demo-lib/"+v+"/demo-lib-"+v+".pom", "<project/>"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.ingest("demo-releases", "com/example/demo-gone/maven-metadata.xml", "<metadata/>\n"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	before := w.livePaths()
	if !slices.Equal(before, []string{"demo-releases/com/example/demo-gone/maven-metadata.xml",
		"demo-releases/com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom", "demo-releases/com/example/demo-lib/1.1.0/demo-lib-1.1.0.pom"}) {
		t.Fatalf("before %v", before)
	}
	// a freeze stops the run before its first write
	if err := w.srv.readOnly.Freeze(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.RebuildMaven(ctx, "demo-releases", tasks.MavenScope{}); !errors.Is(err, errFrozen) {
		t.Fatalf("a run while frozen: %v", err)
	}
	if got := w.livePaths(); !slices.Equal(got, before) {
		t.Fatalf("a frozen run wrote: %v", got)
	}
	if err := w.srv.readOnly.Release(); err != nil {
		t.Fatal(err)
	}
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := w.srv.RebuildMaven(stopped, "demo-releases", tasks.MavenScope{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a stopped run: %v", err)
	}
	c, err := w.srv.RebuildMaven(ctx, "demo-releases", tasks.MavenScope{})
	if err != nil || c != (tasks.MavenCounts{Written: 3, Removed: 1}) {
		t.Fatalf("%+v %v", c, err)
	}
	meta := "demo-releases/com/example/demo-lib/maven-metadata.xml"
	after := w.livePaths()
	if !slices.Equal(after, []string{before[1], before[2], meta, meta + ".md5", meta + ".sha1"}) {
		t.Fatalf("after %v", after)
	}
	row, _ := w.srv.rp.index.Lookup("demo-releases", strings.TrimPrefix(meta, "demo-releases/"))
	if props := w.properties(row.Location); props["@BlobStore.created-by"] != "system" || props["@BlobStore.created-by-ip"] != "internal" {
		t.Fatalf("KN5: %q", props)
	}
	counts, _ := w.scan()
	again, err := w.srv.RebuildMaven(ctx, "demo-releases", tasks.MavenScope{})
	if err != nil || again != (tasks.MavenCounts{Unchanged: 3}) {
		t.Fatalf("second run %+v %v", again, err)
	}
	if now, _ := w.scan(); now != counts || !slices.Equal(w.livePaths(), after) {
		t.Fatalf("the second run changed the store: %+v, before %+v", now, counts)
	}
	for repo, want := range map[string]string{"demo-raw": "is no hosted maven2 repository", "missing": "does not exist"} {
		_, err := w.srv.RebuildMaven(ctx, repo, tasks.MavenScope{})
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), repo) {
			t.Errorf("%s: %v", repo, err)
		}
	}
	if _, err := w.srv.RebuildMaven(ctx, "demo-releases", tasks.MavenScope{ArtifactID: "demo-lib"}); err == nil {
		t.Error("an artifact without its group")
	}
}

// loopingRunner is a scan runner that walks its store again and again
// until the walk stops, so a scan runs until it is cancelled.
func loopingRunner(started chan<- struct{}) scanRunner {
	var once sync.Once
	return func(s stores.Store, _ *index.Index, _ func(int)) (scan.Result, error) {
		once.Do(func() { close(started) })
		for {
			var counts stores.ScanCounts
			if err := s.Entries(&counts, func(stores.Entry) error { return nil }); err != nil {
				return scan.Result{}, err
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestTaskRescansAStore(t *testing.T) {
	w := startIngest(t, nil, spec("demo-raw", "docs/a.txt", []byte("a"), "text/plain"), spec("demo-raw", "docs/b.txt", []byte("b"), "text/plain"))
	ctx := context.Background()
	for _, path := range []string{"docs/a.txt", "docs/b.txt"} {
		if row, err := w.srv.rp.index.Lookup("demo-raw", path); row == nil || err != nil {
			t.Fatalf("before: no row of %s: %v", path, err)
		}
	}
	// remove the files of b from the store of the old product
	row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/b.txt")
	bytesFile := filepath.Join(w.old, row.Location)
	for _, f := range []string{bytesFile, strings.TrimSuffix(bytesFile, ".bytes") + ".properties"} {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.srv.readOnly.Freeze(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.Rescan(ctx, "store-a"); !errors.Is(err, errFrozen) {
		t.Fatalf("a run while frozen: %v", err)
	}
	if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/b.txt"); row == nil {
		t.Fatal("a frozen run scanned")
	}
	if err := w.srv.readOnly.Release(); err != nil {
		t.Fatal(err)
	}
	c, err := w.srv.Rescan(ctx, "store-a")
	if err != nil || c != (tasks.ScanCounts{Live: 1}) {
		t.Fatalf("%+v %v", c, err)
	}
	if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/b.txt"); row != nil {
		t.Fatalf("after: the row of b stays: %+v", row)
	}
	if row, _ := w.srv.rp.index.Lookup("demo-raw", "docs/a.txt"); row == nil {
		t.Fatal("after: the row of a is gone")
	}
	if again, err := w.srv.Rescan(ctx, "store-a"); err != nil || again != c {
		t.Fatalf("second run %+v %v", again, err)
	}
	if _, err := w.srv.Rescan(ctx, "missing"); err == nil || !strings.Contains(err.Error(), "not attached") {
		t.Fatalf("missing store: %v", err)
	}

	// KN4: one scan at a time, and a stop ends the run within a second
	started := make(chan struct{})
	w.srv.job.setRun(loopingRunner(started))
	defer w.srv.job.setRun(scan.Run)
	st, err := w.srv.rp.sync()
	if err != nil {
		t.Fatal(err)
	}
	stop := new(atomic.Bool)
	done, ok := w.srv.job.startStoppable(st.stores["store-a"], stop)
	if !ok {
		t.Fatal("no scan started")
	}
	<-started
	if _, err := w.srv.Rescan(ctx, "store-a"); err == nil || err.Error() != "a scan runs already, so no second scan starts" {
		t.Fatalf("a second scan: %v", err)
	}
	stop.Store(true)
	<-done
	started = make(chan struct{})
	w.srv.job.setRun(loopingRunner(started))
	runCtx, cancelRun := context.WithCancel(ctx)
	ended := make(chan error, 1)
	go func() {
		_, err := w.srv.Rescan(runCtx, "store-a")
		ended <- err
	}()
	<-started
	begin := time.Now()
	cancelRun()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) || time.Since(begin) > time.Second {
			t.Fatalf("%v after %s", err, time.Since(begin))
		}
	case <-time.After(time.Second):
		t.Fatal("the rescan did not stop within a second")
	}
}

// uploadSession opens a Docker upload session of repo, with a work file
// when work is true. An open ends the sessions idle for an hour, so a test
// opens its sessions first and ages them then.
func (w *writing) uploadSession(repo string, work bool) *uploadSession {
	w.t.Helper()
	sess, refused := w.srv.oci.open(repo, "app", "demo-admin")
	if sess == nil {
		w.t.Fatalf("refused at %d", refused)
	}
	w.srv.oci.mu.Lock()
	if work {
		sess.work = workFileIn(w.root(), sess.id)
		if err := os.WriteFile(sess.work, []byte("chunk"), 0o600); err != nil {
			w.t.Fatal(err)
		}
	}
	w.srv.oci.mu.Unlock()
	return sess
}

// age sets the last request of a session ago before now.
func (w *writing) age(sess *uploadSession, ago time.Duration) {
	w.srv.oci.mu.Lock()
	defer w.srv.oci.mu.Unlock()
	sess.lastUsed = w.srv.oci.clock().Add(-ago)
}

func (w *writing) sessionIDs() []string {
	w.srv.oci.mu.Lock()
	defer w.srv.oci.mu.Unlock()
	var ids []string
	for id := range w.srv.oci.sessions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func TestTaskPurgesUploadSessions(t *testing.T) {
	w := startIngest(t, nil)
	ctx := context.Background()
	stale := w.uploadSession("demo-docker", true)
	held := w.uploadSession("demo-docker", false)
	young := w.uploadSession("demo-docker", true)
	other := w.uploadSession("demo-npm", false)
	w.age(stale, 25*time.Hour)
	w.age(held, 30*time.Hour)
	w.age(young, time.Hour)
	w.age(other, 30*time.Hour)
	w.srv.oci.mu.Lock()
	held.inUse++ // a request holds it
	w.srv.oci.mu.Unlock()
	before := w.sessionIDs()
	if len(before) != 4 || len(w.workFiles()) != 2 {
		t.Fatalf("before %v %v", before, w.workFiles())
	}
	if err := w.srv.readOnly.Freeze(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.PurgeUploads(ctx, "demo-docker", 24*time.Hour); !errors.Is(err, errFrozen) {
		t.Fatalf("a run while frozen: %v", err)
	}
	if !slices.Equal(w.sessionIDs(), before) {
		t.Fatal("a frozen run ended a session")
	}
	if err := w.srv.readOnly.Release(); err != nil {
		t.Fatal(err)
	}
	c, err := w.srv.PurgeUploads(ctx, "demo-docker", 24*time.Hour)
	if err != nil || c != (tasks.PurgeCounts{Ended: 1, Kept: 2}) {
		t.Fatalf("%+v %v", c, err)
	}
	after := w.sessionIDs()
	if slices.Contains(after, stale.id) || len(after) != 3 {
		t.Fatalf("after %v", after)
	}
	if files := w.workFiles(); len(files) != 1 || filepath.Join(w.root(), "content", "tmp", files[0]) != young.work {
		t.Fatalf("work files %v", files)
	}
	if again, err := w.srv.PurgeUploads(ctx, "demo-docker", 24*time.Hour); err != nil || again != (tasks.PurgeCounts{Kept: 2}) {
		t.Fatalf("second run %+v %v", again, err)
	}
	if all, err := w.srv.PurgeUploads(ctx, "*", 24*time.Hour); err != nil || all != (tasks.PurgeCounts{Ended: 1, Kept: 2}) {
		t.Fatalf("all %+v %v", all, err)
	}
	if ids := w.sessionIDs(); slices.Contains(ids, other.id) || len(ids) != 2 {
		t.Fatalf("after * %v", ids)
	}
}

func TestTaskBacksUpTheStateFile(t *testing.T) {
	w := startIngest(t, nil)
	st, err := statefile.Create(w.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close() // the file exists now; the backup opens it on its own
	location := t.TempDir()
	file, err := w.srv.BackupState(context.Background(), location)
	if err != nil || !strings.HasPrefix(file.Name, "state-") || file.Bytes == 0 {
		t.Fatalf("%+v %v", file, err)
	}
	if info, err := os.Stat(filepath.Join(location, file.Name)); err != nil || info.Size() != file.Bytes || info.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", info, err)
	}
	// ruling KN12: the data directory and every store root, here the store of
	// the old product and the write store
	for _, place := range []string{w.cfg.DataDir, w.old, filepath.Join(w.old, "content"), w.root()} {
		_, err = w.srv.BackupState(context.Background(), place)
		if err == nil || !strings.Contains(err.Error(), "lies in the data directory or in a store") {
			t.Fatalf("a backup into %s: %v", place, err)
		}
		noDataPath(t, w, err)
	}
}

// writeWorkFile writes a work file of the store at root, last written ago.
func writeWorkFile(t *testing.T, root string, ago time.Duration) string {
	t.Helper()
	path := workFileIn(root, newUUID())
	if err := os.WriteFile(path, []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-ago)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return filepath.Base(path)
}

func TestTaskRemovesOldWorkFiles(t *testing.T) {
	w := startIngest(t, nil)
	ctx := context.Background()
	old := writeWorkFile(t, w.root(), 48*time.Hour)
	young := writeWorkFile(t, w.root(), time.Hour)
	sess := w.uploadSession("demo-docker", true)
	at := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(sess.work, at, at); err != nil {
		t.Fatal(err)
	}
	held := filepath.Base(sess.work)
	if err := os.MkdirAll(filepath.Join(w.old, "content", "tmp"), 0o750); err != nil {
		t.Fatal(err)
	}
	oldStoreFile := writeWorkFile(t, w.old, 48*time.Hour)
	// ruling KN15: a work file the writer holds stays
	writerHeld := writeWorkFile(t, w.root(), 48*time.Hour)
	release := w.srv.writes.writer.Hold(writerHeld)
	defer release()
	before := w.workFiles()
	want := []string{old, young, held, writerHeld}
	slices.Sort(want)
	if slices.Sort(before); !slices.Equal(before, want) {
		t.Fatalf("before %v", before)
	}
	if err := w.srv.readOnly.Freeze(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.RemoveWorkFiles(ctx, "local-write", 24*time.Hour); !errors.Is(err, errFrozen) {
		t.Fatalf("a run while frozen: %v", err)
	}
	if got := w.workFiles(); len(got) != 4 {
		t.Fatalf("a frozen run removed: %v", got)
	}
	if err := w.srv.readOnly.Release(); err != nil {
		t.Fatal(err)
	}
	c, err := w.srv.RemoveWorkFiles(ctx, "local-write", 24*time.Hour)
	if err != nil || c != (tasks.WorkFileCounts{Removed: 1, Kept: 3}) {
		t.Fatalf("%+v %v", c, err)
	}
	after := w.workFiles()
	want = []string{young, held, writerHeld}
	slices.Sort(want)
	if slices.Sort(after); !slices.Equal(after, want) {
		t.Fatalf("after %v", after)
	}
	if again, err := w.srv.RemoveWorkFiles(ctx, "local-write", 24*time.Hour); err != nil || again != (tasks.WorkFileCounts{Kept: 3}) {
		t.Fatalf("second run %+v %v", again, err)
	}
	// A7: a store of the old product is not touched
	_, err = w.srv.RemoveWorkFiles(ctx, "store-a", 24*time.Hour)
	if err == nil || err.Error() != "the store of the task is not a store of this server" {
		t.Fatalf("the old store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.old, "content", "tmp", oldStoreFile)); err != nil {
		t.Fatalf("the work file of the old store: %v", err)
	}
	_, err = w.srv.RemoveWorkFiles(ctx, "missing", 24*time.Hour)
	if err == nil {
		t.Fatal("a missing store")
	}
	noDataPath(t, w, err)
}

// Ruling KN18: one rebuild takes a context, a unit for each write and each
// removal, and a hook after each; the task and RebuildMavenMetadata share
// it.
func TestOneMavenRebuild(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-releases": storage("ALLOW")})
	for _, path := range []string{"com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom", "com/example/demo-gone/maven-metadata.xml"} {
		if _, err := w.ingest("demo-releases", path, "<project/>"); err != nil {
			t.Fatal(err)
		}
	}
	units := 0
	var after []string
	run := rebuildRun{ctx: context.Background(), subject: taskSubject(), address: taskAddress, after: func(path string) { after = append(after, path) },
		unit: func(context.Context) (func(), error) { units++; return func() {}, nil }}
	report, err := w.srv.rebuildMaven(run, "demo-releases", maven.Scope{})
	if err != nil || len(report.Written) != 3 || len(report.Removed) != 1 {
		t.Fatalf("%+v %v", report, err)
	}
	meta := "com/example/demo-lib/maven-metadata.xml"
	if units != 4 || !slices.Equal(after, []string{meta, meta + ".md5", meta + ".sha1", "com/example/demo-gone/maven-metadata.xml"}) {
		t.Fatalf("units %d, after %v", units, after)
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	run.ctx = stopped
	if _, err := w.srv.rebuildMaven(run, "demo-releases", maven.Scope{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a stopped rebuild: %v", err)
	}
}

// startIngestTasks is startIngest with a registry of the kinds given on a
// fake clock.
func startIngestTasks(t *testing.T, kinds map[string]tasks.Kind, list []model.Task) (*writing, *tasks.Registry, *memRuns) {
	t.Helper()
	runs := newMemRuns()
	registry := tasks.NewRegistry(tasks.NewFakeClock(taskStart), runs, tasks.RegistryOptions{Kinds: kinds})
	if err := registry.Load(list); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = registry.Close(ctx) // closed by the server already; this ends a test that failed before
	})
	sec := newFakeSecurity()
	snap := *sec.snap
	snap.Repositories = []model.Repository{{Name: "demo-raw", Format: "raw", Type: "hosted", Online: true, Attributes: storage("ALLOW")}}
	sec.snap = &snap
	base := t.TempDir()
	storeCfgs := []config.StoreConfig{blobStore(t, base, "store-a")}
	w := &writing{catalog: &fakeCatalog{}, clock: &clock{now: time.Date(2026, 11, 18, 10, 15, 30, 0, time.UTC)}, old: storeCfgs[0].Root}
	w.opts = appOptions{defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}, security: sec, writes: true, catalog: w.catalog,
		writeOptions: blobwrite.Options{Now: w.clock.Now}, tasks: registry, taskRuns: runs}
	w.app = startApp(t, base, prepare(t, base, storeCfgs, w.opts), w.opts)
	return w, registry, runs
}

// Ruling KN10: Close stops the runs and waits for their ends before it
// closes the write store; a run that writes after its stop still writes.
func TestCloseWaitsForTheRuns(t *testing.T) {
	started, wrote := make(chan struct{}), make(chan error, 1)
	var w *writing
	kind := func(model.Task) (tasks.Job, error) {
		return tasks.Job{Run: func(ctx context.Context) (string, error) {
			close(started)
			<-ctx.Done()
			time.Sleep(200 * time.Millisecond) // the end of a unit of work after the stop
			upload, err := w.srv.writes.writer.Upload(strings.NewReader("late"), -1)
			if err == nil {
				err = upload.Discard()
			}
			wrote <- err
			return "", nil
		}}, nil
	}
	w, registry, _ := startIngestTasks(t, map[string]tasks.Kind{"demo.kind": kind}, []model.Task{{ID: "demo-run", Name: "Invented task",
		TypeID: "demo.kind", Enabled: true, Schedule: model.TaskSchedule{Type: tasks.TypeManual}, Origin: model.OriginLocal}})
	if err := registry.Run("demo-run"); err != nil {
		t.Fatal(err)
	}
	<-started
	_ = w.srv.Close() // the cleanup of startApp closes it again
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("the run met a closed writer: %v", err)
		}
	default:
		t.Fatal("Close returned before the run ended")
	}
}

// rescanInBackground runs Rescan of store-a and gives its error on ended.
func rescanInBackground(w *writing, ctx context.Context) <-chan error {
	ended := make(chan error, 1)
	go func() {
		_, err := w.srv.Rescan(ctx, "store-a")
		ended <- err
	}()
	return ended
}

// Ruling KN11: a freeze that starts during a rescan stops the walk, which
// ends with "stopped: the server froze", and the freeze succeeds within a
// second instead of waiting for the scan.
func TestAFreezeStopsARescan(t *testing.T) {
	w := startIngest(t, nil, spec("demo-raw", "docs/a.txt", []byte("a"), "text/plain"))
	started := make(chan struct{})
	w.srv.job.setRun(loopingRunner(started))
	defer w.srv.job.setRun(scan.Run)
	w.srv.readOnly.SetFreezeWait(5 * time.Second)
	ended := rescanInBackground(w, context.Background())
	<-started
	begin := time.Now()
	if err := w.srv.readOnly.Freeze(); err != nil || time.Since(begin) > time.Second {
		t.Fatalf("the freeze: %v after %s", err, time.Since(begin))
	}
	select {
	case err := <-ended:
		if err == nil || err.Error() != "stopped: the server froze" {
			t.Fatalf("the rescan: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the rescan did not end")
	}
}

// Ruling KN11: a stop answers at once, and the unit of the rescan is held
// until the scan goroutine has ended, its commit phase included.
func TestAStoppedRescanHoldsItsUnitUntilTheScanEnds(t *testing.T) {
	w := startIngest(t, nil, spec("demo-raw", "docs/a.txt", []byte("a"), "text/plain"))
	started, release := make(chan struct{}), make(chan struct{})
	w.srv.job.setRun(func(stores.Store, *index.Index, func(int)) (scan.Result, error) {
		close(started)
		<-release // a commit phase that does not look at the stop
		return scan.Result{}, nil
	})
	defer w.srv.job.setRun(scan.Run)
	ctx, cancel := context.WithCancel(context.Background())
	ended := rescanInBackground(w, ctx)
	<-started
	begin := time.Now()
	cancel()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) || time.Since(begin) > time.Second {
			t.Fatalf("the stop: %v after %s", err, time.Since(begin))
		}
	case <-time.After(time.Second):
		t.Fatal("the stop did not answer within a second")
	}
	w.srv.readOnly.SetFreezeWait(200 * time.Millisecond)
	if err := w.srv.readOnly.Freeze(); err == nil {
		t.Fatal("a freeze passed while the scan goroutine still ran")
	}
	close(release)
	if !wait(func() bool { return w.srv.readOnly.Freeze() == nil }) {
		t.Fatal("the unit was not released after the scan ended")
	}
}

// wait polls cond for 5 s at most.
func wait(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// Ruling KN13: every error of the host is a tasks.Failure, a fixed text
// without a path or a record name.
func TestHostErrorsAreFailures(t *testing.T) {
	w := startIngest(t, nil)
	ctx := context.Background()
	w.uploadSession("demo-docker", false)
	writeWorkFile(t, w.root(), 48*time.Hour)
	if err := w.srv.readOnly.Freeze(); err != nil {
		t.Fatal(err)
	}
	_, frozen := w.srv.RemoveWorkFiles(ctx, "local-write", time.Hour)
	_, frozenPurge := w.srv.PurgeUploads(ctx, "*", time.Hour)
	if err := w.srv.readOnly.Release(); err != nil {
		t.Fatal(err)
	}
	_, missingRepo := w.srv.RebuildMaven(ctx, "demo-missing", tasks.MavenScope{})
	_, rawRepo := w.srv.RebuildMaven(ctx, "demo-raw", tasks.MavenScope{})
	_, missingStore := w.srv.Rescan(ctx, "demo-missing")
	_, oldStore := w.srv.RemoveWorkFiles(ctx, "store-a", time.Hour)
	_, inside := w.srv.BackupState(ctx, w.cfg.DataDir)
	errs := map[string]error{"frozen": frozen, "missing repository": missingRepo, "raw repository": rawRepo, "missing store": missingStore,
		"old store": oldStore, "backup inside": inside}
	if frozenPurge == nil {
		t.Error("a purge while frozen")
	}
	errs["frozen purge"] = frozenPurge
	for what, err := range errs {
		var f *tasks.Failure
		if !errors.As(err, &f) {
			t.Errorf("%s: %T %v is no Failure", what, err, err)
			continue
		}
		for _, word := range []string{w.cfg.DataDir, "demo-missing", "demo-raw", "store-a"} {
			if strings.Contains(f.Text, word) {
				t.Errorf("%s: %q names %s", what, f.Text, word)
			}
		}
	}
}

// stopAfter is a context whose Err is nil for n calls and Canceled after
// them: a stop that arrives in the middle of a run.
type stopAfter struct {
	context.Context
	left atomic.Int64
	done chan struct{}
	once sync.Once
}

func newStopAfter(n int64) *stopAfter {
	c := &stopAfter{Context: context.Background(), done: make(chan struct{})}
	c.left.Store(n)
	return c
}

func (c *stopAfter) Err() error {
	if c.left.Add(-1) < 0 {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	return nil
}

func (c *stopAfter) Done() <-chan struct{} { return c.done }

// Ruling KN14: every kind stops in the middle of a large listing once its
// context ends.
func TestKindsStopInALargeListing(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-releases": storage("ALLOW")})
	for i := range 60 {
		v := fmt.Sprintf("1.%d.0", i)
		if _, err := w.ingest("demo-releases", "com/example/demo-lib/"+v+"/demo-lib-"+v+".pom", "<project/>"); err != nil {
			t.Fatal(err)
		}
	}
	before := w.livePaths()
	c, err := w.srv.RebuildMaven(newStopAfter(5), "demo-releases", tasks.MavenScope{})
	if !errors.Is(err, context.Canceled) || c.Written != 0 || !slices.Equal(w.livePaths(), before) {
		t.Fatalf("maven: %+v %v", c, err)
	}
	for range 300 {
		writeWorkFile(t, w.root(), 48*time.Hour)
	}
	removed, err := w.srv.RemoveWorkFiles(newStopAfter(5), "local-write", time.Hour)
	if !errors.Is(err, context.Canceled) || removed.Removed > 5 {
		t.Fatalf("work files: %+v %v", removed, err)
	}
	var sessions []*uploadSession
	for range 50 {
		sessions = append(sessions, w.uploadSession("demo-docker", false))
	}
	for _, sess := range sessions {
		w.age(sess, 2*time.Hour)
	}
	purged, err := w.srv.PurgeUploads(newStopAfter(5), "*", time.Hour)
	if !errors.Is(err, context.Canceled) || purged.Ended > 5 {
		t.Fatalf("purge: %+v %v", purged, err)
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	statefileCreate(t, w.cfg.DataDir)
	if _, err := w.srv.BackupState(stopped, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("backup: %v", err)
	}
}

func statefileCreate(t *testing.T, dataDir string) {
	t.Helper()
	st, err := statefile.Create(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close() // the file exists now
}

// result is the value of ch, or a timeout error after 5 s.
func result(ch <-chan error) error {
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		return errors.New("no freeze within 5 s")
	}
}

// freezeAtUnit freezes the server when the run asks for its unit number
// n, and waits until the freeze waits for the changes or holds; release
// ends the freeze.
func freezeAtUnit(t *testing.T, w *writing, n int) (frozen <-chan error) {
	t.Helper()
	done := make(chan error, 1)
	units := 0
	beforeTaskUnit = func() {
		if units++; units != n {
			return
		}
		go func() { done <- w.srv.readOnly.Freeze() }()
		if !wait(w.srv.readOnly.Freezing) {
			t.Error("the freeze did not start")
		}
	}
	t.Cleanup(func() { beforeTaskUnit = nil })
	return done
}

// Finding 12: a freeze that arrives in the middle of a run of each kind
// that writes stops it after the unit that runs, and the freeze succeeds.
func TestAFreezeInTheMiddleOfARun(t *testing.T) {
	ctx := context.Background()
	t.Run("maven", func(t *testing.T) {
		w := startIngest(t, map[string]map[string]any{"demo-releases": storage("ALLOW")})
		for _, v := range []string{"1.0.0", "1.1.0"} {
			if _, err := w.ingest("demo-releases", "com/example/demo-lib/"+v+"/demo-lib-"+v+".pom", "<project/>"); err != nil {
				t.Fatal(err)
			}
		}
		frozen := freezeAtUnit(t, w, 2)
		c, err := w.srv.RebuildMaven(ctx, "demo-releases", tasks.MavenScope{})
		if !errors.Is(err, errFrozen) || c.Written != 1 || result(frozen) != nil {
			t.Fatalf("%+v %v", c, err)
		}
	})
	t.Run("purge", func(t *testing.T) {
		w := startIngest(t, nil)
		var sessions []*uploadSession
		for range 3 {
			sessions = append(sessions, w.uploadSession("demo-docker", false))
		}
		for _, sess := range sessions {
			w.age(sess, 2*time.Hour)
		}
		frozen := freezeAtUnit(t, w, 2)
		c, err := w.srv.PurgeUploads(ctx, "*", time.Hour)
		if !errors.Is(err, errFrozen) || c.Ended != 1 || result(frozen) != nil || len(w.sessionIDs()) != 2 {
			t.Fatalf("%+v %v", c, err)
		}
	})
	t.Run("work files", func(t *testing.T) {
		w := startIngest(t, nil)
		for range 3 {
			writeWorkFile(t, w.root(), 48*time.Hour)
		}
		frozen := freezeAtUnit(t, w, 2)
		c, err := w.srv.RemoveWorkFiles(ctx, "local-write", time.Hour)
		if !errors.Is(err, errFrozen) || c.Removed != 1 || result(frozen) != nil || len(w.workFiles()) != 2 {
			t.Fatalf("%+v %v", c, err)
		}
	})
}
