package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// export_max_files and export_max_bytes (call B18 of
// docs/plans/browse-tools.md) reach the bounds of the export: config.json
// first, the serve options over it. The folder docs holds two files of 5
// bytes each.

func exportTree(t *testing.T) {
	t.Helper()
	dir := dataDir(t)
	tree, err := testkit.MakeTree(filepath.Join(filepath.Dir(dir), "tree"), map[string][]byte{
		"demo-raw/docs/a.txt": []byte("alpha"), "demo-raw/docs/b.txt": []byte("bravo"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, "store", "add", "tree", "--kind", "directory", "--root", tree); code != 0 {
		t.Fatalf("store add: %s", errOut)
	}
	if code, _, errOut := run(t, "scan", "tree"); code != 0 {
		t.Fatalf("scan: %s", errOut)
	}
}

// exportOf is the status of the export of docs and its refusal body.
func exportOf(t *testing.T, args ...string) (int, map[string]any) {
	t.Helper()
	base, stop := serveData(t, args...)
	resp, data := call(t, "GET", base+"/api/export/demo-raw?path=docs", nil, nil)
	stop()
	var body map[string]any
	if resp.StatusCode != 200 {
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatalf("%d %q: %v", resp.StatusCode, data, err)
		}
	}
	return resp.StatusCode, body
}

func TestExportBoundsReachTheExport(t *testing.T) {
	exportTree(t)
	if status, body := exportOf(t); status != 200 {
		t.Fatalf("at the defaults: %d %v", status, body)
	}
	for _, c := range []struct {
		name         string
		files, bytes int64
		args         []string
		status       int
		maxFiles     float64
		maxBytes     float64
	}{
		{"option files", 0, 0, []string{"--export-max-files", "1"}, 413, 1, 2 << 30},
		{"option files at the count", 0, 0, []string{"--export-max-files", "2"}, 200, 0, 0},
		{"option bytes", 0, 0, []string{"--export-max-bytes", "9"}, 413, 20_000, 9},
		{"option bytes at the sum", 0, 0, []string{"--export-max-bytes", "10"}, 200, 0, 0},
		{"config files", 1, 0, nil, 413, 1, 2 << 30},
		{"config bytes", 0, 9, nil, 413, 20_000, 9},
		{"option over config", 1, 9, []string{"--export-max-files", "2", "--export-max-bytes", "10"}, 200, 0, 0},
	} {
		saveConfig(t, func(cfg *config.Config) { cfg.ExportMaxFiles, cfg.ExportMaxBytes = c.files, c.bytes })
		status, body := exportOf(t, c.args...)
		if status != c.status || (status == 413 && (body["max_files"] != c.maxFiles || body["max_bytes"] != c.maxBytes)) {
			t.Errorf("%s: %d %v", c.name, status, body)
		}
	}
}

func TestExportBoundOptionsRefused(t *testing.T) {
	dataDir(t)
	for _, c := range []struct{ args []string }{
		{[]string{"serve", "--export-max-files", "0"}},
		{[]string{"serve", "--export-max-bytes", "-5"}},
		{[]string{"serve", "--export-max-bytes", "9223372036854775808"}},
	} {
		code, _, errOut := run(t, c.args...)
		if want := "error: " + c.args[1] + " is not a whole number above 0: " + c.args[2] + "\n"; code != 1 || errOut != want {
			t.Errorf("%v: %d %q, want %q", c.args, code, errOut, want)
		}
	}
	if code, _, errOut := run(t, "serve", "--export-max-files", "many"); code != 2 ||
		!strings.Contains(errOut, "serve: argument --export-max-files: invalid int value: 'many'\n") {
		t.Errorf("a word: %d %q", code, errOut)
	}
}
