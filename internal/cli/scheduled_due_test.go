package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// writeRecord puts the record of an update from 0.1.0 to 0.2.0 that stopped
// before the links changed into the root, written now.
func (f *fetchFixture) writeRecord(t *testing.T) string {
	t.Helper()
	path := filepath.Join(f.root, release.PendingFile)
	text := fmt.Sprintf(`{"action":"update","from":"0.1.0","to":"0.2.0","previous":"","previous_existed":false,"back":false,"files":[],"written":%q,"bound_seconds":60}`,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The scheduled run ends the record of a switch of a run that stopped before
// anything else, and exits with 1 (finding R3-2).
func TestScheduledRunEndsARecordFirst(t *testing.T) {
	f, svc := newScheduledFixture(t, "true", "0.2.0")
	path := f.writeRecord(t)
	code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 1 || !strings.Contains(errOut, "stopped before the links changed") || len(f.stub.Requests()) != 0 || f.staged(t, "0.2.0") || svc.restarts != 0 {
		t.Fatalf("%d %q, %d requests, %d restarts\n%s", code, errOut, len(f.stub.Requests()), svc.restarts, out)
	}
	if _, err := os.Lstat(path); err == nil {
		t.Fatal("the record stayed")
	}
	if code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root); code != 0 || f.current(t) != "releases/0.2.0" {
		t.Fatalf("the run after: %d %q\n%s", code, errOut, out)
	}
}

// Without scheduled_switch the timer does not end a switch: it refuses with
// exit code 1, names the record and fetches nothing.
func TestScheduledRunWithoutTheSwitchRefusesARecord(t *testing.T) {
	f, svc := newScheduledFixture(t, "false", "0.2.0")
	path := f.writeRecord(t)
	code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 1 || !strings.Contains(errOut, path) || len(f.stub.Requests()) != 0 || svc.restarts != 0 {
		t.Fatalf("%d %q, %d requests, %d restarts\n%s", code, errOut, len(f.stub.Requests()), svc.restarts, out)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the record is gone: %v", err)
	}
}

// A version the scheduled run staged and could not switch because another
// update held the lock is switched by the next scheduled run, not left with
// exit code 0 (finding R3-2).
func TestScheduledSwitchAfterTheLockTriesAgain(t *testing.T) {
	f, svc := newScheduledFixture(t, "true", "0.2.0")
	h, err := release.HoldRoot(f.root)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	h.Release()
	if code != 1 || !strings.Contains(errOut, "another update or rollback runs") || svc.restarts != 0 || !f.staged(t, "0.2.0") {
		t.Fatalf("locked run: %d %q, %d restarts\n%s", code, errOut, svc.restarts, out)
	}
	code, out, errOut = run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 0 || f.current(t) != "releases/0.2.0" || svc.restarts != 1 {
		t.Fatalf("next run: %d %q, current %s, %d restarts\n%s", code, errOut, f.current(t), svc.restarts, out)
	}
	if !strings.Contains(out, "scheduled_switch: switching to 0.2.0") {
		t.Fatalf("output:\n%s", out)
	}
}

// A switch refused before it began, here for a config.json the health wait
// cannot read, leaves no mark of a failed switch: the next scheduled run
// tries it again (finding R3-6).
func TestScheduledSwitchRefusedBeforeItBegan(t *testing.T) {
	f, svc := newScheduledFixture(t, "true", "0.2.0")
	config := filepath.Join(f.data, "config.json")
	good, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 1 || svc.restarts != 0 || f.current(t) != "releases/0.1.0" {
		t.Fatalf("refused run: %d %q, %d restarts\n%s", code, errOut, svc.restarts, out)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "releases", ".switch-failed-0.2.0")); err == nil {
		t.Fatal("a mark of a failed switch for a switch that never began")
	}
	if err := os.WriteFile(config, good, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 0 || f.current(t) != "releases/0.2.0" {
		t.Fatalf("next run: %d %q, current %s\n%s", code, errOut, f.current(t), out)
	}
}
