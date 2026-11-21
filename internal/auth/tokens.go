package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs/audit"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Tokens are issued new by this server (decisions N9 to N11 of the phase 3
// plan). The wire form is <prefix>.<public half>.<secret half>, 8 and 32
// bytes of crypto/rand in URL safe Base64 without padding. npm and user
// tokens are rows of the tokens table, which holds the public half and only
// a SHA-256 of the secret half. Docker bearer tokens live in memory for
// DockerTokenTime and are no rows. A token signs its user in while the realm
// of its kind and the realm of its user are in the stored order and the user
// is still there and active, a directory user while exactly one directory
// of the stored order holds it (lookup.go); the rights are those of the user
// at the request, never a scope a client asked for.

// The kinds of token.
const (
	KindNpm    = "npm"
	KindUser   = "user"
	KindDocker = "docker"
)

// The realm ids of the stored order that let the tokens of a kind in.
const (
	RealmNpm       = "NpmToken"
	RealmDocker    = "DockerToken"
	RealmUserToken = "User-Token-Realm"
)

// Bounds of the tokens.
const (
	MaxTokensPerKind = 20                // a user's npm logins per kind, and its named tokens per kind (N10)
	DockerTokenTime  = 300 * time.Second // the life of a Docker bearer token (N11)
	MaxDockerTokens  = 10_000            // Docker bearer tokens in memory (hardening line 9)
	TouchInterval    = time.Minute       // the last use of a token row is written at most this often
	publicBytes      = 8
	secretBytes      = 32
)

// tokenPrefixes are the prefixes of the wire form: for npm the one of the
// old product, which tells an old token from a damaged one (N9).
var tokenPrefixes = map[string]string{KindNpm: "NpmToken", KindUser: "UserToken", KindDocker: "DockerToken"}

// tokenRealms are the realms that let each kind in.
var tokenRealms = map[string]string{KindNpm: RealmNpm, KindUser: RealmUserToken, KindDocker: RealmDocker}

var (
	errOldShape = errors.New("a token in the shape of the old product")
	errBadToken = errors.New("no token of this server")
)

type parsedToken struct {
	kind, public, secret string
}

// parseToken reads the wire form. A known prefix with one part after it is
// the shape of the old product.
func parseToken(text string) (parsedToken, error) {
	parts := strings.Split(text, ".")
	kind := ""
	for k, prefix := range tokenPrefixes {
		if parts[0] == prefix {
			kind = k
		}
	}
	switch {
	case kind == "":
		return parsedToken{}, errBadToken
	case len(parts) == 2 && parts[1] != "":
		return parsedToken{}, errOldShape
	case len(parts) != 3 || !half(parts[1], publicBytes) || !half(parts[2], secretBytes):
		return parsedToken{}, errBadToken
	}
	return parsedToken{kind: kind, public: parts[1], secret: parts[2]}, nil
}

// half reports the canonical URL safe Base64 of n bytes.
func half(text string, n int) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(text)
	return err == nil && len(raw) == n
}

