package importer_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/importer"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// The files of the cleanup policies, capabilities and tasks of the content
// set (tests/import/FixtureDb.java).
var administrationFiles = []string{"cleanup_policy.jsonl", "capability_storage_item.jsonl", "qrtz_job_details.jsonl",
	"qrtz_triggers.jsonl"}

// jsonlRows are the rows of a file of an export, by the value of key.
func jsonlRows(t *testing.T, dir, file, key string) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		rows[r[key].(string)] = r
	}
	return rows
}

// The content fixture holds invented rows of the cleanup policies, the
// capabilities and the tasks with their triggers. The export took out the
// secret keys and listed them, and the job data it cannot read is null and
// listed; the importer reads the set.
func TestContentFixtureHoldsTheAdministrationTables(t *testing.T) {
	var m struct {
		Files []struct {
			File    string `json:"file"`
			Rows    int    `json:"rows"`
			Removed int    `json:"removed"`
		} `json:"files"`
	}
	data, err := os.ReadFile(filepath.Join(fixtureContent, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	counts := map[string][2]int{}
	for _, f := range m.Files {
		counts[f.File] = [2]int{f.Rows, f.Removed}
	}
	want := map[string][2]int{"cleanup_policy.jsonl": {3, 0}, "capability_storage_item.jsonl": {3, 1},
		"qrtz_job_details.jsonl": {6, 2}, "qrtz_triggers.jsonl": {6, 0}}
	for file, w := range want {
		if counts[file] != w {
			t.Errorf("%s: rows and removed %v, want %v", file, counts[file], w)
		}
	}
	for _, file := range administrationFiles {
		data, err := os.ReadFile(filepath.Join(fixtureContent, file))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "CANARY-") {
			t.Errorf("%s holds a canary", file)
		}
	}
	jobs := jsonlRows(t, fixtureContent, "qrtz_job_details.jsonl", "job_name")
	weekly := jobs["demo-weekly"]
	if config, _ := weekly["job_data"].(map[string]any); config[".typeId"] != "repository.maven.remove-snapshots" ||
		!slices.Equal(anyStrings(weekly["_removed"]), []string{"job_data.remotePassword"}) {
		t.Errorf("demo-weekly: %v", weekly)
	}
	if config, _ := jobs["demo-cron"]["job_data"].(map[string]any); config["blobstoreName"] != "demo-files" ||
		config[".enabled"] != "true" {
		t.Errorf("demo-cron: %v", jobs["demo-cron"])
	}
	if u := jobs["demo-unreadable"]; u["job_data"] != nil || !slices.Equal(anyStrings(u["_removed"]), []string{"job_data"}) {
		t.Errorf("demo-unreadable: %v", u)
	}
	triggers := jsonlRows(t, fixtureContent, "qrtz_triggers.jsonl", "trigger_name")
	if s, _ := triggers["demo-stricter-trigger"]["job_data"].(map[string]any); s["schedule.cronExpression"] != "0 0 12 15W,20 * ?" {
		t.Errorf("demo-stricter-trigger: %v", triggers["demo-stricter-trigger"])
	}
	capabilities := jsonlRows(t, fixtureContent, "capability_storage_item.jsonl", "type")
	if hook := capabilities["demo.webhook"]; !slices.Equal(anyStrings(hook["_removed"]), []string{"properties.secret"}) ||
		hook["properties"].(map[string]any)["url"] != "https://hooks.example.com/demo" {
		t.Errorf("demo.webhook: %v", hook)
	}
	if _, err := importer.ReadExport(fixtureContent); err != nil {
		t.Fatal(err)
	}
}

// An export of the content set written before these tables reads as
// before.
func TestContentExportWithoutTheAdministrationTables(t *testing.T) {
	dir := copyExport(t, fixtureContent)
	for _, file := range administrationFiles {
		if err := os.Remove(filepath.Join(dir, file)); err != nil {
			t.Fatal(err)
		}
	}
	editManifest(t, dir, func(m map[string]any) {
		m["files"] = slices.DeleteFunc(m["files"].([]any), func(f any) bool {
			return slices.Contains(administrationFiles, f.(map[string]any)["file"].(string))
		})
	})
	export, err := importer.ReadExport(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := wantContent(t)
	want.CleanupPolicies, want.Tasks, want.Capabilities = []model.CleanupPolicy{}, []model.Task{}, []model.Capability{}
	same(t, export.Snapshot, want)
	if !slices.Equal(export.Kinds, []state.Kind{state.KindBlobStores, state.KindRepositories}) {
		t.Fatalf("kinds %v", export.Kinds)
	}
}

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := []string{}
	for _, item := range list {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

// editRows rewrites the rows of a file of an export through fn and fixes
// the manifest.
func editRows(t *testing.T, dir, file string, fn func(row map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		fn(r)
		out, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, string(out))
	}
	writeRows(t, dir, file, rows...)
}

func wantPolicies() []model.CleanupPolicy {
	return []model.CleanupPolicy{
		{Name: "demo-age", Notes: "Invented: components older than 30 days", Format: "maven2", Origin: model.OriginImport,
			Criteria: map[string]any{"lastBlobUpdatedDays": json.Number("30")}},
		{Name: "demo-mixed", Format: "ALL_FORMATS", Origin: model.OriginImport,
			Criteria: map[string]any{"lastDownloadedDays": json.Number("60"), "regex": ".*-SNAPSHOT.*"}},
		{Name: "demo-unused", Notes: "Invented: never downloaded in 90 days", Format: "npm", Origin: model.OriginImport,
			Criteria: map[string]any{"lastDownloadedDays": json.Number("90")}},
	}
}

func wantCapabilities() []model.Capability {
	return []model.Capability{
		{ID: "0ca00000-0000-4000-8000-000000000001", TypeID: "baseurl", Enabled: true, Origin: model.OriginImport,
			Properties: map[string]string{"url": "https://registry.example.com/"}},
		{ID: "0ca00000-0000-4000-8000-000000000002", TypeID: "demo.webhook", Notes: "Invented hook", Origin: model.OriginImport,
			Properties: map[string]string{"url": "https://hooks.example.com/demo"}},
		{ID: "0ca00000-0000-4000-8000-000000000003", TypeID: "defaultrole", Enabled: true, Origin: model.OriginImport,
			Properties: map[string]string{"role": "demo-viewer"}},
	}
}

func wantTasks() []model.Task {
	state := func(enabled, trigger string) map[string]string {
		return map[string]string{"enabled": enabled, "triggerState": trigger, "lastRunState.endState": "OK"}
	}
	task := func(id, typeID string, schedule model.TaskSchedule, properties map[string]string) model.Task {
		return model.Task{ID: id, Name: "Invented task " + id, TypeID: typeID, Schedule: schedule, Properties: properties,
			ImportedState: state("true", "WAITING"), Origin: model.OriginImport}
	}
	weekly := task("demo-weekly", "repository.maven.remove-snapshots", model.TaskSchedule{Type: "weekly",
		StartAt: time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC), WeekDays: []string{"FRI", "MON"}}, map[string]string{})
	weekly.ImportedState = state("false", "PAUSED")
	return []model.Task{
		task("demo-cron", "blobstore.compact", model.TaskSchedule{Type: "cron",
			StartAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Cron: "0 0 2 * * ?"},
			map[string]string{"blobstoreName": "demo-files"}),
		task("demo-manual", "db.backup", model.TaskSchedule{Type: "manual"}, map[string]string{"location": "/srv/backup/demo"}),
		task("demo-monthly", "repository.cleanup", model.TaskSchedule{Type: "monthly",
			StartAt: time.Date(2026, 2, 1, 4, 0, 0, 0, time.UTC), MonthDays: []int{1, 999}},
			map[string]string{"repositoryName": "demo-maven-hosted"}),
		task("demo-stricter", "repository.cleanup", model.TaskSchedule{Type: "cron",
			StartAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Cron: "0 0 12 15W,20 * ?"},
			map[string]string{"repositoryName": "*"}),
		weekly,
	}
}

// cronReason is the reason the import gives for an expression the parser
// refuses.
func cronReason(t *testing.T, expr string) string {
	t.Helper()
	_, err := tasks.ParseCron(expr)
	if err == nil {
		t.Fatalf("%q parses", expr)
	}
	return "cron: " + err.Error()
}

func sameJSON(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("%s differ\ngot  %s\nwant %s", what, g, w)
	}
}

