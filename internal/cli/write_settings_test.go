package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The command repos write-settings <name>: the write policy, the two limits
// and the write store of a hosted repository, kept in its record in
// state.sqlite.

// writeSettingsData is a data directory whose state.sqlite holds a hosted
// raw repository with a write policy of the old product and a proxy.
func writeSettingsData(t *testing.T) string {
	t.Helper()
	dir := dataDir(t)
	st, err := state.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap := &model.Snapshot{Repositories: []model.Repository{
		{Name: "demo-raw", Format: "raw", Type: "hosted", Online: true, Attributes: map[string]any{
			"storage": map[string]any{"blobStoreName": "default", "writePolicy": "ALLOW_ONCE", "strictContentTypeValidation": true}}},
		{Name: "demo-raw-proxy", Format: "raw", Type: "proxy", Online: true, Attributes: map[string]any{
			"proxy": map[string]any{"remoteUrl": "https://repo.example.com/raw/"}}},
	}}
	if err := st.Write([]state.Kind{state.KindRepositories}, snap); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// storedRecords reads the repository records of state.sqlite.
func storedRecords(t *testing.T, dir string) []model.Repository {
	t.Helper()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	return snap.Repositories
}

// settingsOf are the write settings the server reads from the records.
func settingsOf(t *testing.T, dir, name string) repos.WriteSettings {
	t.Helper()
	repo, ok := repos.New(nil, nil, nil).WithAttributes(storedRecords(t, dir)).Get(name)
	if !ok {
		t.Fatalf("no repository %s", name)
	}
	return repo.WriteSettings()
}

func TestReposWriteSettingsShow(t *testing.T) {
	writeSettingsData(t)
	code, out, errOut := run(t, "repos", "write-settings", "demo-raw")
	want := "write settings of demo-raw:\n" +
		"  storage.writePolicy     ALLOW_ONCE\n" +
		"  max_body_bytes          10737418240 (default)\n" +
		"  free_space_floor_bytes  2147483648 (default)\n" +
		"  write_store             local-write (default)\n"
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
}

func TestReposWriteSettingsSet(t *testing.T) {
	dir := writeSettingsData(t)
	before := storedRecords(t, dir)
	code, out, errOut := run(t, "repos", "write-settings", "demo-raw", "--write-policy", "DENY",
		"--max-body-bytes", "1048576", "--free-space-floor-bytes", "0", "--write-store", "local-write")
	want := "write settings of demo-raw:\n" +
		"  storage.writePolicy     DENY\n" +
		"  max_body_bytes          1048576\n" +
		"  free_space_floor_bytes  0\n" +
		"  write_store             local-write\n" +
		"a running server reads them at its next load of state.sqlite\n"
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	got := settingsOf(t, dir, "demo-raw")
	if got.WritePolicy != "DENY" || got.MaxBodyBytes != 1048576 || got.FreeSpaceFloor != 0 || got.WriteStore != "local-write" {
		t.Fatalf("the server reads %+v", got)
	}
	// the other attributes of the record and the other record stay
	if !got.StrictContentType || got.BlobStoreName != "default" {
		t.Fatalf("storage lost a key: %+v", got)
	}
	after := storedRecords(t, dir)
	if !reflect.DeepEqual(after[1], before[1]) {
		t.Fatalf("the proxy changed: %+v", after[1])
	}
	if n, ok := after[0].Attributes["max_body_bytes"].(json.Number); !ok || n != "1048576" {
		t.Fatalf("max_body_bytes is stored as %#v, want a number", after[0].Attributes["max_body_bytes"])
	}

	// one option changes one setting
	if code, _, errOut := run(t, "repos", "write-settings", "demo-raw", "--max-body-bytes", "5"); code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	got = settingsOf(t, dir, "demo-raw")
	if got.WritePolicy != "DENY" || got.MaxBodyBytes != 5 || got.FreeSpaceFloor != 0 {
		t.Fatalf("after one option the server reads %+v", got)
	}
}

func TestReposWriteSettingsRefusals(t *testing.T) {
	dir := writeSettingsData(t)
	before := storedRecords(t, dir)
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"demo-missing", "--write-policy", "DENY"}, 1, "no repository named demo-missing"},
		{[]string{"demo-raw-proxy", "--write-policy", "DENY"}, 1, "demo-raw-proxy is a proxy repository"},
		{[]string{"demo-raw", "--write-store", "other-store"}, 1, "writes into the store local-write"},
		{[]string{"demo-raw", "--max-body-bytes", "0"}, 1, "--max-body-bytes is at least 1"},
		{[]string{"demo-raw", "--max-body-bytes", "-1"}, 2, "argument --max-body-bytes"},
		{[]string{"demo-raw", "--free-space-floor-bytes", "1e9"}, 2, "argument --free-space-floor-bytes"},
		{[]string{"demo-raw", "--max-body-bytes", "99999999999999999999"}, 2, "argument --max-body-bytes"},
		{[]string{"demo-raw", "--write-policy", "allow"}, 2, "invalid choice: 'allow'"},
	} {
		code, out, errOut := run(t, append([]string{"repos", "write-settings"}, c.args...)...)
		if code != c.code || out != "" || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: %d %q %q, want %d and %q", c.args, code, out, errOut, c.code, c.want)
		}
	}
	if after := storedRecords(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refusal changed the records:\n%+v\n%+v", before, after)
	}
	if got := settingsOf(t, dir, "demo-raw"); got.WritePolicy != "ALLOW_ONCE" {
		t.Fatalf("the server reads %+v", got)
	}
}

func TestReposWriteSettingsHelp(t *testing.T) {
	code, out, _ := run(t, "repos", "write-settings", "--help")
	if code != 0 || out != commandHelp["repos write-settings"] ||
		!strings.HasPrefix(out, "usage: pipesmith-registry repos write-settings [-h]\n") {
		t.Fatalf("%d %q", code, out)
	}
	if code, out, _ := run(t, "help"); code != 0 || !strings.Contains(out, "  repos write-settings <name>") {
		t.Fatalf("help: %d %q", code, out)
	}
}