func randomHalf(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func digestOf(secret string) [32]byte { return sha256.Sum256([]byte(secret)) }

// NewToken makes a token of a kind for a user of a source: the wire form,
// shown once, and the row the tokens table keeps, with the SHA-256 of the
// secret half only. Both halves come from crypto/rand. The public half is
// the id of the row on the command line, so one that starts with '-', which
// a parser of arguments takes for an option, is drawn again.
func NewToken(kind, userID, source string, now time.Time) (string, model.Token, error) {
	prefix, ok := tokenPrefixes[kind]
	if !ok {
		return "", model.Token{}, fmt.Errorf("auth: no token kind %q", kind)
	}
	public := "-"
	for strings.HasPrefix(public, "-") {
		var err error
		if public, err = randomHalf(publicBytes); err != nil {
			return "", model.Token{}, err
		}
	}
	secret, err := randomHalf(secretBytes)
	if err != nil {
		return "", model.Token{}, err
	}
	row := model.Token{Public: public, Kind: kind, UserID: userID, Source: source, Digest: digestOf(secret), Created: now.UTC()}
	return prefix + "." + public + "." + secret, row, nil
}

// realmOff is the refusal of a token of a kind whose realm is not in the
// stored order, after a good pair.
func realmOff(what, realm string) *model.Error {
	return &model.Error{Status: http.StatusForbidden, Message: what + " are off: the realm " + realm + " is not in the active realms"}
}

// bearer resolves a bearer token. An anonymous Docker token gives no subject
// and no error, as a request without credentials does.
func (s *Service) bearer(ctx context.Context, v *view, text string) (*model.Subject, error) {
	tok, err := parseToken(text)
	if errors.Is(err, errOldShape) {
		// a key that matches but signs nobody in answers as one that matches
		// nothing, so the answer tells no right key from a wrong one
		// (review secrets-3)
		if subject, _ := s.importedBearer(ctx, v, text); subject != nil {
			return subject, nil
		}
		return nil, model.ErrOldToken
	}
	if err != nil || !v.realms[tokenRealms[tok.kind]] {
		return nil, model.ErrUnauthorized
	}
	var who *principal
	if tok.kind == KindDocker {
		var ok bool
		if who, ok = s.docker.get(tok.public, tok.secret); !ok {
			return nil, model.ErrUnauthorized
		}
		if who == nil {
			return nil, nil
		}
		if who.realm == RealmLDAP && !s.now().Before(who.until) {
			// the answer of the directory that issued the token has ended:
			// the directory is asked again, as for an npm token (review R7)
			p, ok := s.directoryUser(ctx, v, who.user.ID)
			if !ok || p.user.Entry != who.user.Entry {
				return nil, model.ErrUnauthorized
			}
			who = &p
		}
	} else {
		row, ok := s.tokenRow(tok)
		if !ok {
			return nil, model.ErrUnauthorized
		}
		p, ok := s.tokenUser(ctx, v, row)
		if !ok {
			return nil, model.ErrUnauthorized
		}
		who = &p
		s.touch(row)
	}
	subject, ok := v.subject(*who)
	if !ok {
		return nil, model.ErrUnauthorized
	}
	return &subject, nil
}

// revokeOrphan revokes the token of a local user that is gone, at its first
// use, as the old product deletes such a key: a user of the same name added
// later does not inherit it. Only a view of the generation of the store
// tells that the user is gone (review S1): a view loaded before the user was
// added, or the last view while the store does not answer, revokes nothing.
func (s *Service) revokeOrphan(v *view, row model.Token) {
	if gen, err := s.store.Generation(); err != nil || gen != v.gen {
		return
	}
	if _, err := s.store.RemoveToken(row.Public); err != nil {
		s.log.Warn("the token of a removed user was not revoked", "token", row.Public, "error", err.Error())
		return
	}
	s.log.Info("token of a removed user revoked", "token", row.Public, "user", row.UserID)
}

// tokenRow is the row of a token of the tokens table whose secret half
// matches, compared in constant time.
func (s *Service) tokenRow(tok parsedToken) (model.Token, bool) {
	if s.store == nil {
		return model.Token{}, false
	}
	row, found, err := s.store.Token(tok.public)
	if err != nil {
		s.log.Warn("a token could not be read from the state file", "error", err.Error())
		return model.Token{}, false
	}
	digest := digestOf(tok.secret)
	if !found || row.Kind != tok.kind || subtle.ConstantTimeCompare(digest[:], row.Digest[:]) != 1 {
		return model.Token{}, false
	}
	return row, true
}

// tokenUser is the user of a token row: a local user of the view, or a
// directory user that a directory of the stored order holds, with the
// groups of its answer (lookup.go). The token of a local user that is gone
// is revoked.
func (s *Service) tokenUser(ctx context.Context, v *view, row model.Token) (principal, bool) {
	switch row.Source {
	case SourceLocal:
		u, there := v.local[row.UserID]
		if !there {
			s.revokeOrphan(v, row)
			return principal{}, false
		}
		return localPrincipal(u), true
	case SourceLDAP:
		p, ok := s.directoryUser(ctx, v, row.UserID)
		if !ok {
			return principal{}, false
		}
		if !s.bindings.holds(row.Public, p.user.Entry) {
			s.log.Warn("the token of a directory user was issued for another directory entry of that name, so it is refused",
				"token", row.Public, "user", row.UserID)
			return principal{}, false
		}
		return p, true
	}
	return principal{}, false
}

// touch writes the last use of a token row, at most once per TouchInterval;
// it raises no generation. A failure leaves the request as it is.
func (s *Service) touch(row model.Token) {
	now := s.now()
	if !row.LastUsed.IsZero() && now.Sub(row.LastUsed) < TouchInterval {
		return
	}
	if err := s.store.TouchToken(row.Public, now); err != nil {
		s.log.Warn("the last use of a token was not stored", "token", row.Public, "error", err.Error())
	}
}

// NpmLogin signs a user name and password in, as the sign in of the
// interface does, and issues an npm token for that user. A pair the limits
// refuse gets no check; a good pair gets no token while the realm of npm
// tokens is not in the stored order.
func (s *Service) NpmLogin(r *http.Request, user, password string) (model.IssuedToken, error) {
	v := s.refresh()
	got, ok, wait := s.tryPair(user, s.ClientAddress(r), func(user string) checked {
		return s.signIn(r.Context(), v, user, password)
	})
	if ok {
		audit.SetSubject(r.Context(), got.subject.User.ID, got.subject.Realm)
	} else {
		setRefused(r.Context(), v, user)
	}
	switch {
	case wait > 0:
		return model.IssuedToken{}, throttled(wait)
	case !ok:
		return model.IssuedToken{}, model.ErrUnauthorized
	case !v.realms[RealmNpm]:
		return model.IssuedToken{}, realmOff("npm tokens", RealmNpm)
	case s.store == nil:
		return model.IssuedToken{}, errors.New("auth: tokens need the state file")
	}
	audit.SetSubject(r.Context(), got.subject.User.ID, got.subject.Realm)
	text, row, err := NewToken(KindNpm, got.principal.user.ID, got.principal.user.Source, s.now())
	if err != nil {
		return model.IssuedToken{}, err
	}
	revoked, err := s.store.AddToken(row, MaxTokensPerKind)
	if err != nil {
		return model.IssuedToken{}, err
	}
	for _, public := range revoked {
		s.bindings.drop(public)
	}
	if got.principal.realm == RealmLDAP {
		s.bindings.holds(row.Public, got.principal.user.Entry)
	}
	return model.IssuedToken{Text: text, IssuedAt: row.Created}, nil
}

// DockerToken answers the token route of the Docker flow: Basic credentials
// of r are checked as every Basic request is, and the token names their
// user; a request without credentials gets a token of the anonymous subject
// when the repository of the token route may be pulled anonymously (N12).
// Any other credential is refused, and a session cookie does not count. The
// token lives in memory for DockerTokenTime.
func (s *Service) DockerToken(r *http.Request, repository string) (model.IssuedToken, error) {
	v := s.refresh()
	header := r.Header.Get("Authorization")
	if header == "" {
		if !v.dockerAccess(repository).AnonymousPull {
			return model.IssuedToken{}, model.ErrUnauthorized
		}
		return s.issueDocker(nil)
	}
	scheme, credentials, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, "Basic") {
		return model.IssuedToken{}, model.ErrUnauthorized
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(credentials))
	user, pass, found := strings.Cut(string(decoded), ":")
	if err != nil || !found {
		return model.IssuedToken{}, model.ErrUnauthorized
	}
	got, ok, wait := s.basic(r, v, user, pass)
	if ok {
		audit.SetSubject(r.Context(), got.subject.User.ID, got.subject.Realm)
	} else {
		setRefused(r.Context(), v, user)
	}
	switch {
	case wait > 0:
		return model.IssuedToken{}, throttled(wait)
	case !ok:
		return model.IssuedToken{}, model.ErrUnauthorized
	case !v.realms[RealmDocker]:
		return model.IssuedToken{}, realmOff("Docker bearer tokens", RealmDocker)
	}
	who := got.principal
	return s.issueDocker(&who)
}

