// Package auth implements model.Security: the password forms of the old
// product, interface sessions in memory with a request token, sign in limits
// per user and client address, Basic credentials of local and directory
// users behind a credential cache, sign in through LDAP directories behind
// the Directory interface, the realms in the stored order and the tokens
// this server issues (tokens.go). Rights come from package authz. With a
// Store the service follows state.sqlite without a restart (reload.go).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/authz"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Names of the session cookie, the request token header, the realms and the
// user sources.
const (
	CookieName  = "pipesmith_session"
	CSRFHeader  = "X-Pipesmith-Csrf"
	RealmLocal  = "NexusAuthenticatingRealm"
	RealmLDAP   = "LdapRealm"
	SourceLocal = "default"
	SourceLDAP  = "LDAP"
)

// Options configure a Service.
type Options struct {
	// Snapshot is the identity state; required.
	Snapshot *model.Snapshot
	// Generation is the generation of Store at the load of Snapshot, read
	// before that load.
	Generation int64
	// Store is state.sqlite: the service reads its generation at most once
	// per ReloadInterval and loads the snapshot again when it changed. Nil
	// keeps Snapshot for the life of the service.
	Store Store
	// Directories are the LDAP connections in their order. They sign in
	// through POST /api/session and Basic credentials (decision E10 of the
	// execution plan, in place of T7 for Basic).
	Directories []Directory
	// DirectoriesFor gives the LDAP connections of each snapshot loaded
	// again; it keeps the directory of a connection whose row and bind
	// password did not change. Nil keeps Directories.
	DirectoriesFor func(servers []model.LDAPServer) []Directory
	// Logger receives warnings at start and when a directory does not
	// answer; nil writes none. No line holds a password, a hash, a session
	// id or a request token.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// TrustedProxies are the reverse proxies whose X-Forwarded-For and
	// X-Forwarded-Proto count for the sign in limits and the session cookie;
	// nil trusts none.
	TrustedProxies TrustedProxies
}

// Service implements model.Security. It is safe for concurrent use.
type Service struct {
	cur      atomic.Pointer[view]
	store    Store
	dirsFor  func(servers []model.LDAPServer) []Directory
	sessions *sessionStore
	limiter  *limiter
	creds    *credentialCache
	docker   *dockerTokens
	proxies  TrustedProxies
	log      *slog.Logger
	now      func() time.Time
	dummy    string // a Shiro 1 hash nobody knows, checked for an unknown user
	// the real clock and the sleep of the refusal floor
	floorNow   func() time.Time
	floorSleep func(time.Duration)
	// the next read of the generation, in nanoseconds of elapsed; reloadMu
	// lets one request at a time read it and load the snapshot
	next     atomic.Int64
	reloadMu sync.Mutex
	// elapsed is the time since the start of the service on the monotonic
	// clock of now (review N3)
	elapsed func() time.Duration

	// the answers of the directories for the users of tokens and the
	// warnings of their lookups (lookup.go)
	lookups        *lookups
	lookupFlight   *flight[lookupAnswer]
	lookupWarnings *warnings
	// the checks of Basic credentials in flight (basic)
	checks *flight[basicAnswer]
}

var _ model.Security = (*Service)(nil)

