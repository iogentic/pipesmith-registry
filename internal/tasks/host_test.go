package tasks

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// fakeHost records each call and answers with fixed counts.
type fakeHost struct {
	mu    sync.Mutex
	calls []string
	fail  error
	block bool // a call waits for the end of its context
}

func (f *fakeHost) record(call string, ctx context.Context) error {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	fail, block := f.fail, f.block
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return fail
}

func (f *fakeHost) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeHost) RebuildMaven(ctx context.Context, repository string, scope MavenScope) (MavenCounts, error) {
	return MavenCounts{Written: 3, Unchanged: 2, Removed: 1}, f.record(fmt.Sprintf("maven %s %+v", repository, scope), ctx)
}

func (f *fakeHost) Rescan(ctx context.Context, store string) (ScanCounts, error) {
	return ScanCounts{Live: 7, Deleted: 1}, f.record("rescan "+store, ctx)
}

func (f *fakeHost) PurgeUploads(ctx context.Context, repository string, age time.Duration) (PurgeCounts, error) {
	return PurgeCounts{Ended: 2, Kept: 1}, f.record(fmt.Sprintf("purge %s %s", repository, age), ctx)
}

func (f *fakeHost) BackupState(ctx context.Context, location string) (BackupFile, error) {
	return BackupFile{Name: "state-20261120T100000Z.sqlite", Bytes: 4096}, f.record("backup "+location, ctx)
}

func (f *fakeHost) RemoveWorkFiles(ctx context.Context, store string, age time.Duration) (WorkFileCounts, error) {
	return WorkFileCounts{Removed: 4, Kept: 2}, f.record(fmt.Sprintf("work %s %s", store, age), ctx)
}

func TestHostRef(t *testing.T) {
	ref := &HostRef{}
	if h, err := ref.Host(); h != nil || !errors.Is(err, ErrNoHost) {
		t.Fatalf("%v %v", h, err)
	}
	host := &fakeHost{}
	ref.Set(host)
	if h, err := ref.Host(); h != Host(host) || err != nil {
		t.Fatalf("%v %v", h, err)
	}
	ref.Set(nil)
	if _, err := ref.Host(); !errors.Is(err, ErrNoHost) {
		t.Fatal(err)
	}
}

// Kinds holds a kind for each built type of the table and for no other.
func TestKindsAreTheBuiltTypes(t *testing.T) {
	var built []string
	for _, tt := range TypeList() {
		built = append(built, tt.ID)
	}
	got := slices.Sorted(maps.Keys(Kinds(&HostRef{})))
	slices.Sort(built)
	if len(got) == 0 || !slices.Equal(got, built) {
		t.Fatalf("kinds %v, want %v", got, built)
	}
}

func task(id, typeID string, properties map[string]string) model.Task {
	return model.Task{ID: id, Name: "Task " + id, TypeID: typeID, Enabled: true, Schedule: manual, Properties: properties,
		Origin: model.OriginLocal}
}

// runOnce registers one task on the kinds of ref, runs it, and gives the
// row of its end.
func runOnce(t *testing.T, ref *HostRef, tk model.Task) memRun {
	t.Helper()
	_, store, r := setUpRegistry(t, Kinds(ref), "/srv/demo-data")
	r.host = ref
	if err := r.Load([]model.Task{tk}); err != nil {
		t.Fatal(err)
	}
	if s, _ := r.Status(tk.ID); !s.Enabled {
		t.Fatalf("%s is disabled: %s", tk.ID, s.Reason)
	}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(tk.ID); err != nil {
		t.Fatal(err)
	}
	return store.waitEnd(t)
}