// issueDocker puts a Docker bearer token of who into memory. A token of the
// anonymous subject, who nil, takes no place there (review N2): its public
// half is the time of its issue on the monotonic clock of the service
// (review 3-37) and its secret half a MAC of that under a key of the
// process.
func (s *Service) issueDocker(who *principal) (model.IssuedToken, error) {
	if who == nil {
		public, secret, issued := s.docker.anonymous()
		return model.IssuedToken{Text: tokenPrefixes[KindDocker] + "." + public + "." + secret,
			ExpiresIn: int(DockerTokenTime / time.Second), IssuedAt: issued}, nil
	}
	public, err := randomHalf(publicBytes)
	if err != nil {
		return model.IssuedToken{}, err
	}
	secret, err := randomHalf(secretBytes)
	if err != nil {
		return model.IssuedToken{}, err
	}
	issued := s.docker.put(public, digestOf(secret), who)
	return model.IssuedToken{Text: tokenPrefixes[KindDocker] + "." + public + "." + secret,
		ExpiresIn: int(DockerTokenTime / time.Second), IssuedAt: issued}, nil
}

// RevokeToken revokes an npm or user token that the subject holds (npm
// logout). ErrNoToken when the subject holds no such token.
func (s *Service) RevokeToken(subject *model.Subject, text string) error {
	if subject == nil {
		return model.ErrUnauthorized
	}
	tok, err := parseToken(text)
	if errors.Is(err, errOldShape) {
		removed, err := s.revokeImported(subject, text)
		if err != nil {
			return err
		}
		if !removed {
			return model.ErrNoToken
		}
		return nil
	}
	if err != nil || tok.kind == KindDocker {
		return model.ErrNoToken
	}
	row, ok := s.tokenRow(tok)
	if !ok || row.UserID != subject.User.ID || row.Source != subject.User.Source {
		return model.ErrNoToken
	}
	removed, err := s.store.RemoveToken(row.Public)
	if err != nil {
		return err
	}
	s.bindings.drop(row.Public)
	if !removed {
		return model.ErrNoToken
	}
	return nil
}

