package server

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	adminrules "github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// The task settings of leg 6-api (contract S39 and S40, rulings AP5, AP6,
// KN16): the task types, the read, create, save and delete of a task on a
// fake security service whose identity store holds the tasks, with a
// registry of the built kinds and of the invented kind demo.kind.

const (
	taskTypesPath = "/api/task-types"
	taskAPIPath   = "/api/tasks"
	asTaskReader  = "demo-taskreader:taskreader-pw"
)

var taskIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// taskAPIApp is a server with security on whose store and registry hold
// demo-local, of demo.kind, which runs until it is stopped, and the
// imported demo-imported of the built type db.backup, paused (I12).
func taskAPIApp(t *testing.T) (*secureApp, *fakeSecurity, *tasks.Registry, *memRuns, chan string) {
	t.Helper()
	sec := newFakeSecurity()
	sec.addBasic(asTaskReader, subject("demo-taskreader", "", "nexus:tasks:read"))
	stored := []model.Task{
		{ID: "demo-imported", Name: "Invented imported backup", TypeID: tasks.TypeBackup, Schedule: model.TaskSchedule{Type: tasks.TypeManual},
			Properties: map[string]string{"location": "/srv/demo-backups"}, ImportedState: map[string]string{"state": "WAITING"},
			Origin: model.OriginImport},
		{ID: "demo-local", Name: "Invented local task", TypeID: "demo.kind", Enabled: true, Schedule: model.TaskSchedule{Type: tasks.TypeManual},
			Properties: map[string]string{}, Origin: model.OriginLocal},
	}
	sec.snap.Tasks = stored
	runs, started := newMemRuns(), make(chan string, 4)
	kinds := tasks.Kinds(&tasks.HostRef{})
	kinds["demo.kind"] = func(task model.Task) (tasks.Job, error) {
		return tasks.Job{Run: func(ctx context.Context) (string, error) {
			started <- task.Name
			<-ctx.Done()
			return "", nil
		}}, nil
	}
	registry := tasks.NewRegistry(tasks.NewFakeClock(taskStart), runs, tasks.RegistryOptions{Kinds: kinds})
	if err := registry.Load(stored); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := registry.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return &secureApp{app: startTasksApp(t, sec, registry, runs), sec: sec}, sec, registry, runs, started
}

func sendTask(t *testing.T, a *secureApp, method, target, body string, headers ...string) *answer {
	t.Helper()
	if len(headers) == 0 {
		headers = []string{"basic", asAdmin}
	}
	return do(t, a.addr(), method, target, []byte(body), append(headers, "Content-Type", jsonType)...)
}

// refusedFields are the ids of a 400 with a list of field errors.
func refusedFields(t *testing.T, r *answer) []string {
	t.Helper()
	status(t, r, 400)
	var list []struct{ ID, Message string }
	if err := json.Unmarshal(r.body, &list); err != nil {
		t.Fatalf("%v %s", err, r.body)
	}
	var ids []string
	for _, e := range list {
		ids = append(ids, e.ID)
	}
	return ids
}

func storedTask(sec *fakeSecurity, id string) (model.Task, bool) {
	list := sec.Snapshot().Tasks
	at := slices.IndexFunc(list, func(x model.Task) bool { return x.ID == id })
	if at < 0 {
		return model.Task{}, false
	}
	return list[at], true
}

func sameJSON(t *testing.T, got []byte, want any) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("%v %s", err, got)
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("answer %s, want %s", got, data)
	}
}

// GET /api/task-types answers the built types of tasks.TypeList, read from
// the list itself, with nexus:tasks:read.
func TestTaskTypesAreTheBuiltTypes(t *testing.T) {
	a, _, _, _, _ := taskAPIApp(t)
	types := tasks.TypeList()
	if len(types) == 0 {
		t.Fatal("no built type")
	}
	for _, who := range []string{asAdmin, asTaskReader} {
		r := a.send("GET", taskTypesPath, "basic", who)
		status(t, r, 200)
		sameJSON(t, r.body, types)
	}
	status(t, a.send("GET", taskTypesPath, "basic", asReader), 403)
}

