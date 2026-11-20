package state

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

func demoTasks() []model.Task {
	return []model.Task{
		{ID: "demo-cron", Name: "Compact demo", TypeID: "blobstore.compact", Origin: model.OriginImport,
			Schedule:      model.TaskSchedule{Type: "cron", Cron: "0 0 12 15W,20 * ?"},
			Properties:    map[string]string{"blobstoreName": "demo-files"},
			ImportedState: map[string]string{"enabled": "true", "triggerState": "WAITING"}},
		{ID: "demo-weekly", Name: "Weekly demo", TypeID: "repository.cleanup", Enabled: true, Origin: model.OriginLocal,
			Schedule: model.TaskSchedule{Type: "weekly", StartAt: time.Date(2026, 11, 19, 9, 30, 0, 0, time.UTC),
				WeekDays: []string{"MON", "FRI"}},
			Properties: map[string]string{}},
	}
}

// A put stores a task with its schedule, a cron expression as given and
// startAt in UTC, its properties and the imported state, nil for a local
// task. Each change raises the generation; a remove of no task writes
// nothing.
func TestTasksThroughUpdate(t *testing.T) {
	st := newStore(t)
	g := generationOf(t, st)
	weekly := demoTasks()[1]
	weekly.Schedule.StartAt = weekly.Schedule.StartAt.In(time.FixedZone("CET", 3600))
	for _, task := range []model.Task{weekly, demoTasks()[0]} {
		if err := st.PutTask(task); err != nil {
			t.Fatal(err)
		}
	}
	if got := generationOf(t, st); got != g+2 {
		t.Fatalf("generation %d after two puts, want %d", got, g+2)
	}
	got, err := st.Tasks()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, demoTasks()) || got[1].Schedule.StartAt.Location() != time.UTC {
		t.Fatalf("tasks\n%+v\nwant\n%+v", got, demoTasks())
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snap.Tasks, demoTasks()) || Counts(snap)["tasks"] != 2 {
		t.Fatalf("Load gives %+v, Counts %v", snap.Tasks, Counts(snap))
	}
	g = generationOf(t, st)
	if ok, err := st.RemoveTask("demo-none"); ok || err != nil || generationOf(t, st) != g {
		t.Fatalf("remove of no task: %v %v", ok, err)
	}
	if ok, err := st.RemoveTask("demo-cron"); !ok || err != nil || generationOf(t, st) != g+1 {
		t.Fatalf("remove: %v %v", ok, err)
	}
	if got, _ := st.Tasks(); len(got) != 1 || got[0].ID != "demo-weekly" || got[0].ImportedState != nil {
		t.Fatalf("after the remove: %+v", got)
	}
	if err := st.PutTask(model.Task{ID: "demo-bad", Origin: "bootstrap"}); err == nil {
		t.Fatal("a task of origin bootstrap was stored")
	}
}

// The secret file of a property lies in the folder of the LDAP secrets,
// and two pairs of id and key never share a file.
func TestPropertySecretPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	seen := map[string]bool{}
	for _, c := range [][3]string{{SecretKindTask, "a-b", "c"}, {SecretKindTask, "a", "b-c"}, {SecretKindTask, "a+b", "c"},
		{SecretKindTask, "a", "b+c"}, {SecretKindTask, "../x", "y"}, {SecretKindTask, ".x", "y"}, {SecretKindCapability, "a-b", "c"}} {
		path := PropertySecretPath(dir, c[0], c[1], c[2])
		if filepath.Dir(path) != filepath.Join(dir, SecretsDir) || seen[path] {
			t.Fatalf("%v gives %s", c, path)
		}
		seen[path] = true
	}
	if got := filepath.Base(PropertySecretPath(dir, SecretKindTask, "demo-manual", "apiToken")); got != "task-demo-manual+apiToken" {
		t.Fatalf("name %s", got)
	}
}