// Tokens are the npm and user tokens of a user of a source, in the order of
// their issue, without their secret halves.
func (s *Service) Tokens(userID, source string) ([]model.Token, error) {
	if s.store == nil {
		return []model.Token{}, nil
	}
	return s.store.Tokens(userID, source)
}

// dockerTokens holds the Docker bearer tokens of signed in users in memory:
// the SHA-256 of the secret half, who the token names and when it ends. A
// full store drops the expired tokens first, then the one that ends first.
// Tokens of the anonymous subject are no entries; key signs them, and
// elapsed, the monotonic clock of the service, dates them.
type dockerTokens struct {
	mu      sync.Mutex
	now     func() time.Time
	elapsed func() time.Duration
	max     int
	entries map[string]dockerToken
	key     []byte
}

type dockerToken struct {
	digest [32]byte
	who    *principal // nil: the anonymous subject
	until  time.Time
}

func newDockerTokens(now func() time.Time) (*dockerTokens, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	start := now()
	return &dockerTokens{now: now, elapsed: func() time.Duration { return now().Sub(start) }, max: MaxDockerTokens,
		entries: map[string]dockerToken{}, key: key}, nil
}

// anonymous gives the halves of a token of the anonymous subject issued now:
// the time of the issue on the monotonic clock in milliseconds as a varint,
// and its MAC. issued is the wall clock time of the answer.
func (d *dockerTokens) anonymous() (public, secret string, issued time.Time) {
	issued = time.UnixMilli(d.now().UnixMilli())
	var at [publicBytes]byte
	binary.PutVarint(at[:], d.elapsed().Milliseconds())
	public = base64.RawURLEncoding.EncodeToString(at[:])
	return public, base64.RawURLEncoding.EncodeToString(d.mac(public)), issued
}

