package auth

import (
	"fmt"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// HashRuns is the number of digests VerifyPassword computed so far.
func HashRuns() int64 { return hashRuns.Load() }

// SessionCount is the number of sessions in the store.
func SessionCount(s *Service) int { return s.sessions.len() }

// PairCount is the number of pairs the limiter holds.
func PairCount(s *Service) int { return s.limiter.len() }

// CacheAsks is the number of credentials compared with the cache of Basic
// successes so far.
func CacheAsks(s *Service) int64 { return s.creds.asks.Load() }

// PlaceWaiters is the number of Basic requests that wait for a place of
// their pair now.
func PlaceWaiters(s *Service) int64 { return s.limiter.waiting.Load() }

// SamePair reports whether two user names from one address are one pair of
// the limiter.
func SamePair(a, b string) bool { return pairKey(a, "192.0.2.1") == pairKey(b, "192.0.2.1") }

// SetMaxPairs replaces the bound of the limiter table.
func SetMaxPairs(s *Service, n int) {
	s.limiter.mu.Lock()
	defer s.limiter.mu.Unlock()
	s.limiter.max = n
}

// SessionLimits gives the bounds the store uses.
func SessionLimits(s *Service) (perUser, total int) { return s.sessions.perUser, s.sessions.total }

// SetSessionLimits replaces the bounds of the store.
func SetSessionLimits(s *Service, perUser, total int) {
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	s.sessions.perUser, s.sessions.total = perUser, total
}

// SessionsPerUser counts the live sessions of each user id.
func SessionsPerUser(s *Service) map[string]int {
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	out := map[string]int{}
	for _, live := range s.sessions.byKey {
		out[live.subject.User.ID]++
	}
	return out
}

// FillSessions starts n sessions of n invented users without a sign in.
func FillSessions(s *Service, n int) error {
	for i := range n {
		u := model.User{ID: fmt.Sprintf("fill-%d", i), Source: "default"}
		got := signedIn{principal: principal{user: u, realm: RealmLocal}, subject: model.Subject{User: u, Realm: RealmLocal}}
		if _, err := s.sessions.create(got, s.view().gen); err != nil {
			return err
		}
	}
	return nil
}

// CredentialCount is the number of remembered Basic credentials.
func CredentialCount(s *Service) int { return s.creds.len() }

// CredentialLimit is the bound of the credential cache.
func CredentialLimit(s *Service) int {
	s.creds.mu.Lock()
	defer s.creds.mu.Unlock()
	return s.creds.max
}

// SetMaxCredentials replaces the bound of the credential cache.
func SetMaxCredentials(s *Service, n int) {
	s.creds.mu.Lock()
	defer s.creds.mu.Unlock()
	s.creds.max = n
}

// RememberCredential stores a successful check of user and password in a
// realm, as a Basic request does.
func RememberCredential(s *Service, realm, user, pass string) {
	u := model.User{ID: user, Source: "default"}
	got := signedIn{principal: principal{user: u, realm: realm}, subject: model.Subject{User: u, Realm: realm}}
	s.creds.put(realm, user, pass, got, s.view().gen)
}

// CredentialHeld reports whether the cache answers for the realm, user and
// password.
func CredentialHeld(s *Service, realm, user, pass string) bool {
	_, ok := s.creds.get(realm, user, pass, s.view().gen)
	return ok
}

// DockerTokenCount is the number of Docker bearer tokens in memory.
func DockerTokenCount(s *Service) int { return s.docker.len() }

// DockerTokenLimit is the bound of the Docker bearer tokens.
func DockerTokenLimit(s *Service) int {
	s.docker.mu.Lock()
	defer s.docker.mu.Unlock()
	return s.docker.max
}

// SetMaxDockerTokens replaces the bound of the Docker bearer tokens.
func SetMaxDockerTokens(s *Service, n int) {
	s.docker.mu.Lock()
	defer s.docker.mu.Unlock()
	s.docker.max = n
}

// SetIntervalClock replaces the clock of the reload interval, the time since
// the start of the service.
func SetIntervalClock(s *Service, elapsed func() time.Duration) { s.elapsed = elapsed }

// SetFloorClock replaces the real clock and the sleep of the refusal floor.
func SetFloorClock(s *Service, now func() time.Time, sleep func(time.Duration)) {
	s.floorNow, s.floorSleep = now, sleep
}

// FillPairs counts one failure for n invented pairs, each of an address of
// its own, so no address budget stops them.
func FillPairs(s *Service, n int) {
	for i := range n {
		user, address := fmt.Sprintf("fill-%d", i), fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255)
		if s.limiter.begin(user, address) == 0 {
			s.limiter.end(user, address, false)
		}
	}
}

// BeginPair reserves an attempt of a pair as a check in flight does.
func BeginPair(s *Service, user, address string) int { return s.limiter.begin(user, address) }

// ReleasePair settles a reservation of BeginPair without a result.
func ReleasePair(s *Service, user, address string) { s.limiter.release(user, address) }

// FailPair counts n failures for the pair of a user name and an address.
func FailPair(s *Service, user, address string, n int) {
	for range n {
		if s.limiter.begin(user, address) == 0 {
			s.limiter.end(user, address, false)
		}
	}
}

// PairFailures is the number of failures the limiter holds for the pair of a
// user name and an address, and whether it holds the pair at all.
func PairFailures(s *Service, user, address string) (int, bool) {
	s.limiter.mu.Lock()
	defer s.limiter.mu.Unlock()
	a, held := s.limiter.entries[pairKey(user, address)]
	if !held {
		return 0, false
	}
	return a.failures, true
}

// PairsInFlight is the number of pairs with an attempt in flight.
func PairsInFlight(s *Service) int {
	s.limiter.mu.Lock()
	defer s.limiter.mu.Unlock()
	n := 0
	for _, a := range s.limiter.entries {
		if a.pending > 0 {
			n++
		}
	}
	return n
}

// CheckWaiters is the number of Basic requests that wait for the check of
// another request with the same user name and password now.
func CheckWaiters(s *Service) int64 { return s.checks.waiting.Load() }

// ForwardedPeers is the number of peers the warner remembers.
func ForwardedPeers(w *ForwardedWarner) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.seen)
}
