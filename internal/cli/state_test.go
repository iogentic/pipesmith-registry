package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/importer"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

const (
	fixtureExport  = "../../testdata/export/fixture"
	fixtureContent = "../../testdata/export/fixture-content"
)

// The help texts argparse of Python 3.12 prints at 80 columns for the three
// commands of the import.
var stateHelp = map[string]string{
	"import": "usage: pipesmith-registry import [-h] [--data DATA] directory\n\npositional arguments:\n  directory\n\n" +
		"options:\n  -h, --help   show this help message and exit\n  --data DATA\n",
	"import-state": "usage: pipesmith-registry import-state [-h] --settings SETTINGS [--data DATA]\n\n" +
		"options:\n  -h, --help           show this help message and exit\n  --settings SETTINGS\n  --data DATA\n",
	"state": "usage: pipesmith-registry state [-h] {show} ...\n\npositional arguments:\n  {show}\n\n" +
		"options:\n  -h, --help  show this help message and exit\n",
	"state show": "usage: pipesmith-registry state show [-h] [--data DATA]\n\n" +
		"options:\n  -h, --help   show this help message and exit\n  --data DATA\n",
}

func TestStateCommandHelp(t *testing.T) {
	dir := dataDir(t)
	for command, want := range stateHelp {
		code, out, errOut := run(t, append(strings.Fields(command), "--help")...)
		if code != 0 || out != want || errOut != "" {
			t.Errorf("%s --help: %d\n%s\nwant\n%s", command, code, out, want)
		}
	}
	_, out, _ := run(t, "help")
	for _, line := range []string{"  import <directory> [--data <dir>]", "  import-state --settings <file> [--data <dir>]",
		"  state show [--data <dir>]"} {
		if !strings.Contains(out, line) {
			t.Errorf("help lacks %q", line)
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("help created the data directory")
	}
}

func TestStateCommandUsage(t *testing.T) {
	dataDir(t)
	for args, want := range map[string]string{
		"import":       "error: pipesmith-registry import: the following arguments are required: directory\n",
		"import-state": "error: pipesmith-registry import-state: the following arguments are required: --settings\n",
		"state":        "error: pipesmith-registry state: the following arguments are required: state_command\n",
	} {
		code, _, errOut := run(t, strings.Fields(args)...)
		if code != 2 || !strings.HasPrefix(errOut, want) {
			t.Errorf("%s: %d %q", args, code, errOut)
		}
	}
}

type shown struct {
	Schema                int               `json:"schema"`
	CleanupPolicies       []json.RawMessage `json:"cleanupPolicies"`
	Tasks                 []json.RawMessage `json:"tasks"`
	Capabilities          []json.RawMessage `json:"capabilities"`
	Counts                map[string]int    `json:"counts"`
	UsersWithPasswordHash int               `json:"usersWithPasswordHash"`
	Users                 []struct {
		ID    string   `json:"userId"`
		Roles []string `json:"roles"`
	} `json:"users"`
	Repositories []struct {
		Name string `json:"name"`
	} `json:"repositories"`
	Anonymous struct {
		Enabled bool `json:"enabled"`
	} `json:"anonymous"`
}

func show(t *testing.T, args ...string) (shown, string) {
	t.Helper()
	code, out, errOut := run(t, append([]string{"state", "show"}, args...)...)
	if code != 0 {
		t.Fatalf("state show: %d %s", code, errOut)
	}
	var s shown
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("state show is no JSON: %v\n%s", err, out)
	}
	return s, out
}

