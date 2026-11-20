package auth

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The user of an npm or user token of a directory user (review B1). A token
// carries no password, so the user and its groups are looked up under the
// service bind (Directory.Lookup). The answer is kept for CredentialTime
// against the view of its generation, bounded and emptied like the
// credential cache, and parallel uses of one user wait for one lookup. The
// one directory of the stored order that holds the name gives the user. A
// name that more than one directory holds is refused (review R2), a
// directory that does not answer leaves the token without an answer, and a
// name no directory holds is refused; a user that is gone from the
// directory, or that the user filter of its connection leaves out, as a
// disabled account usually is, is such a name, and so is an entry the
// directory marks disabled or locked (package ldapauth, review 3-40). A
// token is bound to the entry of its user (tokenBindings).

// tokenBindings are the directory entries the npm and user tokens of
// directory users are bound to (review 3-40), by public half: model.User.Entry
// of the user the token was issued for. The tokens table has no column for
// it, so the bindings live in memory: a token is bound at its issue, and a
// token without a binding, issued before release 0.2.0, by the command line
// or before the start of the process, at its first use whose directory names
// an entry. A bound token whose name a directory now gives to another entry
// is refused.
type tokenBindings struct {
	mu      sync.Mutex
	entries map[string]string
}

func newTokenBindings() *tokenBindings { return &tokenBindings{entries: map[string]string{}} }

// holds reports whether the token of public may sign in the user of entry,
// and binds a token without a binding to it. An empty entry, of a directory
// that names none, binds nothing.
func (b *tokenBindings) holds(public, entry string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	bound, ok := b.entries[public]
	if ok {
		return bound == entry
	}
	if entry != "" {
		b.entries[public] = entry
	}
	return true
}

func (b *tokenBindings) drop(public string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, public)
}

// lookup is the answer of the directories for a user id.
type lookup struct {
	who   principal
	known bool
	gen   int64
	until time.Time
}

// lookups holds the answers by user id. A full table drops the expired
// answers first, then the one that ends first.
type lookups struct {
	mu      sync.Mutex
	now     func() time.Time
	max     int
	entries map[string]lookup
}

func newLookups(now func() time.Time) *lookups {
	return &lookups{now: now, max: MaxCredentials, entries: map[string]lookup{}}
}

// get is the answer for id of the last CredentialTime against the view of
// generation gen.
func (l *lookups) get(id string, gen int64) (lookup, bool) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[id]
	if !ok {
		return lookup{}, false
	}
	if !now.Before(e.until) || e.gen != gen {
		delete(l.entries, id)
		return lookup{}, false
	}
	e.who.groups = slices.Clone(e.who.groups)
	return e, true
}

// put keeps an answer for CredentialTime; the principal of a known user
// ends with it (review R7).
func (l *lookups) put(id string, e lookup) lookup {
	now := l.now()
	e.until = now.Add(CredentialTime)
	e.who.until = e.until
	e.who.groups = slices.Clone(e.who.groups)
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, held := l.entries[id]; !held && len(l.entries) >= l.max {
		l.evict(now)
	}
	l.entries[id] = e
	e.who.groups = slices.Clone(e.who.groups)
	return e
}

func (l *lookups) evict(now time.Time) {
	first, firstUntil, found := "", time.Time{}, false
	for id, e := range l.entries {
		if !now.Before(e.until) {
			delete(l.entries, id)
			continue
		}
		if !found || e.until.Before(firstUntil) {
			first, firstUntil, found = id, e.until, true
		}
	}
	if len(l.entries) >= l.max && found {
		delete(l.entries, first)
	}
}

func (l *lookups) clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	clear(l.entries)
}

func (l *lookups) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// LookupWarningEvery is the least time between two warnings of the lookup
// for one connection (review R10): a missing answer is not kept, so while a
// directory does not answer every use of a token asks it again.
const LookupWarningEvery = 60 * time.Second

