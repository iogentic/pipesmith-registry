package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// Task 7.18: import, a store change and update each write one audit line
// with the subject cli to audit.log; a command that reads writes none.
// While a server holds the log directory the line reaches audit.log
// through that server.

type commandLine struct {
	RequestID  string   `json:"request_id"`
	Subject    string   `json:"subject"`
	Realm      string   `json:"realm"`
	Client     string   `json:"client"`
	Action     string   `json:"action"`
	ObjectType string   `json:"object_type"`
	ObjectName string   `json:"object_name"`
	Result     string   `json:"result"`
	Fields     []string `json:"fields"`
}

// auditLines are the lines of audit.log of the data directory dir.
func auditLines(t *testing.T, dir string) ([]commandLine, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "logs", "audit.log"))
	if os.IsNotExist(err) {
		return nil, ""
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []commandLine
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		var line commandLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		out = append(out, line)
	}
	return out, string(data)
}

func TestCommandsWriteAuditLines(t *testing.T) {
	dir := dataDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o750); err != nil { // as serve made it
		t.Fatal(err)
	}
	tree := demoTree(t, filepath.Dir(dir))
	steps := []struct {
		args               []string
		code               int
		action, name, want string
	}{
		{[]string{"import", fixtureExport}, 0, "export.import", fixtureExport, "ok"},
		{[]string{"state", "show"}, 0, "", "", ""},
		{[]string{"store", "add", "tree", "--kind", "directory", "--root", tree}, 0, "store.create", "tree", "ok"},
		{[]string{"store", "list"}, 0, "", "", ""},
		{[]string{"store", "remove", "tree"}, 0, "store.delete", "tree", "ok"},
		{[]string{"update", filepath.Join(filepath.Dir(dir), "no-release")}, 1, "release.update", filepath.Join(filepath.Dir(dir), "no-release"), "failed"},
	}
	var want []commandLine
	for _, step := range steps {
		code, _, errOut := run(t, step.args...)
		if code != step.code {
			t.Fatalf("%v: exit %d, want %d: %s", step.args, code, step.code, errOut)
		}
		if strings.Contains(errOut, "audit") {
			t.Errorf("%v: standard error names the audit line: %q", step.args, errOut)
		}
		if step.action != "" {
			want = append(want, commandLine{Subject: "cli", Action: step.action, ObjectName: step.name, Result: step.want})
		}
		got, _ := auditLines(t, dir)
		if len(got) != len(want) {
			t.Fatalf("after %v: %d audit lines, want %d: %+v", step.args, len(got), len(want), got)
		}
	}
	got, _ := auditLines(t, dir)
	for i, line := range got {
		w := want[i]
		if line.Subject != w.Subject || line.Action != w.Action || line.ObjectName != w.ObjectName || line.Result != w.Result ||
			line.Realm != "" || line.Client != "" || line.Fields == nil {
			t.Errorf("line %d: %+v, want %+v", i, line, w)
		}
	}
	if !slices.Contains(got[0].Fields, "users") || !slices.Contains(got[0].Fields, "ldap") {
		t.Errorf("fields of the import: %v", got[0].Fields)
	}
	for _, field := range got[0].Fields {
		if strings.Contains(strings.ToLower(field), "password_hash") {
			t.Errorf("the import names the secret field %s", field)
		}
	}
}

// Finding 7b-1: a command beside a running server writes nothing to
// standard error, and its line reaches audit.log within the tick of that
// server, at most 5 s.
func TestCommandAuditBesideAServer(t *testing.T) {
	dir := dataDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o750); err != nil { // as serve made it
		t.Fatal(err)
	}
	tree := demoTree(t, filepath.Dir(dir))
	if code, _, errOut := run(t, "store", "add", "tree", "--kind", "directory", "--root", tree); code != 0 {
		t.Fatalf("store add: %s", errOut)
	}
	server := logs.Open(logs.Options{Dir: filepath.Join(dir, "logs")}) // holds the lock as serve does
	server.Start()
	t.Cleanup(func() { _ = server.Close() })
	code, _, errOut := run(t, "store", "remove", "tree")
	if code != 0 || errOut != "" {
		t.Fatalf("store remove: %d %q", code, errOut)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := auditLines(t, dir)
		if len(got) == 2 {
			if got[1].Action != "store.delete" || got[1].Subject != "cli" || got[1].Result != "ok" {
				t.Fatalf("line %+v", got[1])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit.log after 5 s: %+v", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, "store", "add", "tree", "--kind", "directory", "--root", tree); code != 0 || errOut != "" {
		t.Fatalf("store add: %d %q", code, errOut)
	}
	got, _ := auditLines(t, dir)
	if len(got) != 3 || got[2].Action != "store.create" {
		t.Errorf("lines after the server stopped: %+v", got)
	}
}

// runNames are the run names of every command the parser knows.
func runNames(p *parser) []string {
	var out []string
	if run, ok := p.defaults["run"].(string); ok {
		out = append(out, run)
	}
	for _, a := range p.actions {
		for _, sub := range a.commands {
			out = append(out, runNames(sub)...)
		}
	}
	return out
}

func TestEveryCommandIsClassified(t *testing.T) {
	names := runNames(buildParser())
	if len(names) < 30 {
		t.Fatalf("%d commands found", len(names))
	}
	for _, name := range names {
		_, changes := changingCommands[name]
		if changes == readCommands[name] {
			t.Errorf("command %s: changes state %v, reads only %v; it must be in exactly one list", name, changes, readCommands[name])
		}
	}
	for name := range changingCommands {
		if !slices.Contains(names, name) {
			t.Errorf("changingCommands names %s, which the parser does not know", name)
		}
	}
}

// A command makes no log directory: serve makes it, or the operator, and a
// directory that serve keeps off (--log-dir off) must stay absent. Without
// it the command writes no line.
func TestCommandMakesNoLogDirectory(t *testing.T) {
	dir := dataDir(t)
	if code, _, errOut := run(t, "import", fixtureExport); code != 0 || errOut != "" {
		t.Fatalf("import: %d %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs")); !os.IsNotExist(err) {
		t.Fatalf("the import made the log directory: %v", err)
	}
}

// Finding 7b-2: a password typed into the user name field of the sign in
// of serve is in no file of the log directory and not on standard error;
// the audit line names the subject unknown.
func TestTypedPasswordInNoLogFile(t *testing.T) {
	dir := importedFixture(t)
	const typed = "demo-typed-secret-pass-9"
	base, stop := serveData(t)
	body := strings.NewReader(`{"username":"` + typed + `","password":"demo-wrong-pass"}`)
	if resp, answer := call(t, "POST", base+"/api/session", body, map[string]string{"Content-Type": "application/json"}); resp.StatusCode != 401 {
		t.Fatalf("sign in %d %s", resp.StatusCode, answer)
	}
	log := strings.Join(stop(), "\n")
	if strings.Contains(log, typed) {
		t.Errorf("standard error holds the typed password")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "logs", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		read++
		if strings.Contains(string(data), typed) {
			t.Errorf("%s holds the typed password", e.Name())
		}
	}
	got, _ := auditLines(t, dir)
	signIns := 0
	for _, line := range got {
		if line.Action == "session.sign_in" {
			signIns++
			if line.Subject != "unknown" || line.Result != "denied" {
				t.Errorf("sign in line %+v", line)
			}
		}
	}
	if read < 4 || signIns != 1 {
		t.Fatalf("%d files read, %d sign in lines", read, signIns)
	}
}