func TestTaskCreateReadSaveDelete(t *testing.T) {
	a, sec, registry, _, _ := taskAPIApp(t)
	created := sendTask(t, a, "POST", taskAPIPath, `{"typeId":"db.backup","name":"  Nightly backup ","enabled":true,`+
		`"schedule":{"type":"daily","startAt":"2026-11-20T03:00:00.000Z"},"properties":{"location":"/srv/backups"}}`)
	status(t, created, 201)
	answer := created.object(t)
	id, _ := answer["id"].(string)
	if !taskIDPattern.MatchString(id) || len(answer) != 1 {
		t.Fatalf("created %s, want {id} with 16 lowercase hex characters", created.body)
	}
	// the registry takes the task at once
	s, ok := registry.Status(id)
	if !ok || s.Name != "Nightly backup" || !s.Enabled || !s.HasNext || !s.Next.Equal(time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("registered %+v %v", s, ok)
	}
	if task, ok := storedTask(sec, id); !ok || task.Origin != model.OriginLocal || task.Properties["location"] != "/srv/backups" {
		t.Fatalf("stored %+v %v", task, ok)
	}
	settings := map[string]any{"id": id, "typeId": "db.backup", "name": "Nightly backup", "enabled": true,
		"schedule": map[string]any{"type": "daily", "startAt": "2026-11-20T03:00:00Z"}, "properties": map[string]any{"location": "/srv/backups"}}
	for _, who := range []string{asAdmin, asTaskReader} {
		r := a.send("GET", taskAPIPath+"/"+id, "basic", who)
		status(t, r, 200)
		sameJSON(t, r.body, settings)
		if leaks := adminrules.Leaks(r.json(t), ""); len(leaks) > 0 {
			t.Fatalf("the settings hold a secret at %v", leaks)
		}
	}
	other := sendTask(t, a, "POST", taskAPIPath, `{"typeId":"blobstore.rescan","name":"Rescan","schedule":{"type":"manual"},"properties":{"blobStoreName":"store-a"}}`)
	status(t, other, 201)
	if other.object(t)["id"] == id {
		t.Fatal("two creates gave one id")
	}
	saved := sendTask(t, a, "PUT", taskAPIPath+"/"+id, `{"id":"ignored","typeId":"db.backup","name":"Weekly backup","enabled":false,`+
		`"schedule":{"type":"weekly","startAt":"2026-11-20T03:00:00Z","weekDays":["MON"]},"properties":{"location":"/srv/backups"}}`)
	status(t, saved, 204)
	if saved.header.Get("Cache-Control") != "no-store" || len(saved.body) != 0 {
		t.Fatalf("save answer %v %s", saved.header, saved.body)
	}
	if s, _ := registry.Status(id); s.Name != "Weekly backup" || s.Enabled || s.Schedule.Type != tasks.TypeWeekly || s.HasNext {
		t.Fatalf("saved %+v", s)
	}
	if ids := refusedFields(t, sendTask(t, a, "PUT", taskAPIPath+"/"+id, `{"typeId":"blobstore.rescan","name":"x","schedule":{"type":"manual"},`+
		`"properties":{"blobStoreName":"store-a"}}`)); !slices.Equal(ids, []string{"typeId"}) {
		t.Fatalf("a change of the type: %v", ids)
	}
	// the rights of the contract: a reader of the tasks changes none
	for _, c := range []struct{ method, target string }{{"POST", taskAPIPath}, {"PUT", taskAPIPath + "/" + id}, {"DELETE", taskAPIPath + "/" + id}} {
		status(t, sendTask(t, a, c.method, c.target, `{}`, "basic", asTaskReader), 403)
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		status(t, sendTask(t, a, method, taskAPIPath+"/0000000000000000", `{"typeId":"db.backup","name":"x","schedule":{"type":"manual"}}`), 404)
	}
	deleted := sendTask(t, a, "DELETE", taskAPIPath+"/"+id, "")
	status(t, deleted, 204)
	if _, ok := registry.Status(id); ok {
		t.Fatal("a deleted task is registered")
	}
	if _, ok := storedTask(sec, id); ok {
		t.Fatal("a deleted task is stored")
	}
	status(t, a.send("GET", taskAPIPath+"/"+id, "basic", asAdmin), 404)
}