// The content fixture gives its cleanup policies with every criterion as
// stored, its capabilities and its tasks, every task paused with its old
// state kept (I12, rulings S3 to S6), and the lists of the import result.
func TestReadContentExportAdministration(t *testing.T) {
	export, err := importer.ReadExport(fixtureContent)
	if err != nil {
		t.Fatal(err)
	}
	if want := []state.Kind{state.KindBlobStores, state.KindRepositories, state.KindCleanupPolicies, state.KindTasks,
		state.KindCapabilities}; !slices.Equal(export.Kinds, want) {
		t.Fatalf("kinds %v, want %v", export.Kinds, want)
	}
	sameJSON(t, "policies", export.Snapshot.CleanupPolicies, wantPolicies())
	sameJSON(t, "capabilities", export.Snapshot.Capabilities, wantCapabilities())
	sameJSON(t, "tasks", export.Snapshot.Tasks, wantTasks())
	for _, task := range export.Snapshot.Tasks {
		if task.Enabled {
			t.Fatalf("task %s arrives enabled", task.ID)
		}
	}
	sameJSON(t, "policies that may not run", export.PoliciesNotRun,
		[]importer.PolicyNote{{Name: "demo-mixed", Unknown: []string{"regex"},
			Reason: "criteria this server does not know: regex"}})
	sameJSON(t, "task notes", export.TaskNotes, []importer.TaskNote{
		{ID: "demo-monthly", Reason: "monthDays: 999 is outside 1 to 31"},
		{ID: "demo-stricter", Reason: cronReason(t, "0 0 12 15W,20 * ?")},
	})
	sameJSON(t, "cron tasks", export.CronTasks, []string{"demo-cron", "demo-stricter"})
	sameJSON(t, "unreadable tasks", export.UnreadableTasks, []string{"demo-unreadable"})
	sameJSON(t, "secret properties", export.SecretProperties, []importer.SecretProperty{
		{Kind: "capability", ID: "0ca00000-0000-4000-8000-000000000002", Key: "secret"},
		{Kind: "task", ID: "demo-weekly", Key: "remotePassword"},
	})
}

