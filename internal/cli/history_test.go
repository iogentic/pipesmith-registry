package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/release"
)

// serve finds the history of updates two levels above its binary when it
// runs as <root>/releases/<version>/pipesmith-registry, also through the
// link current, and writes one audit line per history line.
func TestServeShipsTheHistory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, release.ReleasesDir, "0.2.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, release.InstalledBinary)
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/0.2.0", filepath.Join(root, release.CurrentLink)); err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(root, release.ReleasesDir, release.HistoryFile)
	if got := historyOf(filepath.Join(root, release.CurrentLink, release.InstalledBinary)); got != history {
		t.Fatalf("history %q, want %q", got, history)
	}
	for _, other := range []string{filepath.Join(root, "pipesmith-registry"), filepath.Join(root, "none")} {
		if got := historyOf(other); got != "" {
			t.Fatalf("%s: history %q", other, got)
		}
	}
	line, _ := json.Marshal(release.HistoryEntry{ID: "20261120T000000.000000000Z-00000000", Action: "update", Result: "ok", From: "0.1.0", To: "0.2.0", Reason: "r"})
	if err := os.WriteFile(history, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := executable
	executable = func() (string, error) { return bin, nil }
	t.Cleanup(func() { executable = saved })
	logDir := t.TempDir()
	files := logs.Open(logs.Options{Dir: logDir})
	shipHistory(logDir, files)()
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	audit, err := os.ReadFile(filepath.Join(logDir, "audit.log"))
	if err != nil || strings.Count(string(audit), "\n") != 1 || !strings.Contains(string(audit), `"object_name":"0.1.0 -> 0.2.0"`) {
		t.Fatalf("audit.log %q %v", audit, err)
	}
	if stop := shipHistory("", files); stop == nil {
		t.Fatal("no stop function with the files off")
	}
}
