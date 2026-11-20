package release

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// kill makes the hook point a kill: the run returns there without a way
// back, as a process that was killed.
func kill(point string) func(string) error {
	return func(p string) error {
		if p == point {
			return errors.New("killed at " + p)
		}
		return nil
	}
}

func (r *rig) noPending() {
	r.t.Helper()
	if _, err := os.Lstat(filepath.Join(r.l.Root, PendingFile)); !errors.Is(err, os.ErrNotExist) {
		r.t.Fatalf("%s: %v", PendingFile, err)
	}
}

// An update killed after the links changed: the next command finds the
// record of the switch, goes back to the release before and returns; the
// same update then passes (finding A-3).
func TestKilledAfterTheSwitchGoesBack(t *testing.T) {
	r := newRig(t, "0.1.0")
	r.u.RestartTimeout = time.Second // the restart bound the record holds, so the next command waits briefly
	dir := r.release(versionScript("0.2.0"))
	r.u.hook = kill("kill-after-switch")
	if err := r.u.Update(dir, false); err == nil || !strings.Contains(err.Error(), "killed at") {
		t.Fatalf("killed update: %v", err)
	}
	if r.link(CurrentLink) != "releases/0.2.0" || r.svc.running != "0.1.0" {
		t.Fatalf("after the kill: current %q, running %q", r.link(CurrentLink), r.svc.running)
	}
	r.u.hook = nil
	err := r.u.Update(dir, false)
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "went back to releases/0.1.0, which answers again") {
		t.Fatalf("next command: %v\n%s", err, r.out.String())
	}
	if r.link(CurrentLink) != "releases/0.1.0" || r.link(PreviousLink) != "" || r.svc.running != "0.1.0" {
		t.Fatalf("current %q previous %q running %q", r.link(CurrentLink), r.link(PreviousLink), r.svc.running)
	}
	r.noPending()
	if err := r.u.Update(dir, false); err != nil {
		t.Fatalf("the same update again: %v\n%s", err, r.out.String())
	}
	if r.link(CurrentLink) != "releases/0.2.0" || r.svc.running != "0.2.0" {
		t.Fatalf("current %q running %q", r.link(CurrentLink), r.svc.running)
	}
	r.noPending()
}

// An update killed after the new release answered healthy: the next command
// sees the switch ended, changes nothing and returns.
func TestKilledAfterTheRestartFinishes(t *testing.T) {
	r := newRig(t, "0.1.0")
	r.u.hook = kill("kill-after-restart")
	if err := r.u.Update(r.release(versionScript("0.2.0")), false); err == nil {
		t.Fatal("the killed update passed")
	}
	r.u.hook = nil
	restarts := r.svc.restartCount()
	err := r.u.Rollback()
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "had ended") {
		t.Fatalf("next command: %v", err)
	}
	if r.link(CurrentLink) != "releases/0.2.0" || r.link(PreviousLink) != "releases/0.1.0" || r.svc.restartCount() != restarts {
		t.Fatalf("current %q previous %q, %d restarts", r.link(CurrentLink), r.link(PreviousLink), r.svc.restartCount()-restarts)
	}
	r.noPending()
	if err := r.u.Rollback(); err != nil {
		t.Fatalf("rollback after that: %v", err)
	}
}

// A rollback killed after the links changed is taken back by the next
// command as well.
func TestKilledRollbackGoesBack(t *testing.T) {
	r := rollbackRig(t)
	record := fmt.Sprintf(`{"action":"rollback","from":"0.2.0","to":"0.1.0","previous":"releases/0.1.0","previous_existed":true,"back":false,"files":[],"written":%q,"bound_seconds":1}`,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(r.l.Root, PendingFile), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r.l.Root, CurrentLink)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/0.1.0", filepath.Join(r.l.Root, CurrentLink)); err != nil {
		t.Fatal(err)
	}
	r.svc.unhealthy["0.1.0"] = true
	err := r.u.Rollback()
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "went back to releases/0.2.0") {
		t.Fatalf("next command: %v", err)
	}
	if r.link(CurrentLink) != "releases/0.2.0" || r.link(PreviousLink) != "releases/0.1.0" || r.svc.running != "0.2.0" {
		t.Fatalf("current %q previous %q running %q", r.link(CurrentLink), r.link(PreviousLink), r.svc.running)
	}
	r.noPending()
}