// KN16: for each built type of tasks.TypeList, the create and the save
// refuse at the same property what the load of a stored task refuses, with
// the reason of the load where the value itself is refused. A type whose
// load refuses no property is created and saved with any value.
func TestTaskSavesRefuseWhatLoadRefuses(t *testing.T) {
	// the kind of repository.cleanup reads an empty repositoryName as *
	refusesNone := map[string]bool{tasks.TypeCleanup: true}
	refused := map[string]map[string]string{
		tasks.TypeCompact:         {"blobStoreName": "store-a", "olderThanDays": "0"},
		tasks.TypeRemoveSnapshots: {"repositoryName": "*", "minimumRetained": "1", "snapshotRetentionDays": "30"},
		tasks.TypeBackup:          {"location": "relative/backups"},
		tasks.TypeMavenMetadata:   {"repositoryName": "*"},
		tasks.TypeRescan:          {"blobStoreName": ""},
		tasks.TypeUploadPurge:     {"repositoryName": "demo-docker", "olderThanHours": "0"},
		tasks.TypeWorkFiles:       {"blobStoreName": "store-a", "olderThanHours": "87601"},
	}
	accepted := map[string]map[string]string{
		tasks.TypeCompact:         {"blobStoreName": "store-a", "olderThanDays": "7"},
		tasks.TypeCleanup:         {"repositoryName": "demo-maven"},
		tasks.TypeRemoveSnapshots: {"repositoryName": "demo-maven", "minimumRetained": "1", "snapshotRetentionDays": "30"},
		tasks.TypeBackup:          {"location": "/srv/backups"},
		tasks.TypeMavenMetadata:   {"repositoryName": "demo-maven"},
		tasks.TypeRescan:          {"blobStoreName": "store-a"},
		tasks.TypeUploadPurge:     {"repositoryName": "demo-docker", "olderThanHours": "24"},
		tasks.TypeWorkFiles:       {"blobStoreName": "store-a", "olderThanHours": "24"},
	}
	a, _, _, _, _ := taskAPIApp(t)
	body := func(typeID string, properties map[string]string) string {
		data, err := json.Marshal(map[string]any{"typeId": typeID, "name": "Invented " + typeID, "schedule": map[string]string{"type": "manual"},
			"properties": properties})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for _, tt := range tasks.TypeList() {
		if refusesNone[tt.ID] {
			for _, properties := range []map[string]string{{}, {"repositoryName": ""}, accepted[tt.ID]} {
				if err := tasks.CheckProperties(tt.ID, properties); err != nil {
					t.Fatalf("%s: the kind refuses %v: %v", tt.ID, properties, err)
				}
			}
			good := sendTask(t, a, "POST", taskAPIPath, body(tt.ID, accepted[tt.ID]))
			status(t, good, 201)
			id := good.object(t)["id"].(string)
			status(t, sendTask(t, a, "PUT", taskAPIPath+"/"+id, body(tt.ID, map[string]string{})), 204)
			continue
		}
		bad, known := refused[tt.ID]
		if !known {
			t.Errorf("the built type %s has no refused properties in this test", tt.ID)
			continue
		}
		var field *tasks.PropertyError
		if !errors.As(tasks.CheckProperties(tt.ID, bad), &field) {
			t.Fatalf("%s: the kind takes %v", tt.ID, bad)
		}
		load := tasks.NewRegistry(tasks.NewFakeClock(taskStart), newMemRuns(), tasks.RegistryOptions{Kinds: tasks.Kinds(&tasks.HostRef{})})
		if err := load.Load([]model.Task{{ID: "demo-load", Name: "x", TypeID: tt.ID, Schedule: model.TaskSchedule{Type: tasks.TypeManual},
			Properties: bad, Origin: model.OriginLocal}}); err != nil {
			t.Fatal(err)
		}
		s, _ := load.Status("demo-load")
		reason, cut := strings.CutPrefix(s.Reason, "the task demo-load cannot run: ")
		if !cut || s.Enabled {
			t.Fatalf("%s: the load gives %+v", tt.ID, s)
		}
		_ = load.Close(context.Background())
		check := func(label string, r *answer) {
			t.Helper()
			status(t, r, 400)
			var list []struct{ ID, Message string }
			if err := json.Unmarshal(r.body, &list); err != nil || len(list) != 1 || list[0].ID != "properties."+field.Field {
				t.Fatalf("%s of %s: %s, want one refusal at properties.%s", label, tt.ID, r.body, field.Field)
			}
			if bad[field.Field] != "" && list[0].Message != reason {
				t.Fatalf("%s of %s: %q, want the reason of the load %q", label, tt.ID, list[0].Message, reason)
			}
		}
		check("the create", sendTask(t, a, "POST", taskAPIPath, body(tt.ID, bad)))
		good := sendTask(t, a, "POST", taskAPIPath, body(tt.ID, accepted[tt.ID]))
		status(t, good, 201)
		id := good.object(t)["id"].(string)
		check("the save", sendTask(t, a, "PUT", taskAPIPath+"/"+id, body(tt.ID, bad)))
		status(t, sendTask(t, a, "PUT", taskAPIPath+"/"+id, body(tt.ID, accepted[tt.ID])), 204)
	}
}

// AP5: a save of a running task applies from its next run; the delete of a
// running task answers 409 and keeps it.
func TestTaskChangesOfARunningTask(t *testing.T) {
	a, sec, registry, runs, started := taskAPIApp(t)
	status(t, a.send("POST", tasksPath+"/demo-local/run", "basic", asAdmin), 204)
	if got := receiveID(t, started); got != "Invented local task" {
		t.Fatalf("started %q", got)
	}
	status(t, sendTask(t, a, "PUT", taskAPIPath+"/demo-local", `{"typeId":"demo.kind","name":"Renamed while running","enabled":true,`+
		`"schedule":{"type":"daily","startAt":"2026-11-20T03:00:00Z"},"properties":{}}`), 204)
	if s, _ := registry.Status("demo-local"); !s.Running || s.Name != "Renamed while running" || s.HasNext {
		t.Fatalf("after the save %+v", s)
	}
	r := sendTask(t, a, "DELETE", taskAPIPath+"/demo-local", "")
	if r.status != 409 || !strings.Contains(r.object(t)["message"].(string), "running") {
		t.Fatalf("the delete of a running task: %d %s", r.status, r.body)
	}
	if _, ok := registry.Status("demo-local"); !ok {
		t.Fatal("a refused delete removed the task from the registry")
	}
	if _, ok := storedTask(sec, "demo-local"); !ok {
		t.Fatal("a refused delete removed the stored task")
	}
	status(t, a.send("POST", tasksPath+"/demo-local/stop", "basic", asAdmin), 204)
	select {
	case <-runs.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, _ := registry.Status("demo-local")
		if s.HasNext && s.Next.Equal(time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the run the task waits for %+v, want the saved schedule", s)
		}
		time.Sleep(10 * time.Millisecond)
	}
	status(t, a.send("POST", tasksPath+"/demo-local/run", "basic", asAdmin), 204)
	if got := receiveID(t, started); got != "Renamed while running" {
		t.Fatalf("the next run did the work of %q, want the saved task", got)
	}
	status(t, a.send("POST", tasksPath+"/demo-local/stop", "basic", asAdmin), 204)
	<-runs.ended
	status(t, sendTask(t, a, "DELETE", taskAPIPath+"/demo-local", ""), 204)
}

// I12: an imported task is paused until it is saved enabled; the save makes
// it a task of origin local, which runs.
func TestImportedTaskSavedEnabledLeavesThePausedState(t *testing.T) {
	a, sec, registry, _, _ := taskAPIApp(t)
	if s, _ := registry.Status("demo-imported"); s.Enabled || !strings.Contains(s.Reason, "imported and stays paused") {
		t.Fatalf("imported %+v", s)
	}
	r := a.send("GET", taskAPIPath+"/demo-imported", "basic", asAdmin)
	status(t, r, 200)
	sameJSON(t, r.body, map[string]any{"id": "demo-imported", "typeId": "db.backup", "name": "Invented imported backup", "enabled": false,
		"schedule": map[string]any{"type": "manual"}, "properties": map[string]any{"location": "/srv/demo-backups"}})
	status(t, sendTask(t, a, "PUT", taskAPIPath+"/demo-imported", `{"typeId":"db.backup","name":"Invented imported backup","enabled":true,`+
		`"schedule":{"type":"manual"},"properties":{"location":"/srv/demo-backups"}}`), 204)
	if s, _ := registry.Status("demo-imported"); !s.Enabled || s.Reason != "" {
		t.Fatalf("saved enabled %+v", s)
	}
	if task, _ := storedTask(sec, "demo-imported"); task.Origin != model.OriginLocal || !task.Enabled || task.ImportedState != nil {
		t.Fatalf("stored %+v", task)
	}
}

// The refusals of a body: no object, a time that cannot be read, a value
// that is no string and a secret key of a task of a type without a check.
func TestTaskSettingsRefusals(t *testing.T) {
	a, sec, _, _, _ := taskAPIApp(t)
	for _, body := range []string{`[]`, ``, `{`} {
		if ids := refusedFields(t, sendTask(t, a, "POST", taskAPIPath, body)); !slices.Equal(ids, []string{"*"}) {
			t.Fatalf("body %q: %v", body, ids)
		}
	}
	if ids := refusedFields(t, sendTask(t, a, "POST", taskAPIPath, `{"typeId":"db.backup","name":"x","schedule":{"type":"daily","startAt":"tomorrow"},`+
		`"properties":{"location":"/srv/x"}}`)); !slices.Equal(ids, []string{"schedule.startAt"}) {
		t.Fatalf("a time that cannot be read: %v", ids)
	}
	if ids := refusedFields(t, sendTask(t, a, "POST", taskAPIPath, `{"typeId":"demo.kind","name":"x","schedule":{"type":"manual"}}`)); !slices.Equal(ids, []string{"typeId"}) {
		t.Fatalf("a type that is not built: %v", ids)
	}
	if ids := refusedFields(t, sendTask(t, a, "PUT", taskAPIPath+"/demo-local", `{"typeId":"demo.kind","name":"x","schedule":{"type":"manual"},`+
		`"properties":{"count":3,"password":"example-task-secret-1"}}`)); !slices.Equal(ids, []string{"properties.count", "properties.password"}) {
		t.Fatalf("a number and a secret key: %v", ids)
	}
	if task, _ := storedTask(sec, "demo-local"); task.Name != "Invented local task" || len(task.Properties) != 0 {
		t.Fatalf("a refused save changed %+v", task)
	}
}

// AP6: the writes take the request token through a session cookie and stay
// behind freezeGate: while frozen they answer 503 with the reason and change
// nothing; the reads pass.
func TestTaskWritesNeedTheRequestTokenAndStopWhileFrozen(t *testing.T) {
	a, sec, registry, _, _ := taskAPIApp(t)
	writes := []struct{ method, target, body string }{
		{"POST", taskAPIPath, `{"typeId":"blobstore.rescan","name":"Rescan","schedule":{"type":"manual"},"properties":{"blobStoreName":"store-a"}}`},
		{"PUT", taskAPIPath + "/demo-local", `{"typeId":"demo.kind","name":"Frozen","schedule":{"type":"manual"},"properties":{}}`},
		{"DELETE", taskAPIPath + "/demo-local", ``},
	}
	for _, c := range writes {
		r := sendTask(t, a, c.method, c.target, c.body, "Cookie", "fake-session=s1")
		if r.status != 403 || r.object(t)["message"] != "missing or wrong request token" {
			t.Errorf("%s %s without the request token: %d %s", c.method, c.target, r.status, r.body)
		}
		for _, p := range unfrozen {
			if p.MatchString(c.target) {
				t.Fatalf("a task write is in unfrozen: %s", p)
			}
		}
	}
	status(t, sendTask(t, a, "PUT", taskAPIPath+"/demo-local", `{"typeId":"demo.kind","name":"With the token","schedule":{"type":"manual"},"properties":{}}`,
		"Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1"), 204)
	status(t, do(t, a.addr(), "POST", "/service/rest/v1/read-only/freeze", nil, "basic", asAdmin), 204)
	reason := readOnlyOf(t, a).SummaryReason
	if reason == "" {
		t.Fatal("a freeze without a reason")
	}
	before := len(sec.Snapshot().Tasks)
	for _, c := range writes {
		r := sendTask(t, a, c.method, c.target, c.body)
		if r.status != 503 || r.object(t)["message"] != reason {
			t.Errorf("%s %s while frozen: %d %s", c.method, c.target, r.status, r.body)
		}
	}
	if task, _ := storedTask(sec, "demo-local"); len(sec.Snapshot().Tasks) != before || task.Name != "With the token" {
		t.Fatalf("a write while frozen changed the tasks: %+v", sec.Snapshot().Tasks)
	}
	if s, _ := registry.Status("demo-local"); s.Name != "With the token" {
		t.Fatalf("a write while frozen changed the registry: %+v", s)
	}
	status(t, a.send("GET", taskAPIPath+"/demo-local", "basic", asAdmin), 200)
	status(t, a.send("GET", taskTypesPath, "basic", asAdmin), 200)
	status(t, a.send("POST", "/service/rest/v1/read-only/release", "basic", asAdmin), 204)
	status(t, sendTask(t, a, "DELETE", taskAPIPath+"/demo-local", ""), 204)
}