// warnings lets one warning per connection through in LookupWarningEvery,
// on the monotonic clock of the service. A connection is its directory, or
// its place in the order for a directory that cannot be a map key.
type warnings struct {
	mu   sync.Mutex
	last map[any]time.Duration
}

func newWarnings() *warnings { return &warnings{last: map[any]time.Duration{}} }

func (w *warnings) allow(i int, d Directory, now time.Duration) bool {
	var key any = i
	if reflect.ValueOf(d).Comparable() {
		key = d
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if last, seen := w.last[key]; seen && now-last < LookupWarningEvery {
		return false
	}
	for k, last := range w.last {
		if now-last >= LookupWarningEvery {
			delete(w.last, k)
		}
	}
	w.last[key] = now
	return true
}

// lookupAnswer is a lookup and whether the directories gave one.
type lookupAnswer struct {
	lookup
	answered bool
}

// directoryUser is the principal of a directory user by id, with the groups
// of the answer of its directory. The lookup answers every request that
// waits for it, so it runs under a context of its own that the request that
// started it does not end (review R4); the timeout of each connection, on
// its dial and on every operation, bounds it.
func (s *Service) directoryUser(ctx context.Context, v *view, id string) (principal, bool) {
	if !v.ldapOn || len(v.dirs) == 0 {
		return principal{}, false
	}
	if e, ok := s.lookups.get(id, v.gen); ok {
		return e.who, e.known
	}
	got, _, err := s.lookupFlight.do(ctx, strconv.FormatInt(v.gen, 10)+"\x00"+id, func() lookupAnswer {
		if e, ok := s.lookups.get(id, v.gen); ok {
			return lookupAnswer{lookup: e, answered: true}
		}
		got := s.lookUp(context.WithoutCancel(ctx), v, id)
		if got.answered {
			got.lookup = s.lookups.put(id, got.lookup)
		}
		return got
	})
	if err != nil || !got.answered || !got.known {
		return principal{}, false
	}
	who := got.who
	who.groups = slices.Clone(who.groups)
	return who, true
}

// lookUp asks every directory of the view for id, in their order (review
// R2). The row of a token keeps the user id and the source only, so exactly
// one directory may hold the name: while a second one holds it the token is
// refused, and a directory that does not answer, wherever it stands, leaves
// the token without an answer, since it may hold the name as well.
func (s *Service) lookUp(ctx context.Context, v *view, id string) lookupAnswer {
	refused := lookupAnswer{lookup: lookup{gen: v.gen}, answered: true}
	found, holders := refused, []string{}
	for i, d := range v.dirs {
		u, groups, err := d.Lookup(ctx, id)
		switch {
		case err == nil && u.ID != "":
			holders = append(holders, strconv.Itoa(i+1))
			who := principal{user: model.User{ID: id, FirstName: u.FirstName, LastName: u.LastName, Email: u.Email,
				Status: "active", Source: SourceLDAP, Entry: u.Entry}, realm: RealmLDAP, groups: slices.Clone(groups)}
			if len(holders) == 1 {
				found = lookupAnswer{lookup: lookup{who: who, known: true, gen: v.gen}, answered: true}
			}
		case err == nil:
			s.log.Warn("directory answered a lookup without a user id", "connection", i+1, "of", len(v.dirs))
		case errors.Is(err, ErrNoUser):
		case errors.Is(err, ErrUnreachable):
			if s.lookupWarnings.allow(i, d, s.elapsed()) {
				s.log.Warn("directory did not answer the lookup of the user of a token", "connection", i+1, "of", len(v.dirs))
			}
			return lookupAnswer{}
		default:
			if s.lookupWarnings.allow(i, d, s.elapsed()) {
				s.log.Warn("directory lookup of the user of a token failed", "connection", i+1, "of", len(v.dirs))
			}
			return lookupAnswer{}
		}
	}
	if len(holders) > 1 {
		s.log.Warn("more than one directory holds the user of a token, so the token is refused", "user", id,
			"connections", strings.Join(holders, ","), "of", len(v.dirs))
		return refused
	}
	return found
}