// A record that another user may write, or that names no release, is
// refused before anything changes.
func TestPendingRecordRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		text string
		mode os.FileMode
		want error
	}{
		"writable":  {`{"action":"update","from":"0.1.0","to":"0.2.0","files":[]}`, 0o622, ErrPermissions},
		"bad name":  {`{"action":"update","from":"../x","to":"0.2.0","files":[]}`, 0o600, ErrLayout},
		"bad file":  {`{"action":"update","from":"0.1.0","to":"0.2.0","files":[{"name":"x.sqlite","from":1}]}`, 0o600, ErrLayout},
		"no object": {`[]`, 0o600, ErrLayout},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "0.1.0")
			p := filepath.Join(r.l.Root, PendingFile)
			if err := os.WriteFile(p, []byte(c.text), c.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, c.mode); err != nil {
				t.Fatal(err)
			}
			r.unchanged(c.want, r.u.Rollback)
		})
	}
}

// died is the panic of die.
type died string

// die makes the hook point a kill that skips every step after it but the
// deferred calls: the run panics there, and dies recovers.
func die(point string) func(string) error {
	return func(p string) error {
		if p == point {
			panic(died(p))
		}
		return nil
	}
}

// dies runs f and returns true when it panicked at a point of die.
func dies(f func() error) (killed bool) {
	defer func() {
		if v := recover(); v != nil {
			if _, ok := v.(died); !ok {
				panic(v)
			}
			killed = true
		}
	}()
	_ = f()
	return false
}

// A rollback killed between the change of previous and the change of
// current: the next command puts previous back from the record, so a
// rollback after it goes back to the release before (finding R3-4).
func TestKilledBetweenTheLinksPutsPreviousBack(t *testing.T) {
	r := rollbackRig(t)
	r.u.hook = die("swap-current")
	if !dies(r.u.Rollback) {
		t.Fatal("the rollback was not killed")
	}
	if r.link(CurrentLink) != "releases/0.2.0" || r.link(PreviousLink) != "releases/0.2.0" {
		t.Fatalf("after the kill: current %q previous %q", r.link(CurrentLink), r.link(PreviousLink))
	}
	r.u.hook = nil
	err := r.u.Rollback()
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "previous points to releases/0.1.0 again") {
		t.Fatalf("next command: %v\n%s", err, r.out.String())
	}
	if r.link(CurrentLink) != "releases/0.2.0" || r.link(PreviousLink) != "releases/0.1.0" || r.svc.restartCount() != 0 {
		t.Fatalf("current %q previous %q, %d restarts", r.link(CurrentLink), r.link(PreviousLink), r.svc.restartCount())
	}
	r.noPending()
	if err := r.u.Rollback(); err != nil || r.link(CurrentLink) != "releases/0.1.0" {
		t.Fatalf("rollback after that: %v, current %q", err, r.link(CurrentLink))
	}
	// an update killed there: previous did not exist before, and goes again
	u := newRig(t, "0.1.0")
	u.u.hook = die("swap-current")
	dir := u.release(versionScript("0.2.0"))
	if !dies(func() error { return u.u.Update(dir, false) }) || u.link(PreviousLink) != "releases/0.1.0" {
		t.Fatalf("the update was not killed there: previous %q", u.link(PreviousLink))
	}
	u.u.hook = nil
	if err := u.u.Update(dir, false); !errors.Is(err, ErrInterrupted) || u.link(PreviousLink) != "" {
		t.Fatalf("next command: %v, previous %q", err, u.link(PreviousLink))
	}
}
