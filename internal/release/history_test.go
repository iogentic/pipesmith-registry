package release

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// jsonLines reads every line of path as a JSON object.
func jsonLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("%s: %q: %v", path, sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

// ship hands the history of the rig to log files in a new log directory,
// as serve does, and returns that directory.
func (r *rig) ship() string {
	r.t.Helper()
	dir := filepath.Join(r.t.TempDir(), "logs")
	files := logs.Open(logs.Options{Dir: dir})
	history := filepath.Join(r.l.Releases(), HistoryFile)
	cursor := filepath.Join(dir, HistoryCursor)
	for range 2 { // the second run finds nothing new
		if _, err := ShipHistory(history, cursor, files); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := files.Close(); err != nil {
		r.t.Fatal(err)
	}
	return dir
}

// Each of a good update, a refused update and a way back writes one line to
// releases/history.jsonl, and the server writes one audit line and one
// server log line from it, with the same versions and reason (task 8.9,
// finding A-8).
func TestHistoryLines(t *testing.T) {
	for name, c := range map[string]struct {
		run    func(r *rig) error
		result string
		audit  string
		to     string
		reason string
	}{
		"good update": {func(r *rig) error { return r.u.Update(r.release(versionScript("0.2.0")), false) }, "ok", "ok", "0.2.0",
			"releases/0.2.0 answers healthy"},
		"refused update": {func(r *rig) error { return r.u.Update(r.release(versionScript("0.1.0")), false) }, "refused", "denied", "0.1.0",
			"0.1.0 is the version current runs"},
		"way back": {func(r *rig) error {
			r.svc.unhealthy["0.2.0"] = true
			r.u.HealthTimeout = 200 * time.Millisecond
			return r.u.Update(r.release(versionScript("0.2.0")), false)
		}, "back", "failed", "0.2.0", "went back to releases/0.1.0, which answers again"},
		"way back to a release that does not answer": {func(r *rig) error {
			r.svc.unhealthy["0.2.0"], r.svc.unhealthy["0.1.0"] = true, true
			r.u.HealthTimeout = 200 * time.Millisecond
			return r.u.Update(r.release(versionScript("0.2.0")), false)
		}, "back", "failed", "0.2.0", "went back to releases/0.1.0, which does not answer either"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "0.1.0")
			err := c.run(r)
			if (err == nil) != (c.result == "ok") {
				t.Fatalf("run: %v", err)
			}
			if want := strings.Contains(c.reason, "which answers again"); r.u.WentBack() != want {
				t.Fatalf("WentBack %v, want %v", r.u.WentBack(), want)
			}
			history := jsonLines(t, filepath.Join(r.l.Releases(), HistoryFile))
			if len(history) != 1 {
				t.Fatalf("history %v", history)
			}
			h := history[0]
			reason, _ := h["reason"].(string)
			if h["action"] != "update" || h["result"] != c.result || h["from"] != "0.1.0" || h["to"] != c.to || !strings.Contains(reason, c.reason) {
				t.Fatalf("history line %v", h)
			}
			if err != nil && reason != err.Error() {
				t.Fatalf("reason %q, error %q", reason, err)
			}
			dir := r.ship()
			audit := jsonLines(t, filepath.Join(dir, "audit.log"))
			if len(audit) != 1 {
				t.Fatalf("audit.log %v", audit)
			}
			a := audit[0]
			if a["action"] != "release.update" || a["result"] != c.audit || a["object_type"] != "release" ||
				a["object_name"] != "0.1.0 -> "+c.to || a["reason"] != reason || a["request_id"] != h["id"] {
				t.Fatalf("audit line %v", a)
			}
			var server []map[string]any
			for _, l := range jsonLines(t, filepath.Join(dir, "server.log")) {
				if l["logger"] == HistoryLogger {
					server = append(server, l)
				}
			}
			if len(server) != 1 {
				t.Fatalf("server.log %v", server)
			}
			s := server[0]
			if s["from"] != "0.1.0" || s["to"] != c.to || s["result"] != c.result || s["reason"] != reason || s["id"] != h["id"] {
				t.Fatalf("server line %v", s)
			}
		})
	}
}

// A rollback writes its line too, and a history file that shrank is read
// again from its start.
func TestHistoryRollbackAndCursor(t *testing.T) {
	r := rollbackRig(t)
	if err := r.u.Rollback(); err != nil {
		t.Fatal(err)
	}
	history := jsonLines(t, filepath.Join(r.l.Releases(), HistoryFile))
	if len(history) != 1 || history[0]["action"] != "rollback" || history[0]["from"] != "0.2.0" || history[0]["to"] != "0.1.0" || history[0]["result"] != "ok" {
		t.Fatalf("history %v", history)
	}
	dir := filepath.Join(t.TempDir(), "logs")
	files := logs.Open(logs.Options{Dir: dir})
	defer files.Close()
	path, cursor := filepath.Join(r.l.Releases(), HistoryFile), filepath.Join(dir, HistoryCursor)
	if n, err := ShipHistory(path, cursor, files); n != 1 || err != nil {
		t.Fatalf("first: %d %v", n, err)
	}
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.u.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n, err := ShipHistory(path, cursor, files); n != 1 || err != nil {
		t.Fatalf("after the file shrank: %d %v", n, err)
	}
	if n, err := ShipHistory(filepath.Join(t.TempDir(), "none"), cursor, files); n != 0 || err != nil {
		t.Fatalf("no history: %d %v", n, err)
	}
}