// Each of the 27 cron forms the old class accepts and this parser refuses
// (testdata/vectors/cron/stricter.json) imports as stored and is listed
// with its reason (ruling S6).
func TestStricterCronFormsAreListed(t *testing.T) {
	data, err := os.ReadFile("../../testdata/vectors/cron/stricter.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Cases []struct {
			Expression string `json:"expression"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Cases) != 27 {
		t.Fatalf("%d cases, want 27", len(vectors.Cases))
	}
	for _, c := range vectors.Cases {
		t.Run(strings.TrimSpace(c.Expression), func(t *testing.T) {
			dir := copyExport(t, fixtureContent)
			editRows(t, dir, "qrtz_triggers.jsonl", func(r map[string]any) {
				if r["job_name"] == "demo-cron" {
					r["job_data"].(map[string]any)["schedule.cronExpression"] = c.Expression
				}
			})
			export, err := importer.ReadExport(dir)
			if err != nil {
				t.Fatal(err)
			}
			want := importer.TaskNote{ID: "demo-cron", Reason: cronReason(t, c.Expression)}
			if !slices.Contains(export.TaskNotes, want) {
				t.Fatalf("notes %+v lack %+v", export.TaskNotes, want)
			}
			if i := slices.IndexFunc(export.Snapshot.Tasks, func(x model.Task) bool { return x.ID == "demo-cron" }); i < 0 ||
				export.Snapshot.Tasks[i].Schedule.Cron != c.Expression {
				t.Fatalf("the expression is not kept as stored: %+v", export.Snapshot.Tasks)
			}
		})
	}
}

// An import replaces the rows of origin import and never a row of origin
// local; an imported row whose key a local row holds is left out and
// listed (ruling S4).
func TestImportKeepsLocalRows(t *testing.T) {
	cfg := dataDir(t)
	st, err := state.Create(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	local := model.Task{ID: "demo-cron", Name: "Local", TypeID: "demo.local", Enabled: true, Origin: model.OriginLocal,
		Schedule: model.TaskSchedule{Type: "manual"}, Properties: map[string]string{}}
	own := model.Task{ID: "demo-own", Name: "Own", TypeID: "demo.local", Origin: model.OriginLocal,
		Schedule: model.TaskSchedule{Type: "manual"}, Properties: map[string]string{}}
	old := model.Task{ID: "demo-old", Name: "Old", TypeID: "demo.old", Origin: model.OriginImport,
		Schedule: model.TaskSchedule{Type: "manual"}, Properties: map[string]string{}, ImportedState: map[string]string{}}
	policy := model.CleanupPolicy{Name: "demo-age", Format: "raw", Origin: model.OriginLocal, Criteria: map[string]any{}}
	capability := model.Capability{ID: "0ca00000-0000-4000-8000-0000000000ff", TypeID: "demo.local", Origin: model.OriginLocal,
		Properties: map[string]string{}}
	for _, err := range []error{st.PutTask(local), st.PutTask(own), st.PutTask(old), st.PutCleanupPolicy(policy),
		st.PutCapability(capability)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := importer.Import(fixtureContent, cfg)
	if err != nil {
		t.Fatal(err)
	}
	snap := loadState(t, cfg)
	tasksWant := slices.Concat([]model.Task{local, own}, wantTasks()[1:])
	slices.SortFunc(tasksWant, func(a, b model.Task) int { return strings.Compare(a.ID, b.ID) })
	sameJSON(t, "tasks", snap.Tasks, tasksWant)
	sameJSON(t, "policies", snap.CleanupPolicies, append([]model.CleanupPolicy{policy}, wantPolicies()[1:]...))
	sameJSON(t, "capabilities", snap.Capabilities, append(wantCapabilities(), capability))
	sameJSON(t, "local rows kept", result.LocalKept, []string{"cleanup policy demo-age", "task demo-cron"})

	// a second run gives the same rows
	if _, err := importer.Import(fixtureContent, cfg); err != nil {
		t.Fatal(err)
	}
	again := loadState(t, cfg)
	sameJSON(t, "tasks after a second run", again.Tasks, tasksWant)
}

// An export without the administration tables leaves their rows as they
// are.
func TestImportWithoutTheAdministrationTablesKeepsTheirRows(t *testing.T) {
	cfg := dataDir(t)
	if _, err := importer.Import(fixtureContent, cfg); err != nil {
		t.Fatal(err)
	}
	dir := copyExport(t, fixtureContent)
	for _, file := range administrationFiles {
		if err := os.Remove(filepath.Join(dir, file)); err != nil {
			t.Fatal(err)
		}
	}
	editManifest(t, dir, func(m map[string]any) {
		m["files"] = slices.DeleteFunc(m["files"].([]any), func(f any) bool {
			return slices.Contains(administrationFiles, f.(map[string]any)["file"].(string))
		})
	})
	if _, err := importer.Import(dir, cfg); err != nil {
		t.Fatal(err)
	}
	snap := loadState(t, cfg)
	if len(snap.Tasks) != 5 || len(snap.CleanupPolicies) != 3 || len(snap.Capabilities) != 3 {
		t.Fatalf("rows after an import without the tables: %d tasks, %d policies, %d capabilities", len(snap.Tasks),
			len(snap.CleanupPolicies), len(snap.Capabilities))
	}
}

const (
	taskCanary       = "CANARY-IMPORT-TASK-TOKEN-8d3e"
	capabilityCanary = "CANARY-IMPORT-CAPABILITY-KEY-2b9f"
)

// canaryExport is the content fixture with a secret property in a task and
// in a capability, as an export program that removed nothing would write
// them.
func canaryExport(t *testing.T) string {
	t.Helper()
	dir := copyExport(t, fixtureContent)
	editRows(t, dir, "qrtz_job_details.jsonl", func(r map[string]any) {
		if r["job_name"] == "demo-manual" {
			r["job_data"].(map[string]any)["apiToken"] = taskCanary
		}
	})
	editRows(t, dir, "capability_storage_item.jsonl", func(r map[string]any) {
		if r["type"] == "baseurl" {
			r["properties"].(map[string]any)["accessKey"] = capabilityCanary
		}
	})
	return dir
}

// A property whose key the secret word rule marks is kept in a secret file
// of mode 0600 beside the LDAP secrets and in no record, no result and no
// byte of state.sqlite (ruling S7, A11).
func TestSecretPropertiesGoToSecretFiles(t *testing.T) {
	cfg := dataDir(t)
	result, err := importer.Import(canaryExport(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ kind, id, key, canary string }{
		{"task", "demo-manual", "apiToken", taskCanary},
		{"capability", "0ca00000-0000-4000-8000-000000000001", "accessKey", capabilityCanary},
	} {
		path := state.PropertySecretPath(cfg.DataDir, c.kind, c.id, c.key)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 || filepath.Dir(path) != filepath.Join(cfg.DataDir, state.SecretsDir) {
			t.Fatalf("%s: mode %o", path, info.Mode().Perm())
		}
		value, err := state.ReadSecret(path)
		if err != nil || string(value) != c.canary {
			t.Fatalf("%s does not hold the secret: %v", path, err)
		}
		want := importer.SecretProperty{Kind: c.kind, ID: c.id, Key: c.key, Kept: true}
		if !slices.Contains(result.SecretProperties, want) {
			t.Fatalf("%+v lacks %+v", result.SecretProperties, want)
		}
	}
	text, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	stateBytes := stateFiles(t, cfg.DataDir)
	snap := loadState(t, cfg)
	records, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"result": text, "state.sqlite": stateBytes, "records": records} {
		for _, canary := range []string{taskCanary, capabilityCanary} {
			if bytes.Contains(data, []byte(canary)) {
				t.Errorf("%s holds the secret %s", name, canary)
			}
		}
	}
	// positive twin: the records hold the other properties of both rows
	if !bytes.Contains(records, []byte("/srv/backup/demo")) || !bytes.Contains(records, []byte("https://registry.example.com/")) {
		t.Fatalf("records lack the properties next to the secrets:\n%s", records)
	}
}

const nestedCanary = "CANARY-IMPORT-NESTED-PASSWORD-5e1c"

// A property value that is no string goes through the same secret removal
// as the other records: each secret key at any depth is taken out and
// listed with its path as a property to enter again (ruling S9).
func TestNestedSecretPropertiesAreRemovedAndListed(t *testing.T) {
	dir := copyExport(t, fixtureContent)
	editRows(t, dir, "capability_storage_item.jsonl", func(r map[string]any) {
		if r["type"] == "baseurl" {
			r["properties"].(map[string]any)["auth"] = map[string]any{"user": "demo", "password": nestedCanary,
				"hosts": []any{map[string]any{"name": "a.example.com", "token": nestedCanary}}}
		}
	})
	editRows(t, dir, "qrtz_job_details.jsonl", func(r map[string]any) {
		if r["job_name"] == "demo-manual" {
			r["job_data"].(map[string]any)["remote"] = map[string]any{"url": "https://remote.example.com/",
				"credentials": map[string]any{"user": "demo"}}
		}
	})
	cfg := dataDir(t)
	result, err := importer.Import(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	baseurl := "0ca00000-0000-4000-8000-000000000001"
	for _, want := range []importer.SecretProperty{
		{Kind: "capability", ID: baseurl, Key: "auth.password"},
		{Kind: "capability", ID: baseurl, Key: "auth.hosts[0].token"},
		{Kind: "task", ID: "demo-manual", Key: "remote.credentials"},
	} {
		if !slices.Contains(result.SecretProperties, want) {
			t.Errorf("%+v lacks %+v", result.SecretProperties, want)
		}
	}
	snap := loadState(t, cfg)
	records, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	text, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	stateBytes := stateFiles(t, cfg.DataDir)
	for name, data := range map[string][]byte{"result": text, "records": records, "state.sqlite": stateBytes} {
		if bytes.Contains(data, []byte(nestedCanary)) {
			t.Errorf("%s holds the nested secret", name)
		}
	}
	// positive twin: the rest of each value is stored as its JSON text
	i := slices.IndexFunc(snap.Capabilities, func(c model.Capability) bool { return c.ID == baseurl })
	if got := snap.Capabilities[i].Properties["auth"]; got != `{"hosts":[{"name":"a.example.com"}],"user":"demo"}` {
		t.Errorf("auth stored as %s", got)
	}
	j := slices.IndexFunc(snap.Tasks, func(x model.Task) bool { return x.ID == "demo-manual" })
	if got := snap.Tasks[j].Properties["remote"]; got != `{"url":"https://remote.example.com/"}` {
		t.Errorf("remote stored as %s", got)
	}
}

// A key the export program took out of a value inside a property is listed
// with its path, as a key at the top is (finding 3 of the review).
func TestRemovedNestedKeysAreListedWithTheirPath(t *testing.T) {
	dir := copyExport(t, fixtureContent)
	editRows(t, dir, "capability_storage_item.jsonl", func(r map[string]any) {
		if r["type"] == "baseurl" {
			r["properties"].(map[string]any)["auth"] = map[string]any{"user": "demo"}
			r["_removed"] = []any{"properties.auth.password", "properties.auth.hosts[0].token"}
		}
	})
	editRows(t, dir, "qrtz_job_details.jsonl", func(r map[string]any) {
		if r["job_name"] == "demo-manual" {
			r["_removed"] = []any{"job_data.remote.credentials"}
		}
	})
	export, err := importer.ReadExport(dir)
	if err != nil {
		t.Fatal(err)
	}
	baseurl := "0ca00000-0000-4000-8000-000000000001"
	for _, want := range []importer.SecretProperty{
		{Kind: "capability", ID: baseurl, Key: "auth.password"},
		{Kind: "capability", ID: baseurl, Key: "auth.hosts[0].token"},
		{Kind: "task", ID: "demo-manual", Key: "remote.credentials"},
		{Kind: "task", ID: "demo-weekly", Key: "remotePassword"},
	} {
		if !slices.Contains(export.SecretProperties, want) {
			t.Errorf("%+v lacks %+v", export.SecretProperties, want)
		}
	}
}

// Every secret file is checked before the state transaction: a value above
// state.MaxSecretBytes or a file name the file system does not take refuses
// the import before anything is written, and the message names the record
// and the key, never the value (ruling S10).
func TestSecretFilesAreCheckedBeforeTheState(t *testing.T) {
	long := "apiToken" + strings.Repeat("x", 300)
	for _, c := range []struct {
		name, key, value, message string
	}{
		{"too large", "apiToken", taskCanary + strings.Repeat("v", state.MaxSecretBytes), "task demo-manual: the secret property apiToken"},
		{"name too long", long, taskCanary, "task demo-manual: the secret property " + long},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := copyExport(t, fixtureContent)
			editRows(t, dir, "qrtz_job_details.jsonl", func(r map[string]any) {
				if r["job_name"] == "demo-manual" {
					r["job_data"].(map[string]any)[c.key] = c.value
				}
			})
			cfg := dataDir(t)
			_, err := importer.Import(dir, cfg)
			if err == nil || !strings.Contains(err.Error(), c.message) {
				t.Fatalf("error %v, want one that names %q", err, c.message)
			}
			if strings.Contains(err.Error(), taskCanary) {
				t.Fatal("the error holds the value")
			}
			if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
				t.Fatalf("the data directory was written: %v", err)
			}
		})
	}
	// positive twin: a value of state.MaxSecretBytes bytes is kept
	dir := copyExport(t, fixtureContent)
	value := strings.Repeat("v", state.MaxSecretBytes)
	editRows(t, dir, "qrtz_job_details.jsonl", func(r map[string]any) {
		if r["job_name"] == "demo-manual" {
			r["job_data"].(map[string]any)["apiToken"] = value
		}
	})
	cfg := dataDir(t)
	if _, err := importer.Import(dir, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := state.ReadSecret(state.PropertySecretPath(cfg.DataDir, "task", "demo-manual", "apiToken"))
	if err != nil || string(got) != value {
		t.Fatalf("the secret file does not hold the value: %v", err)
	}
}

// A secret property of an imported row that a local row of the same key
// keeps out is not written, so it is listed as not kept (ruling S11).
func TestSecretOfARowKeptOutIsNotKept(t *testing.T) {
	cfg := dataDir(t)
	st, err := state.Create(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	local := model.Task{ID: "demo-manual", Name: "Local", TypeID: "demo.local", Origin: model.OriginLocal,
		Schedule: model.TaskSchedule{Type: "manual"}, Properties: map[string]string{}}
	if err := st.PutTask(local); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := importer.Import(canaryExport(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	notKept := importer.SecretProperty{Kind: "task", ID: "demo-manual", Key: "apiToken"}
	if !slices.Contains(result.SecretProperties, notKept) ||
		slices.Contains(result.SecretProperties, importer.SecretProperty{Kind: "task", ID: "demo-manual", Key: "apiToken", Kept: true}) {
		t.Fatalf("%+v does not list %+v as not kept", result.SecretProperties, notKept)
	}
	if _, err := os.Stat(state.PropertySecretPath(cfg.DataDir, "task", "demo-manual", "apiToken")); !os.IsNotExist(err) {
		t.Fatalf("the secret file of the row kept out exists: %v", err)
	}
	// positive twin: the secret of the capability that was written is kept
	kept := importer.SecretProperty{Kind: "capability", ID: "0ca00000-0000-4000-8000-000000000001", Key: "accessKey", Kept: true}
	if !slices.Contains(result.SecretProperties, kept) {
		t.Fatalf("%+v lacks %+v", result.SecretProperties, kept)
	}
}

// After the state transaction the import removes each secret file of a
// task or capability that belongs to no stored row and key any more; the
// files of LDAP connections and of local rows stay (ruling S12).
func TestStaleSecretFilesAreRemoved(t *testing.T) {
	cfg := dataDir(t)
	if _, err := importer.Import(canaryExport(t), cfg); err != nil {
		t.Fatal(err)
	}
	taskFile := state.PropertySecretPath(cfg.DataDir, "task", "demo-manual", "apiToken")
	capabilityFile := state.PropertySecretPath(cfg.DataDir, "capability", "0ca00000-0000-4000-8000-000000000001", "accessKey")
	gone := state.PropertySecretPath(cfg.DataDir, "task", "demo-gone", "apiToken")
	ldap := state.LDAPSecretPath(cfg.DataDir, "demo-ldap")
	own := state.PropertySecretPath(cfg.DataDir, "task", "demo-own", "apiToken")
	for _, path := range []string{gone, ldap, own} {
		if err := state.WriteSecret(path, []byte("invented")); err != nil {
			t.Fatal(err)
		}
	}
	st, err := state.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutTask(model.Task{ID: "demo-own", Name: "Own", TypeID: "demo.local", Origin: model.OriginLocal,
		Schedule: model.TaskSchedule{Type: "manual"}, Properties: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// the same rows again, without the secret properties
	if _, err := importer.Import(fixtureContent, cfg); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{taskFile, capabilityFile, gone} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s stays: %v", filepath.Base(path), err)
		}
	}
	for _, path := range []string{ldap, own} {
		if _, err := state.ReadSecret(path); err != nil {
			t.Errorf("%s: %v", filepath.Base(path), err)
		}
	}
	// a third run with the secrets writes them again
	if _, err := importer.Import(canaryExport(t), cfg); err != nil {
		t.Fatal(err)
	}
	if value, err := state.ReadSecret(taskFile); err != nil || string(value) != taskCanary {
		t.Fatalf("the secret file of apiToken after a run again: %v", err)
	}
}

// An import without the tasks leaves the secret files of the stored tasks.
func TestSecretFilesOfKindsNotImportedStay(t *testing.T) {
	cfg := dataDir(t)
	if _, err := importer.Import(canaryExport(t), cfg); err != nil {
		t.Fatal(err)
	}
	dir := copyExport(t, fixtureContent)
	for _, file := range administrationFiles {
		if err := os.Remove(filepath.Join(dir, file)); err != nil {
			t.Fatal(err)
		}
	}
	editManifest(t, dir, func(m map[string]any) {
		m["files"] = slices.DeleteFunc(m["files"].([]any), func(f any) bool {
			return slices.Contains(administrationFiles, f.(map[string]any)["file"].(string))
		})
	})
	if _, err := importer.Import(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if value, err := state.ReadSecret(state.PropertySecretPath(cfg.DataDir, "task", "demo-manual", "apiToken")); err != nil ||
		string(value) != taskCanary {
		t.Fatalf("the secret file of a stored task: %v", err)
	}
}

// addRow appends a row to a file of an export and fixes the manifest.
func addRow(t *testing.T, dir, file string, r map[string]any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	rows := append(strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), string(line))
	writeRows(t, dir, file, rows...)
}

// dropRows removes the rows of a file of an export for which fn holds.
func dropRows(t *testing.T, dir, file string, fn func(r map[string]any) bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if !fn(r) {
			rows = append(rows, line)
		}
	}
	writeRows(t, dir, file, rows...)
}

func taskByID(t *testing.T, export *importer.Export, id string) model.Task {
	t.Helper()
	i := slices.IndexFunc(export.Snapshot.Tasks, func(x model.Task) bool { return x.ID == id })
	if i < 0 {
		t.Fatalf("no task %s in %+v", id, export.Snapshot.Tasks)
	}
	return export.Snapshot.Tasks[i]
}

// The edge cases of the join of jobs and triggers: each is stored as read
// and listed, or refused with the file named; none is left out silently.
func TestTaskEdgeCases(t *testing.T) {
	trigger := func(name, job string, data map[string]any) map[string]any {
		return map[string]any{"trigger_name": name, "trigger_group": "demo", "job_name": job, "job_group": "demo",
			"trigger_state": "WAITING", "trigger_type": "SIMPLE", "job_data": data, "_removed": []any{}}
	}
	read := func(t *testing.T, dir string) *importer.Export {
		t.Helper()
		export, err := importer.ReadExport(dir)
		if err != nil {
			t.Fatal(err)
		}
		return export
	}
	hasNote := func(t *testing.T, export *importer.Export, want importer.TaskNote) {
		t.Helper()
		if !slices.Contains(export.TaskNotes, want) {
			t.Fatalf("notes %+v lack %+v", export.TaskNotes, want)
		}
	}
	t.Run("job without trigger", func(t *testing.T) {
		dir := copyExport(t, fixtureContent)
		dropRows(t, dir, "qrtz_triggers.jsonl", func(r map[string]any) bool { return r["job_name"] == "demo-cron" })
		export := read(t, dir)
		hasNote(t, export, importer.TaskNote{ID: "demo-cron", Reason: "the job has no trigger; read as manual"})
		if task := taskByID(t, export, "demo-cron"); task.Schedule.Type != tasks.TypeManual {
			t.Fatalf("schedule %+v", task.Schedule)
		}
		if slices.Contains(export.CronTasks, "demo-cron") {
			t.Fatal("a task read as manual is listed as cron")
		}
	})
	for _, file := range []string{"qrtz_job_details.jsonl", "qrtz_triggers.jsonl"} {
		t.Run("only one task file without "+file, func(t *testing.T) {
			dir := copyExport(t, fixtureContent)
			if err := os.Remove(filepath.Join(dir, file)); err != nil {
				t.Fatal(err)
			}
			editManifest(t, dir, func(m map[string]any) {
				m["files"] = slices.DeleteFunc(m["files"].([]any), func(f any) bool { return f.(map[string]any)["file"] == file })
			})
			_, err := importer.ReadExport(dir)
			if err == nil || !strings.Contains(err.Error(), "qrtz_job_details.jsonl and qrtz_triggers.jsonl; the tasks need both") {
				t.Fatalf("error %v", err)
			}
		})
	}
	t.Run("several triggers", func(t *testing.T) {
		dir := copyExport(t, fixtureContent)
		addRow(t, dir, "qrtz_triggers.jsonl", trigger("demo-cron-second", "demo-cron", map[string]any{"schedule.type": "manual"}))
		export := read(t, dir)
		hasNote(t, export, importer.TaskNote{ID: "demo-cron", Reason: "the job has 2 triggers; the first is read"})
		if task := taskByID(t, export, "demo-cron"); task.Schedule.Cron != "0 0 2 * * ?" {
			t.Fatalf("schedule %+v", task.Schedule)
		}
	})
	t.Run("trigger without job", func(t *testing.T) {
		dir := copyExport(t, fixtureContent)
		addRow(t, dir, "qrtz_triggers.jsonl", trigger("demo-orphan-trigger", "demo-missing", map[string]any{"schedule.type": "manual"}))
		export := read(t, dir)
		if !slices.Equal(export.TriggersWithoutJob, []string{"demo.demo-orphan-trigger"}) {
			t.Fatalf("triggers without job %v", export.TriggersWithoutJob)
		}
		if len(export.Snapshot.Tasks) != 5 {
			t.Fatalf("%d tasks", len(export.Snapshot.Tasks))
		}
	})
	t.Run("unparsable start", func(t *testing.T) {
		dir := copyExport(t, fixtureContent)
		editRows(t, dir, "qrtz_triggers.jsonl", func(r map[string]any) {
			if r["job_name"] == "demo-cron" {
				r["job_data"].(map[string]any)["schedule.startAt"] = "the first of January"
			}
		})
		export := read(t, dir)
		hasNote(t, export, importer.TaskNote{ID: "demo-cron",
			Reason: "schedule.startAt is no time of the form 2006-01-02T15:04:05Z07:00"})
		if task := taskByID(t, export, "demo-cron"); !task.Schedule.StartAt.IsZero() || task.Schedule.Cron != "0 0 2 * * ?" {
			t.Fatalf("schedule %+v", task.Schedule)
		}
	})
	t.Run("monthly day no number", func(t *testing.T) {
		dir := copyExport(t, fixtureContent)
		editRows(t, dir, "qrtz_triggers.jsonl", func(r map[string]any) {
			if r["job_name"] == "demo-monthly" {
				r["job_data"].(map[string]any)["schedule.daysToRun"] = "1,last"
			}
		})
		export := read(t, dir)
		hasNote(t, export, importer.TaskNote{ID: "demo-monthly", Reason: "schedule.daysToRun holds a day that is no number"})
		if task := taskByID(t, export, "demo-monthly"); !slices.Equal(task.Schedule.MonthDays, []int{1}) {
			t.Fatalf("schedule %+v", task.Schedule)
		}
	})
	t.Run("duplicate task id", func(t *testing.T) {
		dir := copyExport(t, fixtureContent)
		editRows(t, dir, "qrtz_job_details.jsonl", func(r map[string]any) {
			if r["job_name"] == "demo-manual" {
				r["job_data"].(map[string]any)[".id"] = "demo-cron"
			}
		})
		_, err := importer.ReadExport(dir)
		if err == nil || !strings.Contains(err.Error(), `qrtz_job_details.jsonl: task "demo-cron" is given twice`) {
			t.Fatalf("error %v", err)
		}
	})
}

// A cleanup policy with no criterion, or with every criterion null, is
// listed with the reason no criterion; one whose criterion holds a value the
// server does not take is listed with that reason (ruling S14).
func TestPolicyReasons(t *testing.T) {
	dir := copyExport(t, fixtureContent)
	editRows(t, dir, "cleanup_policy.jsonl", func(r map[string]any) {
		switch r["name"] {
		case "demo-age":
			r["criteria"] = map[string]any{}
		case "demo-unused":
			r["criteria"] = map[string]any{"lastDownloadedDays": nil}
		}
	})
	addRow(t, dir, "cleanup_policy.jsonl", map[string]any{"name": "demo-zero", "notes": nil, "format": "raw",
		"mode": "delete", "criteria": map[string]any{"lastDownloadedDays": 0}, "_removed": []any{}})
	export, err := importer.ReadExport(dir)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, "policies that may not run", export.PoliciesNotRun, []importer.PolicyNote{
		{Name: "demo-age", Unknown: []string{}, Reason: "no criterion"},
		{Name: "demo-mixed", Unknown: []string{"regex"}, Reason: "criteria this server does not know: regex"},
		{Name: "demo-unused", Unknown: []string{}, Reason: "no criterion"},
		{Name: "demo-zero", Unknown: []string{}, Reason: "a criterion holds a value this server does not take"},
	})
}

const triggerCanary = "CANARY-IMPORT-TRIGGER-TOKEN-71ad"

// The secret keys of the job data of a trigger are listed like the others:
// a value of the trigger that is read goes into a secret file of its task,
// a key the export removed or a key of a further trigger is listed to enter
// again (ruling S14).
func TestTriggerSecretsAreListed(t *testing.T) {
	dir := copyExport(t, fixtureContent)
	editRows(t, dir, "qrtz_triggers.jsonl", func(r map[string]any) {
		if r["job_name"] == "demo-manual" {
			r["job_data"].(map[string]any)["schedule.apiToken"] = triggerCanary
			r["_removed"] = []any{"job_data.schedule.password"}
		}
	})
	addRow(t, dir, "qrtz_triggers.jsonl", map[string]any{"trigger_name": "demo-cron-second", "trigger_group": "demo",
		"job_name": "demo-cron", "job_group": "demo", "trigger_state": "WAITING", "trigger_type": "SIMPLE",
		"job_data": map[string]any{"schedule.type": "manual", "secretHeader": triggerCanary}, "_removed": []any{}})
	cfg := dataDir(t)
	result, err := importer.Import(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []importer.SecretProperty{
		{Kind: "task", ID: "demo-manual", Key: "schedule.apiToken", Kept: true},
		{Kind: "task", ID: "demo-manual", Key: "schedule.password"},
		{Kind: "task", ID: "demo-cron", Key: "secretHeader"},
	} {
		if !slices.Contains(result.SecretProperties, want) {
			t.Errorf("%+v lacks %+v", result.SecretProperties, want)
		}
	}
	value, err := state.ReadSecret(state.PropertySecretPath(cfg.DataDir, "task", "demo-manual", "schedule.apiToken"))
	if err != nil || string(value) != triggerCanary {
		t.Fatalf("the secret file of the trigger value: %v", err)
	}
	text, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	stateBytes := stateFiles(t, cfg.DataDir)
	for name, data := range map[string][]byte{"result": text, "state.sqlite": stateBytes} {
		if bytes.Contains(data, []byte(triggerCanary)) {
			t.Errorf("%s holds the trigger secret", name)
		}
	}
}

// stateFiles are the bytes of state.sqlite and, when it exists, of its
// write-ahead log, where a row written last may still wait.
func stateFiles(t *testing.T, dataDir string) []byte {
	t.Helper()
	data, err := os.ReadFile(state.Path(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(state.Path(dataDir) + "-wal")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return append(data, log...)
}

// The canary checks read the write-ahead log too.
func TestStateFilesReadTheLog(t *testing.T) {
	cfg := dataDir(t)
	if _, err := importer.Import(fixtureContent, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.Path(cfg.DataDir)+"-wal", []byte("CANARY-IN-THE-LOG"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := stateFiles(t, cfg.DataDir)
	if !bytes.Contains(data, []byte("CANARY-IN-THE-LOG")) {
		t.Fatal("the bytes lack the write-ahead log")
	}
	if !bytes.Contains(data, []byte("SQLite format 3")) {
		t.Fatal("the bytes lack state.sqlite")
	}
}
