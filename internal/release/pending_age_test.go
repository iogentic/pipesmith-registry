package release

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ageRecord sets the time the record of the switch was written to age ago.
func (r *rig) ageRecord(age time.Duration) {
	r.t.Helper()
	path := filepath.Join(r.l.Root, PendingFile)
	data, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		r.t.Fatal(err)
	}
	p["written"] = time.Now().Add(-age).UTC().Format(time.RFC3339Nano)
	if data, err = json.Marshal(p); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// leftAsItIs runs f and fails unless it returns ErrStaleSwitch, the tree
// below the root is equal before and after, and nothing was restarted or
// stopped.
func (r *rig) leftAsItIs(f func() error) error {
	r.t.Helper()
	before, restarts := tree(r.t, r.l.Root), r.svc.restartCount()
	r.svc.mu.Lock()
	stops := len(r.svc.stops)
	r.svc.mu.Unlock()
	err := f()
	if !errors.Is(err, ErrStaleSwitch) {
		r.t.Fatalf("error %v, want %v\n%s", err, ErrStaleSwitch, r.out.String())
	}
	if after := tree(r.t, r.l.Root); after != before {
		r.t.Fatalf("the tree changed:\nbefore\n%s\nafter\n%s", before, after)
	}
	r.svc.mu.Lock()
	defer r.svc.mu.Unlock()
	if len(r.svc.restarts) != restarts || len(r.svc.stops) != stops {
		r.t.Fatalf("%d restarts and %d stops after a refusal", len(r.svc.restarts)-restarts, len(r.svc.stops)-stops)
	}
	return err
}

// An update that migrated the data files ended healthy, but its run was
// killed before it removed the record. Days later the unit is stopped for
// maintenance and an operator runs update: the record is older than the
// restart bound of that switch, so the command leaves links, data and record
// as they are and says what to run (finding R3-1).
func TestStaleRecordChangesNothing(t *testing.T) {
	sr := newSchemaRig(t, schemaN+1)
	sr.rig.svc.unhealthy["0.2.0"] = false
	sr.u.hook = kill("kill-after-restart")
	if err := sr.u.Update(sr.dir, false); err == nil || !strings.Contains(err.Error(), "killed at") {
		t.Fatalf("killed update: %v\n%s", err, sr.out.String())
	}
	sr.u.hook = nil
	if err := sr.svc.Stop(context.Background(), DefaultUnit); err != nil {
		t.Fatal(err)
	}
	sr.ageRecord(48 * time.Hour)
	data := tree(t, sr.data)
	err := sr.leftAsItIs(func() error { return sr.u.Update(sr.dir, false) })
	for _, words := range []string{"releases/0.1.0 to releases/0.2.0", "current points to releases/0.2.0", "rm " + filepath.Join(sr.l.Root, PendingFile),
		"rollback --root " + sr.l.Root} {
		if !strings.Contains(err.Error(), words) {
			t.Fatalf("the refusal does not say %q: %v", words, err)
		}
	}
	if got := tree(t, sr.data); got != data {
		t.Fatalf("the data directory changed:\nbefore\n%s\nafter\n%s", data, got)
	}
	if _, err := os.Lstat(filepath.Join(sr.l.Root, PendingFile)); err != nil {
		t.Fatalf("the record is gone: %v", err)
	}
	// rollback is refused the same way
	sr.leftAsItIs(sr.u.Rollback)
	if got := tree(t, sr.data); got != data {
		t.Fatalf("the data directory changed:\nbefore\n%s\nafter\n%s", data, got)
	}
}

// A stale record of a switch whose release answers healthy now is ended
// without a change: the switch had ended.
func TestStaleRecordOfAHealthyRelease(t *testing.T) {
	r := newRig(t, "0.1.0")
	r.u.hook = kill("kill-after-restart")
	if err := r.u.Update(r.release(versionScript("0.2.0")), false); err == nil {
		t.Fatal("the killed update passed")
	}
	r.u.hook = nil
	r.ageRecord(48 * time.Hour)
	err := r.u.Rollback()
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "had ended") {
		t.Fatalf("next command: %v", err)
	}
	if r.link(CurrentLink) != "releases/0.2.0" || r.svc.restartCount() != 1 {
		t.Fatalf("current %q, %d restarts", r.link(CurrentLink), r.svc.restartCount())
	}
	r.noPending()
}

// A record without the time it was written, as a record of an older build,
// counts as stale.
func TestRecordWithoutTimeIsStale(t *testing.T) {
	r := rollbackRig(t)
	if err := os.WriteFile(filepath.Join(r.l.Root, PendingFile), []byte(`{"action":"update","from":"0.1.0","to":"0.2.0","previous":"","previous_existed":false,"back":false,"files":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.svc.unhealthy["0.2.0"] = true
	r.leftAsItIs(r.u.Rollback)
}

// A record young enough that the restart of its switch can still be in
// flight is not decided by one probe: the command waits until the release
// answers healthy, so a start that is slow is not taken back.
func TestYoungRecordWaitsForTheHealth(t *testing.T) {
	r := newRig(t, "0.1.0")
	r.u.RestartTimeout = time.Second
	r.u.hook = kill("kill-after-switch")
	if err := r.u.Update(r.release(versionScript("0.2.0")), false); err == nil {
		t.Fatal("the killed update passed")
	}
	r.u.hook = nil
	// the restart job of the service manager ends after the kill; the new
	// release answers 503 a few times while it starts
	r.svc.late = 20
	if err := r.svc.Restart(context.Background(), DefaultUnit); err != nil {
		t.Fatal(err)
	}
	err := r.u.Rollback()
	if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "had ended") {
		t.Fatalf("next command: %v\n%s", err, r.out.String())
	}
	if r.link(CurrentLink) != "releases/0.2.0" || r.svc.running != "0.2.0" {
		t.Fatalf("current %q running %q", r.link(CurrentLink), r.svc.running)
	}
	r.noPending()
}
