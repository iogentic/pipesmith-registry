package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Bounds of the credential cache (decision N8 of the phase 3 plan, hardening
// line 9).
const (
	CredentialTime = 300 * time.Second
	MaxCredentials = 10_000
)

// credentialCache remembers the successful checks of Basic credentials
// (decision N8), so a build that sends Basic with every request costs one
// hash or one LDAP bind per CredentialTime. The key is an HMAC-SHA-256 of
// realm, user name and password under 32 random bytes of the process, so the
// cache holds no password and no hash that outlives the process. An entry
// ends CredentialTime after its check; a hit does not extend it. A full
// cache drops the expired entries first, then the entry that ends first.
type credentialCache struct {
	mu      sync.Mutex
	secret  []byte
	now     func() time.Time
	max     int
	entries map[[32]byte]credential
	// names holds the keys of the entries of each realm and user name, so
	// find tells a password that missed a remembered check of its name from
	// one whose name has none (finding T1 of review 3)
	names map[[32]byte]map[[32]byte]struct{}
	// asks counts the credentials find compared with the cache. Tests read
	// it to see how many guesses of a burst were compared.
	asks atomic.Int64
}

// credential is a remembered check against the view of generation gen; a
// check against another view does not take it. name is the key of its realm
// and user name in names.
type credential struct {
	got   signedIn
	gen   int64
	until time.Time
	name  [32]byte
}

func newCredentialCache(now func() time.Time) (*credentialCache, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	return &credentialCache{secret: secret, now: now, max: MaxCredentials, entries: map[[32]byte]credential{},
		names: map[[32]byte]map[[32]byte]struct{}{}}, nil
}

// key is the HMAC of realm, user name and password. The realm and the user
// name carry their length, so no two triples give the same input.
func (c *credentialCache) key(realm, user, password string) [32]byte {
	mac := c.fields(realm, user)
	mac.Write([]byte(password))
	return sum(mac)
}

// nameKey is the HMAC of realm and user name, the key of names. It lives in
// a map of its own, apart from the keys of the entries.
func (c *credentialCache) nameKey(realm, user string) [32]byte {
	return sum(c.fields(realm, user))
}

// fields is the HMAC under the secret of the process with the realm and the
// user name written, each after its length.
func (c *credentialCache) fields(realm, user string) hash.Hash {
	mac := hmac.New(sha256.New, c.secret)
	var size [8]byte
	for _, field := range []string{realm, user} {
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		mac.Write(size[:])
		mac.Write([]byte(field))
	}
	return mac
}

func sum(mac hash.Hash) [32]byte {
	var key [32]byte
	copy(key[:], mac.Sum(nil))
	return key
}

// flightKey names the checks of one user name and password against the
// view of generation gen, for the checks in flight (Service.basic).
func (c *credentialCache) flightKey(gen int64, user, password string) string {
	key := c.key(strconv.FormatInt(gen, 10), user, password)
	return string(key[:])
}

// copySubject is a subject that shares no slice with s.
func copySubject(s model.Subject) model.Subject {
	s.Permissions = slices.Clone(s.Permissions)
	s.User.Roles = slices.Clone(s.User.Roles)
	return s
}

// find compares a user name and password with the cache: a check of the
// last CredentialTime against the view of generation gen in one of the
// realms, asked in their order. Each call counts one comparison (asks).
// held reports that the cache holds such a check of the user name in one of
// the realms, the password of the call or another: a password that missed
// was compared with it (finding T1 of review 3). One read of the clock under
// one lock decides both, so no entry ends between the two answers.
func (c *credentialCache) find(realms []string, user, password string, gen int64) (got signedIn, ok, held bool) {
	c.asks.Add(1)
	keys, names := make([][32]byte, len(realms)), make([][32]byte, len(realms))
	for i, realm := range realms {
		keys[i], names[i] = c.key(realm, user, password), c.nameKey(realm, user)
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range keys {
		e, found := c.entries[key]
		switch {
		case !found:
		case now.Before(e.until) && e.gen == gen:
			return e.got.clone(), true, true
		default:
			c.drop(key, e)
		}
	}
	for _, name := range names {
		for key := range c.names[name] {
			if e := c.entries[key]; now.Before(e.until) && e.gen == gen {
				return signedIn{}, false, true
			}
		}
	}
	return signedIn{}, false, false
}

// get answers a check of the last CredentialTime against the view of
// generation gen with a copy of it.
func (c *credentialCache) get(realm, user, password string, gen int64) (signedIn, bool) {
	key := c.key(realm, user, password)
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return signedIn{}, false
	}
	if !now.Before(e.until) || e.gen != gen {
		c.drop(key, e)
		return signedIn{}, false
	}
	return e.got.clone(), true
}

// put remembers a successful check against the view of generation gen for
// CredentialTime, and no longer than the answer of the directory that let a
// directory user in (review R7).
func (c *credentialCache) put(realm, user, password string, got signedIn, gen int64) {
	key, name := c.key(realm, user, password), c.nameKey(realm, user)
	now := c.now()
	until := now.Add(CredentialTime)
	if end := got.principal.until; !end.IsZero() && end.Before(until) {
		until = end
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, held := c.entries[key]; !held && len(c.entries) >= c.max {
		c.evict(now)
	}
	c.entries[key] = credential{got: got.clone(), gen: gen, until: until, name: name}
	keys := c.names[name]
	if keys == nil {
		keys = map[[32]byte]struct{}{}
		c.names[name] = keys
	}
	keys[key] = struct{}{}
}

// drop removes the entry of key and its key from names. The caller holds mu.
func (c *credentialCache) drop(key [32]byte, e credential) {
	delete(c.entries, key)
	if keys := c.names[e.name]; keys != nil {
		delete(keys, key)
		if len(keys) == 0 {
			delete(c.names, e.name)
		}
	}
}

// evict makes room for one entry: it drops the expired entries, and when
// that is not enough the entry that ends first, which is the oldest.
func (c *credentialCache) evict(now time.Time) {
	var first [32]byte
	var firstUntil time.Time
	found := false
	for key, e := range c.entries {
		if !now.Before(e.until) {
			c.drop(key, e)
			continue
		}
		if !found || e.until.Before(firstUntil) {
			first, firstUntil, found = key, e.until, true
		}
	}
	if len(c.entries) >= c.max && found {
		c.drop(first, c.entries[first])
	}
}

func (c *credentialCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
	clear(c.names)
}

func (c *credentialCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// ForgetCredentials empties the cache of Basic credentials and the answers
// of the directories for the users of tokens, so the next request of every
// client is checked again. A change of a user, of its roles or of the realm
// order calls it (decision N8): a reload of the snapshot does (reload.go).
func (s *Service) ForgetCredentials() {
	s.creds.clear()
	s.lookups.clear()
}
