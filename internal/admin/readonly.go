package admin

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// The read only mode of a running server (ruling SB2 of leg 6-system). It
// lives in memory and starts released: a restart releases a freeze. A
// freeze by a user ends with release; a freeze of the system ends with
// force release, which the contract offers only while systemInitiated is
// true. The server refuses the changes while the mode is frozen.

// ReadOnlyState is the state of the read only mode as GET read-only shows
// it; Reason is its summaryReason.
type ReadOnlyState struct {
	Frozen, SystemInitiated bool
	Reason                  string
}

// ReadOnlyMode is the read only mode of a running server. Its methods are
// safe for use by several goroutines.
type ReadOnlyMode struct {
	now    func() time.Time
	mu     sync.Mutex
	frozen bool
	system bool
	reason string
	number uint64 // counts the freezes

	active   int           // the changes that run (Begin)
	freezing bool          // a freeze waits for the changes
	changed  chan struct{} // closed at every change of active and freezing
	wait     time.Duration // how long a freeze waits
}

// Frozen reports whether the mode is frozen, and the number of the freeze,
// which a new freeze raises.
func (m *ReadOnlyMode) Frozen() (bool, uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.frozen, m.number
}

const inMemory = " The freeze lives in memory only; a restart of the server releases it."

// Freezing reports whether the mode is frozen or a freeze waits for the
// changes that run, so that a long change can stop and let it pass
// (ruling KN11 of leg 6-kinds).
func (m *ReadOnlyMode) Freezing() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.freezing || m.frozen
}

// NewReadOnlyMode is a released mode whose freezes take their time from now.
func NewReadOnlyMode(now func() time.Time) *ReadOnlyMode {
	return &ReadOnlyMode{now: now, changed: make(chan struct{}), wait: FreezeWait}
}

// State is the state of the mode.
func (m *ReadOnlyMode) State() ReadOnlyState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.frozen {
		return ReadOnlyState{Reason: "The server takes changes." + inMemory}
	}
	return ReadOnlyState{Frozen: true, SystemInitiated: m.system, Reason: m.reason + inMemory}
}

func readOnlyRefusal(message string) error {
	return &Refusal{Code: CodeReadOnlyState, Message: message}
}

// Freeze is the freeze of a user, a point in time (ruling SB9): it waits
// for the changes that run, while new ones wait, for FreezeWait at most.
// Changes that outlast it are refused with 409 and the mode stays
// released. A frozen mode, or one a freeze waits for already, refuses it
// with 409.
func (m *ReadOnlyMode) Freeze() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.frozen:
		return readOnlyRefusal("the server is frozen already")
	case m.freezing:
		return readOnlyRefusal("a freeze waits for the running changes already")
	}
	m.freezing = true
	timer := time.NewTimer(m.wait)
	defer timer.Stop()
	for m.active > 0 {
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-changed:
			m.mu.Lock()
		case <-timer.C:
			m.mu.Lock()
			if m.active > 0 {
				m.freezing = false
				m.signal()
				return readOnlyRefusal(fmt.Sprintf("%d changes still ran after %s, so the server stays released; freeze it again later",
					m.active, m.wait))
			}
		}
	}
	m.freezing = false
	m.signal()
	m.frozen, m.system = true, false
	m.number++
	m.reason = "A user froze the server at " + m.now().UTC().Format(time.RFC3339) + "; it refuses changes until a release."
	return nil
}

// FreezeBySystem freezes the mode for the server itself, with the reason,
// also over a freeze of a user.
func (m *ReadOnlyMode) FreezeBySystem(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.frozen {
		m.number++
	}
	m.frozen, m.system, m.reason = true, true, reason
}

// Release ends a freeze of a user. It refuses with 409 a mode that is not
// frozen and a freeze of the system.
func (m *ReadOnlyMode) Release() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case !m.frozen:
		return readOnlyRefusal("the server is not frozen")
	case m.system:
		return readOnlyRefusal("the server froze itself; only a force release ends that freeze")
	}
	m.frozen, m.reason = false, ""
	return nil
}

// ForceRelease ends a freeze of the system. It refuses with 409 a mode that
// is not frozen and a freeze of a user, which release ends.
func (m *ReadOnlyMode) ForceRelease() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case !m.frozen:
		return readOnlyRefusal("the server is not frozen")
	case !m.system:
		return readOnlyRefusal("a user froze the server; a release ends that freeze, a force release only one the server set itself")
	}
	m.frozen, m.system, m.reason = false, false, ""
	return nil
}

// FreezeWait is how long a freeze waits for the changes that run (ruling
// SB9).
const FreezeWait = 30 * time.Second

// SetFreezeWait sets how long a freeze waits for the changes that run;
// tests shorten it.
func (m *ReadOnlyMode) SetFreezeWait(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wait = d
}

// signal wakes everyone who waits for a change of the mode; m.mu is held.
func (m *ReadOnlyMode) signal() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// Begin starts a change that holds the mode shared until end is called
// (ruling SB9). While a freeze waits, Begin waits for its outcome. ok is
// false when the mode is frozen or ctx ends first; state is then the state
// to answer with.
func (m *ReadOnlyMode) Begin(ctx context.Context) (end func(), state ReadOnlyState, ok bool) {
	m.mu.Lock()
	for m.freezing {
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, m.State(), false
		}
		m.mu.Lock()
	}
	if m.frozen {
		m.mu.Unlock()
		return nil, m.State(), false
	}
	m.active++
	m.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.active--
			m.signal()
		})
	}, ReadOnlyState{}, true
}