// A run before the server sets the host fails with a message; SetHost of
// the registry sets the host of its kinds.
func TestARunBeforeTheHostFails(t *testing.T) {
	ref := &HostRef{}
	_, store, r := setUpRegistry(t, Kinds(ref), "")
	r2 := NewRegistry(NewFakeClock(t0), newMemStore(), RegistryOptions{Kinds: Kinds(ref), Host: ref})
	if err := r.Load([]model.Task{task("demo-rescan", TypeRescan, map[string]string{"blobStoreName": "demo-store"})}); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.Run("demo-rescan"); err != nil {
		t.Fatal(err)
	}
	if row := store.waitEnd(t); row.Result != ResultFailed || row.Message != ErrNoHost.Error() {
		t.Fatalf("%+v", row)
	}
	host := &fakeHost{}
	r2.SetHost(host)
	if h, err := ref.Host(); h != Host(host) || err != nil {
		t.Fatalf("SetHost did not set the host: %v %v", h, err)
	}
	if err := r.Run("demo-rescan"); err != nil {
		t.Fatal(err)
	}
	if row := store.waitEnd(t); row.Result != ResultOK || row.Message != "store rescanned: 7 live, 1 deleted, 0 copies, 0 without bytes, 0 unparsable" {
		t.Fatalf("%+v", row)
	}
	r2.SetHost(nil)
	if _, err := ref.Host(); !errors.Is(err, ErrNoHost) {
		t.Fatal("SetHost(nil) left the host")
	}
}

// Each built kind calls its host with the properties of its task and ends
// with a message of counts and record names.
func TestBuiltKindsCallTheHost(t *testing.T) {
	cases := []struct {
		task    model.Task
		call    string
		message string
	}{
		{task("demo-maven", TypeMavenMetadata, map[string]string{"repositoryName": "demo-releases"}),
			"maven demo-releases {GroupID: ArtifactID: BaseVersion:}",
			"maven metadata rebuilt: 3 written, 2 unchanged, 1 removed, 0 kept in a read only store"},
		{task("demo-maven-scope", TypeMavenMetadata, map[string]string{"repositoryName": "demo-releases", "groupId": "com.example",
			"artifactId": "demo-lib", "baseVersion": "1.0"}),
			"maven demo-releases {GroupID:com.example ArtifactID:demo-lib BaseVersion:1.0}",
			"maven metadata rebuilt: 3 written, 2 unchanged, 1 removed, 0 kept in a read only store"},
		{task("demo-rescan", TypeRescan, map[string]string{"blobStoreName": "demo-store"}), "rescan demo-store",
			"store rescanned: 7 live, 1 deleted, 0 copies, 0 without bytes, 0 unparsable"},
		{task("demo-purge", TypeUploadPurge, map[string]string{"repositoryName": "*", "olderThanHours": "24"}), "purge * 24h0m0s",
			"docker upload sessions: 2 ended, 1 kept"},
		{task("demo-purge-default", TypeUploadPurge, map[string]string{"repositoryName": "demo-docker"}), "purge demo-docker 24h0m0s",
			"docker upload sessions: 2 ended, 1 kept"},
		{task("demo-backup", TypeBackup, map[string]string{"location": "/srv/demo-backup"}), "backup /srv/demo-backup",
			"state file copied to state-20261120T100000Z.sqlite, 4096 bytes"},
		{task("demo-work", TypeWorkFiles, map[string]string{"blobStoreName": "local-write", "olderThanHours": "2"}), "work local-write 2h0m0s",
			"work files: 4 removed, 2 kept"},
	}
	for _, c := range cases {
		host := &fakeHost{}
		ref := &HostRef{}
		ref.Set(host)
		row := runOnce(t, ref, c.task)
		if calls := host.recorded(); !slices.Equal(calls, []string{c.call}) {
			t.Errorf("%s: calls %q, want %q", c.task.ID, calls, c.call)
		}
		if row.Result != ResultOK || row.Message != c.message {
			t.Errorf("%s: %+v, want %q", c.task.ID, row, c.message)
		}
	}
}

