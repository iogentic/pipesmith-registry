package state

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

var backupAt = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

// backupData is a data directory whose state file holds two tasks and one
// capability, open for writes while the test runs.
func backupData(t *testing.T) (string, *Store) {
	t.Helper()
	dataDir := t.TempDir()
	st, err := Create(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // a test file; the checks are done
	for _, id := range []string{"demo-a", "demo-b"} {
		if err := st.PutTask(model.Task{ID: id, Name: "Invented task " + id, TypeID: "db.backup", Schedule: model.TaskSchedule{Type: "manual"},
			Properties: map[string]string{}, Origin: model.OriginLocal}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutCapability(model.Capability{ID: "demo-cap", TypeID: "baseurl", Properties: map[string]string{}, Origin: model.OriginLocal}); err != nil {
		t.Fatal(err)
	}
	return dataDir, st
}

// A backup is a new file of mode 0600 named by the time, outside the data
// directory, that opens as a state file with the same records; the data
// directory and the location hold nothing else afterwards.
func TestBackupCopiesTheStateFile(t *testing.T) {
	dataDir, st := backupData(t)
	location := t.TempDir()
	before, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	name, size, err := Backup(context.Background(), dataDir, location, nil, backupAt)
	if err != nil {
		t.Fatal(err)
	}
	if name != "state-20261120T100000Z.sqlite" || name != BackupName(backupAt) {
		t.Fatalf("name %s", name)
	}
	info, err := os.Stat(filepath.Join(location, name))
	if err != nil || info.Size() != size || size == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v size %d", info, err, size)
	}
	if entries, _ := os.ReadDir(location); len(entries) != 1 {
		t.Fatalf("the location holds %d entries", len(entries))
	}
	if after, _ := os.ReadDir(dataDir); len(after) != len(before) {
		t.Fatalf("the data directory changed: %d entries, before %d", len(after), len(before))
	}
	copyDir := t.TempDir()
	data, err := os.ReadFile(filepath.Join(location, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(copyDir), data, 0o600); err != nil {
		t.Fatal(err)
	}
	copied, err := Open(copyDir)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	want, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	got, err := copied.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(Counts(got), Counts(want)) || Counts(got)["tasks"] != 2 || Counts(got)["capabilities"] != 1 {
		t.Fatalf("counts %v, want %v", Counts(got), Counts(want))
	}
}

// The location must be an absolute, existing directory outside the data
// directory and outside every guarded store root, compared as files, also
// through a link and in another case of a file system that ignores case,
// and no directory on its way may be open to every user without the
// sticky bit; no refusal names the location, and nothing is written
// (ruling KN12).
func TestBackupRefusals(t *testing.T) {
	dataDir, _ := backupData(t)
	outside := t.TempDir()
	store := t.TempDir()
	link := filepath.Join(outside, "demo-link")
	if err := os.Symlink(dataDir, link); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dataDir, "demo-sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	storeSub := filepath.Join(store, "content")
	if err := os.Mkdir(storeSub, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(outside, "demo-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(outside, "demo-open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	inside := "lies in the data directory or in a store"
	cases := map[string]string{
		dataDir:                           inside,
		sub:                               inside,
		link:                              inside,
		filepath.Join(link, "demo-sub"):   inside,
		store:                             inside,
		storeSub:                          inside,
		filepath.Join(outside, "missing"): "is not an existing directory",
		file:                              "is not an existing directory",
		"relative/demo":                   "is not an absolute directory",
		open:                              "other users may change the location",
	}
	if upper := strings.ToUpper(dataDir); upper != dataDir {
		if _, err := os.Stat(upper); err == nil {
			cases[upper] = inside // a file system that ignores case
		} else {
			t.Logf("the file system of %s keeps case; the case of another spelling is not run", dataDir)
		}
	}
	for location, want := range cases {
		name, _, err := Backup(context.Background(), dataDir, location, []string{store}, backupAt)
		if err == nil || !strings.Contains(err.Error(), want) || name != "" {
			t.Errorf("%s: %q %v, want %q", location, name, err, want)
			continue
		}
		if strings.Contains(err.Error(), location) || strings.Contains(err.Error(), dataDir) {
			t.Errorf("%s: the refusal names a path: %v", location, err)
		}
	}
	for _, dir := range []string{dataDir, sub, store, storeSub, open} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "state-") || strings.HasPrefix(e.Name(), ".state-backup-") {
				t.Errorf("%s holds %s", dir, e.Name())
			}
		}
	}
}

// A name that exists at the place of the backup, also a link to another
// file, is never written through, and no work directory stays behind.
func TestBackupNeverWritesThroughAName(t *testing.T) {
	dataDir, _ := backupData(t)
	location, victimDir := t.TempDir(), t.TempDir()
	victim := filepath.Join(victimDir, "demo-victim")
	if err := os.WriteFile(victim, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(location, BackupName(backupAt))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Backup(context.Background(), dataDir, location, nil, backupAt); err == nil || !strings.Contains(err.Error(), "exists in the location") {
		t.Fatalf("a backup over a link: %v", err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "kept" {
		t.Fatalf("the file behind the link changed: %q", data)
	}
	entries, _ := os.ReadDir(location)
	if len(entries) != 1 {
		t.Fatalf("the location holds %d entries", len(entries))
	}
}
