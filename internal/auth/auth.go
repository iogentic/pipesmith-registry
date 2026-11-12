// Package auth implements model.Security for the thin slice: the password
// forms of the old product, interface sessions in memory with a request
// token, sign in limits, Basic credentials of local users, and sign in
// through LDAP directories behind the Directory interface. Rights come from
// package authz.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/authz"
	"github.com/mirosso25/pipesmith-registry/internal/model"
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

// realmsKnown are the stored realm ids this server reads: the two local ones
// and LDAP. Every other id of the stored order is named once at start.
var realmsKnown = []string{RealmLocal, "NexusAuthorizingRealm", RealmLDAP}

// Options configure a Service.
type Options struct {
	// Snapshot is the identity state; required.
	Snapshot *model.Snapshot
	// Directories are the LDAP connections in their order. They sign in
	// through POST /api/session only; Basic credentials are checked against
	// local users (decision T7).
	Directories []Directory
	// Logger receives warnings at start and when a directory does not
	// answer; nil writes none. No line holds a password, a hash, a session
	// id or a request token.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Service implements model.Security. It is safe for concurrent use.
type Service struct {
	snap      *model.Snapshot
	resolver  *authz.Resolver
	sessions  *sessionStore
	limiter   *limiter
	dirs      []Directory
	log       *slog.Logger
	local     map[string]model.User
	localOn   bool
	ldapOn    bool
	anonymous []string // permissions of the anonymous user
	dummy     string   // a Shiro 1 hash nobody knows, checked for an unknown user
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
	s := &Service{
		snap:     o.Snapshot,
		resolver: authz.NewResolver(o.Snapshot),
		sessions: newSessionStore(now),
		limiter:  newLimiter(now),
		dirs:     slices.Clone(o.Directories),
		log:      log,
		local:    map[string]model.User{},
		localOn:  slices.Contains(o.Snapshot.Realms, RealmLocal),
		ldapOn:   slices.Contains(o.Snapshot.Realms, RealmLDAP),
		dummy: "$shiro1$SHA-512$1024$" + base64.StdEncoding.EncodeToString(salt) + "$" +
			base64.StdEncoding.EncodeToString(digest),
	}
	for _, u := range o.Snapshot.Users {
		s.local[u.ID] = u
	}
	s.anonymous = s.anonymousPermissions()
	s.startLog()
	return s, nil
}

func (s *Service) startLog() {
	if !s.localOn {
		s.log.Warn("realm not in the stored order: local users do not sign in", "realm", RealmLocal)
	}
	if !s.ldapOn && len(s.dirs) > 0 {
		s.log.Warn("realm not in the stored order: LDAP users do not sign in", "realm", RealmLDAP)
	}
	var unknown []string
	for _, r := range s.snap.Realms {
		if !slices.Contains(realmsKnown, r) {
			unknown = append(unknown, r)
		}
	}
	if len(unknown) > 0 {
		s.log.Warn("realms this server does not build: nobody signs in through them", "realms", strings.Join(unknown, ","))
	}
	for kind, n := range s.resolver.Ignored() {
		s.log.Warn("stored privileges that grant nothing", "type", kind, "count", n)
	}
}

// anonymousPermissions expands the roles of the configured anonymous user:
// its local row, else a role mapping of the local source. A missing or
// inactive user holds nothing.
func (s *Service) anonymousPermissions() []string {
	id := s.snap.Anonymous.UserID
	if u, ok := s.local[id]; ok {
		if u.Status != "active" {
			return []string{}
		}
		return s.resolver.Permissions(u.Roles)
	}
	return s.resolver.Permissions(s.mappedRoles(id, SourceLocal))
}

// mappedRoles are the roles of the role mappings of a source for a user id,
// matched without regard to letter case as the old product does.
func (s *Service) mappedRoles(id, source string) []string {
	var roles []string
	for _, m := range s.snap.RoleMappings {
		if m.Source == source && strings.EqualFold(m.UserID, id) {
			roles = append(roles, m.Roles...)
		}
	}
	return roles
}

// Subject resolves the caller: a live session cookie, else Basic credentials
// of a local user. Without credentials it returns nil and no error. A cookie
// that names no live session, without Basic credentials next to it, gives
// ErrUnauthorized, so the interface learns that its session ended.
func (s *Service) Subject(r *http.Request) (*model.Subject, error) {
	cookie, cookieErr := r.Cookie(CookieName)
	if cookieErr == nil {
		if live, ok := s.sessions.get(cookie.Value); ok {
			subject := live.subject
			subject.Permissions = slices.Clone(subject.Permissions)
			subject.User.Roles = slices.Clone(subject.User.Roles)
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
	subject, ok, wait := s.tryPair(user, clientAddress(r), func() (model.Subject, bool) {
		return s.checkLocal(user, pass)
	})
	if wait > 0 {
		return nil, throttled(wait)
	}
	if !ok {
		return nil, model.ErrUnauthorized
	}
	return &subject, nil
}

// tryPair runs check under a reservation of the pair in the limiter and
// settles it with the result; a panic of check counts as a refusal. wait > 0
// means the pair may not try now and check did not run.
func (s *Service) tryPair(user, address string, check func() (model.Subject, bool)) (subject model.Subject, ok bool, wait int) {
	if wait = s.limiter.begin(user, address); wait > 0 {
		return model.Subject{}, false, wait
	}
	defer func() { s.limiter.end(user, address, ok) }()
	subject, ok = check()
	return subject, ok, 0
}

func throttled(wait int) *model.Error {
	return &model.Error{Status: model.ErrThrottled.Status, Message: model.ErrThrottled.Message, RetryAfter: wait}
}

// clientAddress is the host of the connection; forwarded headers do not
// count (decision N6).
func clientAddress(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// RefusalFloor is the least time a refused local check takes, above the
// slowest stored form measured in review 6 (PBKDF2, 1.4 ms). A successful
// check is not slowed down.
const RefusalFloor = 5 * time.Millisecond

// checkLocal verifies a local user and accepts only the state active
// (decision N4). It runs one hash for every user name, known or not, and a
// refusal lasts at least RefusalFloor, so the time tells neither a known
// name nor the hash form of a user. The floor uses the real clock, not
// Options.Now.
func (s *Service) checkLocal(user, pass string) (model.Subject, bool) {
	start := time.Now()
	u, known := s.local[user]
	stored := u.PasswordHash
	if !known || stored == "" {
		stored = s.dummy
	}
	match := VerifyPassword(pass, stored)
	if !known || u.PasswordHash == "" || !match || u.Status != "active" || !s.localOn || pass == "" {
		if rest := RefusalFloor - time.Since(start); rest > 0 {
			time.Sleep(rest)
		}
		return model.Subject{}, false
	}
	return s.subjectOf(u, u.Roles, RealmLocal), true
}

// checkDirectories asks the directories in their order; the first that
// accepts the pair signs the user in.
func (s *Service) checkDirectories(ctx context.Context, user, pass string) (model.Subject, bool) {
	if !s.ldapOn || pass == "" {
		return model.Subject{}, false
	}
	for i, d := range s.dirs {
		u, groups, err := d.Login(ctx, user, pass)
		if err != nil {
			if errors.Is(err, ErrUnreachable) {
				s.log.Warn("directory did not answer", "connection", i+1, "of", len(s.dirs))
			} else if !errors.Is(err, ErrWrongPair) {
				s.log.Warn("directory sign in failed", "connection", i+1, "of", len(s.dirs))
			}
			continue
		}
		if u.ID == "" {
			s.log.Warn("directory answered without a user id", "connection", i+1, "of", len(s.dirs))
			continue
		}
		u.Source = SourceLDAP
		var roles []string
		for _, g := range groups {
			if len(s.resolver.Roles([]string{g})) > 0 {
				roles = append(roles, g)
			}
		}
		roles = append(roles, s.mappedRoles(u.ID, SourceLDAP)...)
		slices.Sort(roles)
		return s.subjectOf(u, slices.Compact(roles), RealmLDAP), true
	}
	return model.Subject{}, false
}

func (s *Service) subjectOf(u model.User, roles []string, realm string) model.Subject {
	u.PasswordHash = ""
	u.Roles = slices.Clone(roles)
	return model.Subject{User: u, Realm: realm, Permissions: s.resolver.Permissions(roles)}
}

// signIn checks a pair against the local users, then the directories. The
// caller holds a reservation of the limiter.
func (s *Service) signIn(ctx context.Context, user, pass string) (model.Subject, bool) {
	if subject, ok := s.checkLocal(user, pass); ok {
		return subject, true
	}
	return s.checkDirectories(ctx, user, pass)
}

// Permits reports whether the subject holds the permission by the wildcard
// rule. A nil subject holds the rights of the anonymous user while anonymous
// access is on, and nothing otherwise.
func (s *Service) Permits(subject *model.Subject, permission string) bool {
	return authz.AnyImplies(s.permissionsOf(subject), permission)
}

// PermitsSome is Permits with the reading of the contract for a * in the
// permission of a screen: the subject holds the permission for at least one
// value of each part that holds *.
func (s *Service) PermitsSome(subject *model.Subject, permission string) bool {
	return authz.AnyImpliesSome(s.permissionsOf(subject), permission)
}

func (s *Service) permissionsOf(subject *model.Subject) []string {
	if subject != nil {
		return subject.Permissions
	}
	if s.snap.Anonymous.Enabled {
		return s.anonymous
	}
	return nil
}

// AnonymousEnabled reports whether anonymous access is on.
func (s *Service) AnonymousEnabled() bool {
	return s.snap.Anonymous.Enabled
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

// Snapshot is the state of the service.
func (s *Service) Snapshot() *model.Snapshot {
	return s.snap
}
