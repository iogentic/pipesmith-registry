package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Bounds of the sessions (decisions T3 and N7).
const (
	IdleTime           = 30 * time.Minute
	MaxSessionAge      = 12 * time.Hour
	MaxSessionsPerUser = 20
	MaxSessions        = 10_000
)

// session is one interface session. Only the SHA-256 of its id is a key of
// the store, so a lookup does not compare ids byte by byte. The subject
// holds the rights of the view of generation gen; principal is what a later
// view checks again.
type session struct {
	key       [32]byte
	id        string
	csrf      string
	subject   model.Subject
	principal principal
	gen       int64
	created   time.Time
	seen      time.Time
	gone      bool
}

// sessionStore holds the sessions in memory. order and byUser list the
// sessions in the order they began and may still hold ended ones; they are
// rebuilt when they grow past twice the live sessions.
type sessionStore struct {
	mu      sync.Mutex
	byKey   map[[32]byte]*session
	order   []*session
	byUser  map[string][]*session
	now     func() time.Time
	perUser int
	total   int
}

func newSessionStore(now func() time.Time) *sessionStore {
	return &sessionStore{byKey: map[[32]byte]*session{}, byUser: map[string][]*session{}, now: now,
		perUser: MaxSessionsPerUser, total: MaxSessions}
}

// randomText is 32 bytes of crypto/rand in URL safe Base64 without padding.
func randomText() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func userKey(u model.User) string { return u.Source + "\x00" + u.ID }

// create starts a session for a successful check against the view of
// generation gen, with a new id and request token. The oldest session of the
// user ends when the user holds the bound, the oldest of all when the store
// does, after the expired ones. It returns a copy of the new session.
func (st *sessionStore) create(got signedIn, gen int64) (session, error) {
	id, err := randomText()
	if err != nil {
		return session{}, err
	}
	csrf, err := randomText()
	if err != nil {
		return session{}, err
	}
	got = got.clone()
	subject := got.subject
	subject.Session = id
	subject.User.PasswordHash = ""
	now := st.now()
	s := &session{key: sha256.Sum256([]byte(id)), id: id, csrf: csrf, subject: subject, principal: got.principal, gen: gen,
		created: now, seen: now}
	owner := userKey(subject.User)

	st.mu.Lock()
	defer st.mu.Unlock()
	own := st.byUser[owner][:0]
	for _, other := range st.byUser[owner] {
		if !other.gone && st.expired(other, now) {
			st.drop(other)
		}
		if !other.gone {
			own = append(own, other)
		}
	}
	for len(own) >= st.perUser {
		st.drop(own[0])
		own = own[1:]
	}
	st.byUser[owner] = own
	if len(st.byKey) >= st.total {
		for _, other := range st.byKey {
			if st.expired(other, now) {
				st.drop(other)
			}
		}
	}
	for _, other := range st.order {
		if len(st.byKey) < st.total {
			break
		}
		if !other.gone {
			st.drop(other)
		}
	}
	st.byKey[s.key] = s
	st.order = append(st.order, s)
	st.byUser[owner] = append(st.byUser[owner], s)
	if len(st.order) > 2*len(st.byKey)+64 {
		st.compact()
	}
	// a copy: a reload may give the stored session the rights of a new view meanwhile
	return *s, nil
}

func (st *sessionStore) drop(s *session) {
	s.gone = true
	delete(st.byKey, s.key)
}

// compact drops the ended sessions from order and byUser.
func (st *sessionStore) compact() {
	live := st.order[:0]
	st.byUser = map[string][]*session{}
	for _, s := range st.order {
		if !s.gone {
			live = append(live, s)
			owner := userKey(s.subject.User)
			st.byUser[owner] = append(st.byUser[owner], s)
		}
	}
	clear(st.order[len(live):])
	st.order = live
}

func (st *sessionStore) expired(s *session, now time.Time) bool {
	return now.Sub(s.seen) >= IdleTime || now.Sub(s.created) >= MaxSessionAge
}

// get returns a copy of the live session of an id and marks it used now. A
// session past its idle time or its age ends.
func (st *sessionStore) get(id string) (session, bool) {
	if id == "" {
		return session{}, false
	}
	key := sha256.Sum256([]byte(id))
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.byKey[key]
	if !ok {
		return session{}, false
	}
	if st.expired(s, now) {
		st.drop(s)
		return session{}, false
	}
	s.seen = now
	return *s, true
}

// live is get for a request that checks against the view of generation gen:
// a session made against another view is checked again with recheck first.
// It takes the new subject when recheck accepts its principal, and ends
// otherwise.
func (st *sessionStore) live(id string, gen int64, recheck func(principal) (model.Subject, bool)) (session, bool) {
	if id == "" {
		return session{}, false
	}
	key := sha256.Sum256([]byte(id))
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.byKey[key]
	if !ok {
		return session{}, false
	}
	if st.expired(s, now) || !st.current(s, gen, recheck) {
		st.drop(s)
		return session{}, false
	}
	s.seen = now
	return *s, true
}

// current brings a session to the view of generation gen; false means its
// principal is no longer valid there. The caller holds the lock.
func (st *sessionStore) current(s *session, gen int64, recheck func(principal) (model.Subject, bool)) bool {
	if s.gen == gen {
		return true
	}
	subject, ok := recheck(s.principal)
	if !ok {
		return false
	}
	subject.Session = s.id
	subject.User.PasswordHash = ""
	s.subject, s.gen = subject, gen
	return true
}

// recheckAll brings every live session to the view of generation gen and
// ends those whose principal is no longer valid there.
func (st *sessionStore) recheckAll(gen int64, recheck func(principal) (model.Subject, bool)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, s := range st.byKey {
		if !st.current(s, gen, recheck) {
			st.drop(s)
		}
	}
}

// remove ends the session of an id.
func (st *sessionStore) remove(id string) {
	key := sha256.Sum256([]byte(id))
	st.mu.Lock()
	defer st.mu.Unlock()
	if s, ok := st.byKey[key]; ok {
		st.drop(s)
	}
}

func (st *sessionStore) len() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.byKey)
}
