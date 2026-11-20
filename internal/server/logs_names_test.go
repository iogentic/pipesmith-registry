package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLogFileNameThatIsNoLogFileAnswers404: a plain name that is no log file
// of this server answers 404 like a missing log file, opens nothing and
// echoes nothing of the file (docs/ui/API.md lines 49 and 551; the contract
// walk sends GET /api/logs/demo-walk-unknown and accepts 404).
func TestLogFileNameThatIsNoLogFileAnswers404(t *testing.T) {
	a, dir, _ := logsApp(t)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/api/logs/notes.txt", "/api/logs/logs.lock", "/api/logs/demo-walk-unknown"} {
		for _, q := range []string{"", "?download=1"} {
			r := a.send("GET", target+q, "basic", asAdmin)
			if r.status != 404 || strings.Contains(string(r.body), "notes") || r.header.Get("Content-Disposition") != "" {
				t.Errorf("%s%s: %d %q", target, q, r.status, r.body)
			}
		}
	}
}
