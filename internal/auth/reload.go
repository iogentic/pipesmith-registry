package auth

import (
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// ReloadInterval is the least time between two reads of the generation of
// the store. A change that another process writes into state.sqlite reaches
// the service at the first request after it, within about this time,
// without a restart.
const ReloadInterval = time.Second

// Store is what the service reads and writes of state.sqlite; package state
// implements it. Every identity write raises the generation; the last use of
// a token is none.
type Store interface {
	Generation() (int64, error)
	Load() (*model.Snapshot, error)
	// SetPasswordHash replaces the stored form of a local user while the row
	// still holds old; false when it holds another form.
	SetPasswordHash(userID, old, hash string) (bool, error)
	// AddToken writes a token row and revokes the oldest of the same user,
	// source and kind beyond keep.
	AddToken(t model.Token, keep int) ([]string, error)
	// Token is the row of a public half; false when there is none.
	Token(public string) (model.Token, bool, error)
	// Tokens are the rows of a user of a source in the order of their issue.
	Tokens(userID, source string) ([]model.Token, error)
	// RemoveToken deletes the row of a public half; false when there was none.
	RemoveToken(public string) (bool, error)
	// TouchToken writes the last use of a row.
	TouchToken(public string, at time.Time) error
}

// refresh returns the view a request checks against. At most once per
// ReloadInterval it reads the generation of the store, and when another
// write raised it the snapshot is loaded again first; a request that comes
// meanwhile waits for that load. The interval runs on the monotonic clock
// (review N3): a step of the wall clock neither pauses nor hurries it.
func (s *Service) refresh() *view {
	if s.store == nil {
		return s.view()
	}
	now := int64(s.elapsed())
	if now < s.next.Load() {
		return s.view()
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if now < s.next.Load() {
		return s.view()
	}
	s.next.Store(now + int64(ReloadInterval))
	s.reload()
	return s.view()
}

// reload installs the view of a snapshot loaded again when the generation
// changed. It empties the credential cache and the answers of the
// directories for the users of tokens, and checks every live session
// against the new view: one whose user is gone or no longer active, or whose
// realm left the stored order, ends; the others take the rights of the new
// view. The directories come from DirectoriesFor at each change, since a
// bind password can change outside state.sqlite; the builder keeps the
// directory of a connection that did not change. A store that does not
// answer leaves the last view in place.
func (s *Service) reload() {
	old := s.view()
	gen, err := s.store.Generation()
	if err == nil && gen == old.gen {
		return
	}
	var snap *model.Snapshot
	if err == nil {
		snap, err = s.store.Load()
	}
	if err != nil {
		s.log.Warn("the identity state was not loaded again; the last one stays", "error", err.Error())
		return
	}
	dirs := old.dirs
	if s.dirsFor != nil {
		dirs = s.dirsFor(snap.LDAP)
	}
	v := newView(snap, gen, dirs)
	s.cur.Store(v)
	s.creds.clear()
	s.lookups.clear()
	s.sessions.recheckAll(v.gen, v.subject)
	s.log.Info("identity state loaded again", "generation", gen)
	v.report(s.log)
}
