package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/repos"
	"github.com/mirosso25/pipesmith-registry/internal/state"
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
	Schema                int            `json:"schema"`
	Counts                map[string]int `json:"counts"`
	UsersWithPasswordHash int            `json:"usersWithPasswordHash"`
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
	if s.Schema != 1 || s.UsersWithPasswordHash != 4 || len(s.Users) != 4 || s.Users[0].ID != "admin" ||
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