func TestImportAndStateShow(t *testing.T) {
	dir := dataDir(t)
	code, out, errOut := run(t, "import", fixtureExport)
	if code != 0 || errOut != "" {
		t.Fatalf("import: %d %q %q", code, out, errOut)
	}
	for _, want := range []string{"set identity", "users 4 (4 with a password hash)", "role mappings 1", "roles 5",
		"privileges 6", "LDAP servers 1", "realms 4", "anonymous access off", state.Path(dir)} {
		if !strings.Contains(out, want) {
			t.Errorf("import output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "$shiro1$") {
		t.Fatal("import printed a hash")
	}
	code, out, _ = run(t, "import", fixtureContent)
	if code != 0 || !strings.Contains(out, "repositories 6") || !strings.Contains(out, "blob stores 4") ||
		!strings.Contains(out, "demo-maven-proxy, demo-npm-proxy") || !strings.Contains(out, filepath.Join(dir, "repositories.json")) {
		t.Fatalf("import of the content set: %d\n%s", code, out)
	}
	s, text := show(t)
	if strings.Contains(text, "$shiro1$") || strings.Contains(text, "SHA-512") || strings.Contains(text, `"passwordHash"`) {
		t.Fatal("state show prints a hash")
	}
	if s.Schema != state.SchemaVersion || s.UsersWithPasswordHash != 4 || len(s.Users) != 4 || s.Users[0].ID != "admin" ||
		s.Users[0].Roles[0] != "nx-admin" || len(s.Repositories) != 6 || s.Counts["roles"] != 5 || s.Anonymous.Enabled {
		t.Fatalf("state show: %+v", s)
	}
	defs, err := repos.LoadDefinitions(filepath.Join(dir, "repositories.json"))
	if err != nil || len(defs) != 6 {
		t.Fatalf("repositories.json: %v %v", defs, err)
	}
}

func TestImportDataOption(t *testing.T) {
	env := dataDir(t)
	other := filepath.Join(t.TempDir(), "other")
	if code, _, errOut := run(t, "import", fixtureExport, "--data", other); code != 0 {
		t.Fatalf("import --data: %d %s", code, errOut)
	}
	if _, err := os.Stat(state.Path(other)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(env); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("import --data wrote into the data directory of the environment: %v", err)
	}
	if s, _ := show(t, "--data", other); len(s.Users) != 4 {
		t.Fatalf("state show --data: %+v", s)
	}
}

func TestImportRefusals(t *testing.T) {
	dir := dataDir(t)
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.Mkdir(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "manifest.json"), []byte(`{"format": "other"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"import", filepath.Join(t.TempDir(), "absent")}, {"import", broken},
		{"import-state", "--settings", filepath.Join(t.TempDir(), "absent.json")}} {
		code, out, errOut := run(t, args...)
		if code != 1 || out != "" || !strings.HasPrefix(errOut, "error: ") {
			t.Errorf("%v: %d %q %q", args, code, out, errOut)
		}
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused import created the data directory: %v", err)
	}
	code, _, errOut := run(t, "state", "show")
	if code != 1 || !strings.Contains(errOut, "no state.sqlite") {
		t.Fatalf("state show without state: %d %q", code, errOut)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state show created the data directory: %v", err)
	}
}

func TestImportStateCommand(t *testing.T) {
	dir := dataDir(t)
	source := filepath.Join(t.TempDir(), "repositorySettings.json")
	settings := `[{"name": "real-a", "format": "raw", "type": "hosted"},
		{"name": "real-b", "format": "npm", "type": "proxy", "proxy": {"remoteUrl": "https://registry.example.com/"},
		 "httpClient": {"authentication": {"type": "username", "username": "u", "password": "settings-secret-2"}}}]`
	if err := os.WriteFile(source, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "import-state", "--settings", source)
	if code != 0 || !strings.Contains(out, "repositories 2") || !strings.Contains(out, "real-b") ||
		strings.Contains(out+errOut, "settings-secret-2") {
		t.Fatalf("import-state: %d %q %q", code, out, errOut)
	}
	if s, text := show(t); len(s.Repositories) != 2 || len(s.Users) != 0 || strings.Contains(text, "settings-secret-2") {
		t.Fatalf("state show after import-state: %s", text)
	}
	defs, err := repos.LoadDefinitions(filepath.Join(dir, "repositories.json"))
	if err != nil || len(defs) != 2 {
		t.Fatalf("repositories.json: %v %v", defs, err)
	}
}

// state show counts the cleanup policies, tasks and capabilities and lists
// them (ruling S8).
func TestStateShowCountsTheKindsOfStep5(t *testing.T) {
	dir := dataDir(t)
	st, err := state.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = errors.Join(
		st.PutCleanupPolicy(model.CleanupPolicy{Name: "demo-policy", Format: "raw", Origin: model.OriginLocal}),
		st.PutTask(model.Task{ID: "demo-task", Name: "Demo", TypeID: "demo.type", Origin: model.OriginLocal,
			Schedule: model.TaskSchedule{Type: "manual"}}),
		st.PutTask(model.Task{ID: "demo-task-2", Name: "Demo 2", TypeID: "demo.type", Origin: model.OriginImport,
			Schedule: model.TaskSchedule{Type: "manual"}, ImportedState: map[string]string{"enabled": "true"}}),
		st.PutCapability(model.Capability{ID: "demo-capability", TypeID: "demo.type", Origin: model.OriginImport}),
		st.Close())
	if err != nil {
		t.Fatal(err)
	}
	s, text := show(t)
	for kind, want := range map[string]int{"cleanup_policies": 1, "tasks": 2, "capabilities": 1} {
		if got, ok := s.Counts[kind]; !ok || got != want {
			t.Errorf("count of %s: %d (named %v), want %d", kind, got, ok, want)
		}
	}
	if len(s.CleanupPolicies) != 1 || len(s.Tasks) != 2 || len(s.Capabilities) != 1 ||
		!strings.Contains(text, `"typeId": "demo.type"`) || !strings.Contains(text, `"origin": "import"`) {
		t.Fatalf("state show lists\n%s", text)
	}
}

const (
	taskCanary       = "CANARY-CLI-TASK-TOKEN-5e71"
	capabilityCanary = "CANARY-CLI-CAPABILITY-KEY-0f3a"
)

// canaryContent is the content fixture with a secret property in the job
// data of demo-manual and in the properties of the capability baseurl, as an
// export program that removed nothing would write them.
func canaryContent(t *testing.T) string {
	t.Helper()
	return contentWith(t, map[string]func(row map[string]any){
		"qrtz_job_details.jsonl": func(row map[string]any) {
			if row["job_name"] == "demo-manual" {
				row["job_data"].(map[string]any)["apiToken"] = taskCanary
			}
		},
		"capability_storage_item.jsonl": func(row map[string]any) {
			if row["type"] == "baseurl" {
				row["properties"].(map[string]any)["accessKey"] = capabilityCanary
			}
		},
	})
}

// contentWith is a copy of the content fixture whose rows of each file named
// went through its edit, with the digests of the manifest set again.
func contentWith(t *testing.T, edits map[string]func(row map[string]any)) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "export")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fixtureContent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(fixtureContent, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sums := map[string]string{}
	for file, edit := range edits {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		var out []byte
		for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			dec := json.NewDecoder(strings.NewReader(line))
			dec.UseNumber()
			var row map[string]any
			if err := dec.Decode(&row); err != nil {
				t.Fatal(err)
			}
			edit(row)
			text, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			out = append(append(out, text...), '\n')
		}
		if err := os.WriteFile(filepath.Join(dir, file), out, 0o600); err != nil {
			t.Fatal(err)
		}
		sums[file] = fmt.Sprintf("%x", sha256.Sum256(out))
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	for _, f := range m["files"].([]any) {
		entry := f.(map[string]any)
		if sum, ok := sums[entry["file"].(string)]; ok {
			entry["sha256"] = sum
		}
	}
	if data, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The import of the content set reports the policies, tasks and
// capabilities with what an administrator must know; a secret property
// reaches no output: not the import report, not state show, not an error,
// not state.sqlite (ruling S7). Its secret file holds it.
func TestImportKeepsSecretPropertiesOutOfEveryOutput(t *testing.T) {
	dir := dataDir(t)
	export := canaryContent(t)
	code, out, errOut := run(t, "import", export)
	if code != 0 {
		t.Fatalf("import: %d %s", code, errOut)
	}
	for _, want := range []string{"cleanup policies 3, tasks 5 imported, all paused, capabilities 3",
		"note: imported tasks arrive paused",
		"note: cleanup policy demo-mixed is kept and never runs; criteria this server does not know: regex",
		"note: task demo-stricter keeps its schedule as stored, which this server does not take: cron: ",
		"note: task demo-monthly keeps its schedule as stored, which this server does not take: monthDays: 999",
		"note: " + importer.CronNote + ": demo-cron, demo-stricter",
		"not imported: demo-unreadable",
		"secret properties kept in secret files of secrets/: capability 0ca00000-0000-4000-8000-000000000001 accessKey," +
			" task demo-manual apiToken",
		"enter them again: capability 0ca00000-0000-4000-8000-000000000002 secret, task demo-weekly remotePassword"} {
		if !strings.Contains(out, want) {
			t.Errorf("the import report lacks %q:\n%s", want, out)
		}
	}
	_, shown := show(t)
	if !strings.Contains(shown, "/srv/backup/demo") || !strings.Contains(shown, `"triggerState": "PAUSED"`) {
		t.Fatalf("state show lacks the task rows:\n%s", shown)
	}
	stateBytes := stateFiles(t, dir)
	secret, err := state.ReadSecret(state.PropertySecretPath(dir, state.SecretKindTask, "demo-manual", "apiToken"))
	if err != nil || string(secret) != taskCanary {
		t.Fatalf("the secret file of apiToken: %v", err)
	}

	// an import that fails at the secret files names the file, not the value
	failing := filepath.Join(t.TempDir(), "failing")
	if err := os.MkdirAll(failing, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(failing, state.SecretsDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	failCode, failOut, failErr := run(t, "import", export, "--data", failing)
	if failCode == 0 || !strings.Contains(failErr, state.SecretsDir) {
		t.Fatalf("an import into a data directory whose secrets is a file: %d %q", failCode, failErr)
	}
	for name, text := range map[string]string{"import report": out + errOut, "state show": shown,
		"state.sqlite": string(stateBytes), "failed import": failOut + failErr} {
		for _, canary := range []string{taskCanary, capabilityCanary} {
			if strings.Contains(text, canary) {
				t.Errorf("%s holds the secret %s", name, canary)
			}
		}
	}
}

// A cleanup policy without a criterion is listed with the reason no
// criterion, and the summary of the tasks counts the stored rows of origin
// import and names the local tasks kept over the import (ruling S14).
func TestImportReportCountsStoredTasksAndNamesPoliciesWithoutCriterion(t *testing.T) {
	dir := dataDir(t)
	st, err := state.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = errors.Join(st.PutTask(model.Task{ID: "demo-cron", Name: "Local", TypeID: "demo.local", Origin: model.OriginLocal,
		Schedule: model.TaskSchedule{Type: "manual"}, Properties: map[string]string{}}), st.Close())
	if err != nil {
		t.Fatal(err)
	}
	export := contentWith(t, map[string]func(row map[string]any){
		"cleanup_policy.jsonl": func(row map[string]any) {
			if row["name"] == "demo-age" {
				row["criteria"] = map[string]any{}
			}
		},
	})
	code, out, errOut := run(t, "import", export)
	if code != 0 {
		t.Fatalf("import: %d %s", code, errOut)
	}
	for _, want := range []string{"cleanup policies 3, tasks 4 imported, all paused, local tasks kept: demo-cron, capabilities 3",
		"note: cleanup policy demo-age is kept and never runs; no criterion",
		"note: cleanup policy demo-mixed is kept and never runs; criteria this server does not know: regex"} {
		if !strings.Contains(out, want) {
			t.Errorf("the import report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tasks 5") {
		t.Errorf("the report counts the task kept out:\n%s", out)
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

// The canary check of the import reads the write-ahead log too.
func TestStateFilesReadTheLog(t *testing.T) {
	dir := dataDir(t)
	if code, _, errOut := run(t, "import", fixtureContent); code != 0 {
		t.Fatalf("import: %d %s", code, errOut)
	}
	if err := os.WriteFile(state.Path(dir)+"-wal", []byte("CANARY-IN-THE-LOG"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := stateFiles(t, dir)
	if !strings.Contains(string(data), "CANARY-IN-THE-LOG") || !strings.Contains(string(data), "SQLite format 3") {
		t.Fatal("the bytes lack the write-ahead log or state.sqlite")
	}
}