func (d *dockerTokens) mac(public string) []byte {
	m := hmac.New(sha256.New, d.key)
	m.Write([]byte("anonymous\x00" + public))
	return m.Sum(nil)
}

// liveAnonymous reports a token of the anonymous subject whose MAC matches
// and whose time has not ended.
func (d *dockerTokens) liveAnonymous(public, secret string) bool {
	at, err := base64.RawURLEncoding.DecodeString(public)
	if err != nil || len(at) != publicBytes {
		return false
	}
	mac, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || !hmac.Equal(mac, d.mac(public)) {
		return false
	}
	ms, n := binary.Varint(at)
	age := d.elapsed() - time.Duration(ms)*time.Millisecond
	return n > 0 && age >= 0 && age < DockerTokenTime
}

// put keeps a token for DockerTokenTime and returns the time of its issue.
func (d *dockerTokens) put(public string, digest [32]byte, who *principal) time.Time {
	if who != nil {
		p := *who
		p.groups = slices.Clone(p.groups)
		who = &p
	}
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.entries) >= d.max {
		d.evict(now)
	}
	d.entries[public] = dockerToken{digest: digest, who: who, until: now.Add(DockerTokenTime)}
	return now
}

func (d *dockerTokens) evict(now time.Time) {
	first, firstUntil, found := "", time.Time{}, false
	for public, t := range d.entries {
		if !now.Before(t.until) {
			delete(d.entries, public)
			continue
		}
		if !found || t.until.Before(firstUntil) {
			first, firstUntil, found = public, t.until, true
		}
	}
	if len(d.entries) >= d.max && found {
		delete(d.entries, first)
	}
}

// get is who a live token names when its secret half matches, compared in
// constant time; who is nil for the anonymous subject.
func (d *dockerTokens) get(public, secret string) (*principal, bool) {
	if d.liveAnonymous(public, secret) {
		return nil, true
	}
	digest := digestOf(secret)
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.entries[public]
	if !ok || subtle.ConstantTimeCompare(digest[:], t.digest[:]) != 1 {
		return nil, false
	}
	if !now.Before(t.until) {
		delete(d.entries, public)
		return nil, false
	}
	if t.who == nil {
		return nil, true
	}
	p := *t.who
	p.groups = slices.Clone(p.groups)
	return &p, true
}

func (d *dockerTokens) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

// checkUserToken takes a user token as the password of its user, as the
// realm of user tokens does at its place in the stored order: the user name
// must be the one of the token, its id compared without outer white space as
// the typed name is (review R6). compared reports a password in the form of
// a user token, which the realm compares with the stored tokens; any other
// password it compares with nothing.
func (s *Service) checkUserToken(ctx context.Context, v *view, user, password string) (got signedIn, ok, compared bool) {
	tok, err := parseToken(password)
	if err != nil || tok.kind != KindUser {
		return signedIn{}, false, false
	}
	row, ok := s.tokenRow(tok)
	if !ok || userName(row.UserID) != user {
		return signedIn{}, false, true
	}
	p, ok := s.tokenUser(ctx, v, row)
	if !ok {
		return signedIn{}, false, true
	}
	subject, ok := v.subject(p)
	if !ok {
		return signedIn{}, false, true
	}
	s.touch(row)
	return signedIn{principal: p, subject: subject, by: RealmUserToken}, true, true
}
