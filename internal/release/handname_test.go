package release

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A first release installed by hand into a directory whose name is not its
// version: rollback waits for the version its binary answers, and the way
// forward again waits for the version of 0.2.0 (finding A-14).
func TestRollbackToADirectoryNamedOtherwise(t *testing.T) {
	r := newRig(t, "0.2.0", "first")
	if err := os.WriteFile(filepath.Join(r.l.Release("first"), InstalledBinary), versionScript("0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/first", filepath.Join(r.l.Root, PreviousLink)); err != nil {
		t.Fatal(err)
	}
	r.svc.versions = map[string]string{"first": "0.1.0"}
	if err := r.u.Rollback(); err != nil {
		t.Fatalf("rollback: %v\n%s", err, r.out.String())
	}
	if r.link(CurrentLink) != "releases/first" || r.svc.running != "first" {
		t.Fatalf("current %q running %q", r.link(CurrentLink), r.svc.running)
	}
	r.svc.unhealthy["0.2.0"] = true
	r.u.HealthTimeout = 200e6
	err := r.u.Rollback()
	if err == nil || r.link(CurrentLink) != "releases/first" || r.svc.running != "first" {
		t.Fatalf("the way back to first: %v, current %q running %q\n%s", err, r.link(CurrentLink), r.svc.running, r.out.String())
	}
	if !strings.Contains(err.Error(), "went back to releases/first, which answers again") {
		t.Fatalf("message %q", err)
	}
}

// A rollback to a directory named otherwise that ended healthy but whose run
// was killed before it removed the record: the next command asks the server
// for the version the binary of that directory answers, sees the switch
// ended and does not switch back (finding R3-7).
func TestEndedRollbackToADirectoryNamedOtherwise(t *testing.T) {
	r := newRig(t, "first", "0.2.0")
	if err := os.WriteFile(filepath.Join(r.l.Release("first"), InstalledBinary), versionScript("0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/0.2.0", filepath.Join(r.l.Root, PreviousLink)); err != nil {
		t.Fatal(err)
	}
	r.svc.versions = map[string]string{"first": "0.1.0"}
	r.u.HealthTimeout = 200e6
	record := `{"action":"rollback","from":"0.2.0","to":"first","previous":"releases/first","previous_existed":true,"back":false,"files":[],"written":"` +
		time.Now().UTC().Format(time.RFC3339Nano) + `","bound_seconds":1}`
	if err := os.WriteFile(filepath.Join(r.l.Root, PendingFile), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	err := r.u.Rollback()
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "had ended") {
		t.Fatalf("next command: %v\n%s", err, r.out.String())
	}
	if r.link(CurrentLink) != "releases/first" || r.svc.running != "first" || r.svc.restartCount() != 0 {
		t.Fatalf("current %q running %q, %d restarts", r.link(CurrentLink), r.svc.running, r.svc.restartCount())
	}
	r.noPending()
}