// New builds the service for one snapshot.
func New(o Options) (*Service, error) {
	if o.Snapshot == nil {
		return nil, errors.New("auth: no snapshot")
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	salt, digest := make([]byte, 16), make([]byte, 64)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(digest); err != nil {
		return nil, err
	}
	creds, err := newCredentialCache(now)
	if err != nil {
		return nil, err
	}
	docker, err := newDockerTokens(now)
	if err != nil {
		return nil, err
	}
	s := &Service{
		store:    o.Store,
		dirsFor:  o.DirectoriesFor,
		sessions: newSessionStore(now),
		limiter:  newLimiter(now),
		creds:    creds,
		docker:   docker,
		proxies:  slices.Clone(o.TrustedProxies),
		log:      log,
		now:      now,
		dummy: "$shiro1$SHA-512$1024$" + base64.StdEncoding.EncodeToString(salt) + "$" +
			base64.StdEncoding.EncodeToString(digest),
		floorNow:   time.Now,
		floorSleep: time.Sleep,
	}
	s.lookups, s.lookupFlight, s.lookupWarnings = newLookups(now), newFlight[lookupAnswer](), newWarnings()
	start := now()
	s.elapsed = func() time.Duration { return now().Sub(start) }
	s.checks = newFlight[basicAnswer]()
	v := newView(o.Snapshot, o.Generation, slices.Clone(o.Directories))
	s.cur.Store(v)
	s.next.Store(int64(ReloadInterval))
	v.report(s.log)
	return s, nil
}

// view is the identity state the service checks against now.
func (s *Service) view() *view { return s.cur.Load() }

// Subject resolves the caller: a live session cookie, else Basic credentials
// of a local or a directory user, else a bearer token (tokens.go). Without
// credentials it returns nil and no error. A cookie that names no live
// session, without credentials next to it, gives ErrUnauthorized, so the
// interface learns that its session ended.
func (s *Service) Subject(r *http.Request) (*model.Subject, error) {
	v := s.refresh()
	cookie, cookieErr := r.Cookie(CookieName)
	if cookieErr == nil {
		if live, ok := s.sessions.live(cookie.Value, v.gen, v.subject); ok {
			subject := copySubject(live.subject)
			return &subject, nil
		}
	}
	header := r.Header.Get("Authorization")
	if header == "" {
		if cookieErr == nil {
			return nil, model.ErrUnauthorized
		}
		return nil, nil
	}
	scheme, credentials, _ := strings.Cut(header, " ")
	if strings.EqualFold(scheme, "Bearer") {
		return s.bearer(r.Context(), v, strings.TrimSpace(credentials))
	}
	if !strings.EqualFold(scheme, "Basic") {
		return nil, model.ErrUnauthorized
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(credentials))
	if err != nil {
		return nil, model.ErrUnauthorized
	}
	user, pass, found := strings.Cut(string(decoded), ":")
	if !found {
		return nil, model.ErrUnauthorized
	}
	got, ok, wait := s.basic(r, v, user, pass)
	if wait > 0 {
		return nil, throttled(wait)
	}
	if !ok {
		return nil, model.ErrUnauthorized
	}
	return &got.subject, nil
}

// basicAnswer is the result of a check of Basic credentials under the
// limiter.
type basicAnswer struct {
	got  signedIn
	ok   bool
	wait int
}

// basic checks the Basic credentials of r (decisions N8 and E10, reviews S2
// and R1, finding T1 of review 3). Every request holds a reservation of its
// pair in the limiter before anything is compared, so a burst of guesses is
// compared at most FreeFailures times; a request that finds the attempts in
// flight of its pair at their cap waits for a place up to PlaceWait, and a
// pair that waits after its failures is refused at once, whatever password
// it sends. Parallel requests of one user name and password share one
// comparison: the first asks the cache for a success of the last
// CredentialTime against the same view and runs the check when there is
// none, the others take only its success or its answer that a directory had
// no place. After a refusal each of them runs a check of its own under its
// reservation. settle gives the reservation back by its rule; from the
// question to the cache on, a way out that sets no result, a panic
// included, counts as a failure.
func (s *Service) basic(r *http.Request, v *view, typed, pass string) (signedIn, bool, int) {
	user, address, ctx := userName(typed), s.ClientAddress(r), r.Context()
	if wait, err := s.limiter.reserve(ctx, user, address); err != nil || wait > 0 {
		return signedIn{}, false, wait
	}
	var mine *checked // what this request compared and found; nil while it compared nothing
	defer func() { s.settle(user, address, mine) }()
	check := func(held bool) basicAnswer {
		mine = &checked{compared: held}
		*mine = s.checkBasic(ctx, v, user, pass)
		mine.compared = mine.compared || held
		if mine.busy {
			return basicAnswer{wait: seconds(FirstDelay)}
		}
		return basicAnswer{got: mine.got, ok: mine.ok}
	}
	a, shared, err := s.checks.do(ctx, s.creds.flightKey(v.gen, user, pass), func() basicAnswer {
		mine = &checked{} // without a result from here on, a failure
		got, ok, held := s.cached(v, user, pass)
		if ok {
			*mine = checked{got: got, ok: true, compared: true, remembered: true}
			return basicAnswer{got: got, ok: true}
		}
		return check(held)
	})
	switch {
	case err != nil:
		return signedIn{}, false, 0
	case shared && a.ok:
		return a.got.clone(), true, 0
	case shared && a.wait == 0:
		a = check(false)
	}
	return a.got, a.ok, a.wait
}

// settle gives a reservation of the pair back by one rule for every way out
// of a check (finding T1 of review 3): a request that compared the password
// with anything ends as a success or as a failure of its pair, never without
// a result. A success of a check forgets the failures of the pair; a
// remembered success forgets none (review R1). Only a request that compared
// nothing gives its reservation back without a result: one that took the
// answer of another request with the same password (c nil), and one whose
// directories had no place for it while no realm compared the password with
// a credential of the name.
func (s *Service) settle(user, address string, c *checked) {
	switch {
	case c == nil || c.remembered:
		s.limiter.release(user, address)
	case c.ok:
		s.limiter.end(user, address, true)
	case c.busy && !c.compared:
		s.limiter.release(user, address)
	default:
		s.limiter.end(user, address, false)
	}
}

// cached is a success of the last CredentialTime against the view, asked in
// the realm order of the view; held reports that the cache holds a success
// of the user name that the password missed or found.
func (s *Service) cached(v *view, user, pass string) (got signedIn, ok, held bool) {
	return s.creds.find(v.checked, user, pass, v.gen)
}

// checkBasic checks Basic credentials against the realms of the stored
// order (signIn) and remembers a success. The caller holds a reservation of
// the limiter.
func (s *Service) checkBasic(ctx context.Context, v *view, user, pass string) checked {
	c := s.signIn(ctx, v, user, pass)
	if c.ok {
		s.creds.put(c.got.by, user, pass, c.got, v.gen)
	}
	return c
}

// checked is what a check of a user name and password found. compared: the
// check compared the password with a credential of the name, which settle
// reads: a remembered success of the name in the cache, found or missed; a
// stored password of an active local user; the answer of a directory, a
// refusal, an error and a panic included; a user token. busy: no realm
// accepted the pair and a directory had no place for the check, so that
// directory asked nothing (finding A2 of the security assessment). A busy
// check is answered with 429 and Retry-After 2, and the Retry-After is
// advice only (finding T5 of review 3): the limiter holds no wait for it.
// settle gives the reservation back or counts one failure by its rule, and
// the pair waits only once its failures make it wait, as after refusals.
// remembered: the success came from the cache.
type checked struct {
	got            signedIn
	ok             bool
	compared, busy bool
	remembered     bool
}

// tryPair runs check for the user name under a reservation of the pair in
// the limiter and settles it with the result; a panic of check counts as a
// refusal. wait > 0 means the pair may not try now and check did not run,
// or that the check was busy: that answer carries the Retry-After of
// FirstDelay as advice, and the limiter holds no wait for it (checked). The
// pair and check take the name without its outer white space.
func (s *Service) tryPair(typed, address string, check func(user string) checked) (signedIn, bool, int) {
	user := userName(typed)
	if wait := s.limiter.begin(user, address); wait > 0 {
		return signedIn{}, false, wait
	}
	var c checked
	defer func() { s.settle(user, address, &c) }()
	c = check(user)
	if c.busy {
		return signedIn{}, false, seconds(FirstDelay)
	}
	return c.got, c.ok, 0
}

// userName is a typed user name without its outer white space, the name
// every credential path keys the limiter with and checks (review S5): a
// directory that ignores outer spaces sees one account, so the limiter
// counts one pair.
func userName(typed string) string { return strings.TrimSpace(typed) }

func throttled(wait int) *model.Error {
	return &model.Error{Status: model.ErrThrottled.Status, Message: model.ErrThrottled.Message, RetryAfter: wait}
}

// ClientAddress is the address of the caller for the sign in limits, read
// with the trusted proxies of the service (TrustedProxies.ClientAddress).
func (s *Service) ClientAddress(r *http.Request) string {
	return s.proxies.ClientAddress(r)
}

// RefusalFloor is the least time a refused check takes, the local users and
// the directories together, above the slowest stored form measured in
// review 6 (PBKDF2, 1.4 ms). A successful check is not slowed down.
const RefusalFloor = 5 * time.Millisecond

// checkLocal verifies a local user and accepts only the state active
// (decision N4). It runs one hash for every user name, known or not. A good
// sign in stores an unsalted form again in the salted one (rehash.go).
// decides reports that the answer depends on the password: the name is an
// active local user with a stored form while the local realm is active.
// Only then was the password compared with a stored one; the hash of an
// unknown or inactive name decides nothing.
func (s *Service) checkLocal(v *view, user, pass string) (got signedIn, ok, decides bool) {
	u, known := v.localUser(user)
	stored := u.PasswordHash
	if !known || stored == "" {
		stored = s.dummy
	}
	match := VerifyPassword(pass, stored)
	decides = known && u.PasswordHash != "" && u.Status == "active" && v.localOn && pass != ""
	if !decides || !match {
		return signedIn{}, false, decides
	}
	s.rehash(v, u.ID, u.PasswordHash, pass)
	u.PasswordHash = ""
	return signedIn{principal: principal{user: u, realm: RealmLocal}, subject: v.subjectOf(u, u.Roles, RealmLocal), by: RealmLocal}, true, true
}

// checkDirectories asks the directories in their order; the first that
// accepts the pair signs the user in. Every answer of a directory counts as
// a comparison of the password, a refusal and an error as well; only a
// directory without a place for the check asked nothing. Such a directory
// marks the check as busy, and the directories after it are asked
// (finding T3 of review 3).
func (s *Service) checkDirectories(ctx context.Context, v *view, user, pass string) checked {
	if !v.ldapOn || pass == "" {
		return checked{}
	}
	var c checked
	for i, d := range v.dirs {
		u, groups, err := d.Login(ctx, user, pass)
		if errors.Is(err, ErrBusy) {
			c.busy = true
			continue
		}
		c.compared = true
		if err != nil {
			switch {
			case errors.Is(err, ErrUnreachable):
				s.log.Warn("directory did not answer", "connection", i+1, "of", len(v.dirs))
			case !errors.Is(err, ErrWrongPair):
				s.log.Warn("directory sign in failed", "connection", i+1, "of", len(v.dirs))
			}
			continue
		}
		if u.ID == "" {
			s.log.Warn("directory answered without a user id", "connection", i+1, "of", len(v.dirs))
			continue
		}
		u.Source = SourceLDAP
		u.PasswordHash = ""
		p := principal{user: u, realm: RealmLDAP, groups: slices.Clone(groups), until: s.now().Add(CredentialTime)}
		return checked{got: signedIn{principal: p, subject: v.subjectOf(u, v.directoryRoles(u.ID, groups), RealmLDAP), by: RealmLDAP},
			ok: true, compared: true}
	}
	return c
}

// signIn checks a pair against the realms of the stored order that check a
// user name and password: the local users, the directories, the user tokens
// (task 6 of the phase 3 plan). The first realm that accepts the pair signs
// the user in; a refusal of one realm lets the next one answer. A refusal
// lasts at least RefusalFloor over all of them together, so the time tells
// neither a known name, nor the hash form of a user, nor a directory that
// refused within the floor. A directory without a place for the check does
// not end it: the realms after it are asked (finding T3 of review 3). When
// none accepts the pair, the check ends as busy, after the floor as well,
// and compared says whether any realm compared the password. The floor runs
// on the real clock, not Options.Now. The caller holds a reservation of the
// limiter.
func (s *Service) signIn(ctx context.Context, v *view, user, pass string) checked {
	start := s.floorNow()
	var busy, compared bool
	for _, realm := range v.checked {
		var c checked
		switch realm {
		case RealmLocal:
			c.got, c.ok, c.compared = s.checkLocal(v, user, pass)
		case RealmLDAP:
			c = s.checkDirectories(ctx, v, user, pass)
		case RealmUserToken:
			c.got, c.ok, c.compared = s.checkUserToken(ctx, v, user, pass)
		}
		if c.ok {
			return c
		}
		busy, compared = busy || c.busy, compared || c.compared
	}
	s.floor(start)
	return checked{busy: busy, compared: compared}
}

// floor sleeps for what is left of RefusalFloor since start.
func (s *Service) floor(start time.Time) {
	if rest := RefusalFloor - s.floorNow().Sub(start); rest > 0 {
		s.floorSleep(rest)
	}
}

// Permits reports whether the subject holds the permission by the wildcard
// rule. A nil subject holds the rights of the anonymous user while anonymous
// access is on, and nothing otherwise; on the read path of a Docker
// repository only under the three conditions of N12 (docker.go).
func (s *Service) Permits(subject *model.Subject, permission string) bool {
	if subject == nil {
		v := s.view()
		if repo, ok := dockerPull(permission); ok && !v.dockerAccess(repo).AnonymousPull {
			return false
		}
		return authz.AnyImplies(v.permissionsOf(nil), permission)
	}
	return authz.AnyImplies(subject.Permissions, permission)
}

// PermitsSome is Permits with the reading of the contract for a * in the
// permission of a screen: the subject holds the permission for at least one
// value of each part that holds *.
func (s *Service) PermitsSome(subject *model.Subject, permission string) bool {
	if subject != nil {
		return authz.AnyImpliesSome(subject.Permissions, permission)
	}
	return authz.AnyImpliesSome(s.view().permissionsOf(nil), permission)
}

// permissionsOf is the rights of a subject; a nil subject holds those of the
// anonymous user while anonymous access is on.
func (v *view) permissionsOf(subject *model.Subject) []string {
	if subject != nil {
		return subject.Permissions
	}
	if v.snap.Anonymous.Enabled {
		return v.anonymous
	}
	return nil
}

// AnonymousEnabled reports whether anonymous access is on.
func (s *Service) AnonymousEnabled() bool {
	return s.view().snap.Anonymous.Enabled
}

// CSRF reports whether a request that changes state carries the request
// token of its session. Safe methods, requests without a session and
// requests with Basic credentials need none.
func (s *Service) CSRF(r *http.Request, subject *model.Subject) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if subject == nil || subject.Session == "" {
		return true
	}
	live, ok := s.sessions.get(subject.Session)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(live.csrf)) == 1
}

// Snapshot is the state the service checks against now.
func (s *Service) Snapshot() *model.Snapshot {
	return s.view().snap
}
