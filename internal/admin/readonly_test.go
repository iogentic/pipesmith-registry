package admin

import (
	"context"
	"strings"
	"testing"
	"time"
)

// refusedWith is the message of a refusal of the read only mode, or fails.
func refusedWith(t *testing.T, err error) string {
	t.Helper()
	status, body := Answer(err)
	m, ok := body.(Message)
	if status != 409 || !ok {
		t.Fatalf("%v: %d %+v, want 409", err, status, body)
	}
	return m.Message
}

// Ruling SB2: the mode starts released; a freeze by a user ends with
// release and not with force release; a freeze of the system ends with
// force release and not with release; a second freeze and a release
// without a freeze are refused with 409. Each state says that a restart
// releases the freeze.
func TestReadOnlyMode(t *testing.T) {
	at := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	m := NewReadOnlyMode(func() time.Time { return at })
	if s := m.State(); s.Frozen || s.SystemInitiated || !strings.Contains(s.Reason, "a restart") {
		t.Fatalf("at start %+v", s)
	}
	if msg := refusedWith(t, m.Release()); msg != "the server is not frozen" {
		t.Fatalf("release %q", msg)
	}
	if msg := refusedWith(t, m.ForceRelease()); msg != "the server is not frozen" {
		t.Fatalf("force release %q", msg)
	}
	if err := m.Freeze(); err != nil {
		t.Fatal(err)
	}
	s := m.State()
	if !s.Frozen || s.SystemInitiated || !strings.Contains(s.Reason, "2026-11-20T08:00:00Z") || !strings.Contains(s.Reason, "a restart") {
		t.Fatalf("frozen %+v", s)
	}
	if msg := refusedWith(t, m.Freeze()); msg != "the server is frozen already" {
		t.Fatalf("second freeze %q", msg)
	}
	if msg := refusedWith(t, m.ForceRelease()); !strings.Contains(msg, "a user froze the server") {
		t.Fatalf("force release of a freeze by a user %q", msg)
	}
	if !m.State().Frozen {
		t.Fatal("a refused force release ended the freeze")
	}
	if err := m.Release(); err != nil || m.State().Frozen {
		t.Fatalf("release %v %+v", err, m.State())
	}
	m.FreezeBySystem("The disk of the data directory is full.")
	s = m.State()
	if !s.Frozen || !s.SystemInitiated || !strings.HasPrefix(s.Reason, "The disk of the data directory is full.") {
		t.Fatalf("frozen by the system %+v", s)
	}
	if msg := refusedWith(t, m.Release()); !strings.Contains(msg, "force release") {
		t.Fatalf("release of a freeze of the system %q", msg)
	}
	if err := m.ForceRelease(); err != nil || m.State().Frozen {
		t.Fatalf("force release %v %+v", err, m.State())
	}
}

// Ruling SB9: a change holds the mode shared; a freeze waits for the
// changes that run, a change that comes while the freeze waits waits too
// and is then refused, and a freeze whose changes outlast the wait answers
// 409 and leaves the server released, after which the waiting change runs.
func TestReadOnlyFreezeWaitsForTheChanges(t *testing.T) {
	m := NewReadOnlyMode(time.Now)
	end, _, ok := m.Begin(context.Background())
	if !ok {
		t.Fatal("a change of a released server is refused")
	}
	frozen := make(chan error, 1)
	go func() { frozen <- m.Freeze() }()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-frozen:
		t.Fatalf("the freeze did not wait: %v", err)
	default:
	}
	late := make(chan bool, 1)
	go func() {
		e, _, ok := m.Begin(context.Background())
		if ok {
			e()
		}
		late <- ok
	}()
	time.Sleep(100 * time.Millisecond)
	end()
	if err := <-frozen; err != nil || !m.State().Frozen {
		t.Fatalf("freeze %v %+v", err, m.State())
	}
	if <-late {
		t.Fatal("a change that came while the freeze waited ran")
	}
	if _, s, ok := m.Begin(context.Background()); ok || !s.Frozen {
		t.Fatal("a change of a frozen server passed")
	}
	if err := m.Release(); err != nil {
		t.Fatal(err)
	}

	m.SetFreezeWait(200 * time.Millisecond)
	end, _, _ = m.Begin(context.Background())
	waiting := make(chan bool, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		e, _, ok := m.Begin(context.Background())
		if ok {
			e()
		}
		waiting <- ok
	}()
	begun := time.Now()
	msg := refusedWith(t, m.Freeze())
	if took := time.Since(begun); took < 200*time.Millisecond || took > 2*time.Second {
		t.Fatalf("the freeze gave up after %s", took)
	}
	if !strings.Contains(msg, "the server stays released") || m.State().Frozen {
		t.Fatalf("%q %+v", msg, m.State())
	}
	if !<-waiting {
		t.Fatal("a change that waited for a freeze that gave up was refused")
	}
	end()
}