// A property the kind cannot take disables the task with a reason that
// names the property and never its value (ruling KN9).
func TestPropertiesThatDisableATask(t *testing.T) {
	cases := []model.Task{
		task("demo-1", TypeMavenMetadata, map[string]string{}),
		task("demo-2", TypeMavenMetadata, map[string]string{"repositoryName": "demo-releases", "artifactId": "demo-secretvalue"}),
		task("demo-3", TypeRescan, map[string]string{"blobStoreName": ""}),
		task("demo-4", TypeUploadPurge, map[string]string{"repositoryName": "*", "olderThanHours": "-3"}),
		task("demo-5", TypeUploadPurge, map[string]string{"repositoryName": "*", "olderThanHours": "1.5x"}),
		task("demo-6", TypeWorkFiles, map[string]string{"blobStoreName": "local-write", "olderThanHours": "99999999"}),
		task("demo-7", TypeBackup, map[string]string{"location": "relative/demo-secretvalue"}),
		task("demo-8", TypeBackup, map[string]string{}),
	}
	_, _, r := setUpRegistry(t, Kinds(&HostRef{}), "")
	if err := r.Load(cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		s, _ := r.Status(c.ID)
		if s.Enabled || !strings.Contains(s.Reason, "the task "+c.ID+" cannot run: the propert") {
			t.Errorf("%s: %+v", c.ID, s)
		}
		for _, v := range c.Properties {
			if v != "" && strings.Contains(s.Reason, v) {
				t.Errorf("%s: the reason %q holds the value %q", c.ID, s.Reason, v)
			}
		}
	}
}

// The failure of the host is the failure of the run; a stop ends a run of
// a host that waits for its context within one second (ruling KN8); the
// location of a backup is a secret of its task, so no message shows it.
func TestKindFailuresAndStop(t *testing.T) {
	host := &fakeHost{fail: &Failure{Text: "the store of the task is not a store of this server"}}
	ref := &HostRef{}
	ref.Set(host)
	if row := runOnce(t, ref, task("demo-work", TypeWorkFiles, map[string]string{"blobStoreName": "demo-old"})); row.Result != ResultFailed ||
		row.Message != "the store of the task is not a store of this server" {
		t.Fatalf("%+v", row)
	}
	host.fail = errors.New("cannot write /srv/demo-backup/x")
	if row := runOnce(t, ref, task("demo-backup", TypeBackup, map[string]string{"location": "/srv/demo-backup"})); row.Result != ResultFailed ||
		strings.Contains(row.Message, "/srv/demo-backup") {
		t.Fatalf("%+v", row)
	}

	blocking := &fakeHost{block: true}
	ref.Set(blocking)
	_, store, r := setUpRegistry(t, Kinds(ref), "")
	if err := r.Load([]model.Task{task("demo-rescan", TypeRescan, map[string]string{"blobStoreName": "demo-store"})}); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if err := r.Run("demo-rescan"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(guard)
	for len(blocking.recorded()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	if err := r.Stop("demo-rescan"); err != nil {
		t.Fatal(err)
	}
	row := store.waitEnd(t)
	if took := time.Since(start); took > time.Second || row.Result != ResultCanceled || row.Message != "stopped" {
		t.Fatalf("%+v after %s", row, took)
	}
}

// Ruling KN13: a run shows the text of a Failure of its host, and every
// other error of the host as a fixed text, never its path or system text.
func TestHostErrorsAreFixedTexts(t *testing.T) {
	ref := &HostRef{}
	ref.Set(&fakeHost{fail: errors.New("open /srv/demo-store/content/tmp/x: permission denied")})
	row := runOnce(t, ref, task("demo-work", TypeWorkFiles, map[string]string{"blobStoreName": "demo-store"}))
	if row.Result != ResultFailed || row.Message != "the run failed; the log of the server names the cause" {
		t.Fatalf("%+v", row)
	}
	ref.Set(&fakeHost{fail: fmt.Errorf("demo: %w", &Failure{Text: "the server is frozen, so the run stops"})})
	row = runOnce(t, ref, task("demo-work", TypeWorkFiles, map[string]string{"blobStoreName": "demo-store"}))
	if row.Result != ResultFailed || row.Message != "the server is frozen, so the run stops" {
		t.Fatalf("%+v", row)
	}
}
