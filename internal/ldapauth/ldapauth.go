// Package ldapauth signs users in against the directory connections of the
// old product (T12 of docs/plans/thin-slice.md): a service bind, a search
// for the user entry, a bind as that entry with the given password, then the
// groups of the entry. It is the only package that imports the LDAP module.
// It writes no log line, and no error and no printed value holds a password.
//
// Built: the simple and the anonymous service bind, ldap and ldaps (verified
// against the system roots plus the certificates of WithCACertificates),
// static groups by member value and dynamic groups by the member of
// attribute (N19), the incident counter of each connection. DIGEST-MD5 and
// CRAM-MD5 wait for phase 3; New refuses them.
package ldapauth

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/go-ldap/ldap/v3"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

var (
	// ErrInvalidCredentials is every refusal of a pair of user name and
	// password: an unknown user, a wrong password, a name that matches more
	// than one entry, an empty password. Login returns it unwrapped and never
	// says which of these it was.
	ErrInvalidCredentials = errors.New("ldapauth: user name or password not accepted")
	// ErrUnavailable is wrapped by every failure of the directory itself: no
	// connection, a timeout, a refused service bind, a failed search, a
	// connection that rests after its incidents, a context that ended.
	ErrUnavailable = errors.New("ldapauth: directory not available")
	// ErrConfiguration is wrapped by every connection setting New refuses.
	ErrConfiguration = errors.New("ldapauth: connection setting refused")
	// ErrNoEntry is a user name of no single entry, the refusal of Lookup:
	// none, more than one, or a name outside the bounds of a user name.
	ErrNoEntry = errors.New("ldapauth: no single entry of the user name")
	// ErrBusy is wrapped by a sign in the connection had no place for:
	// MaxChecksInFlight sign ins ran against it for the whole wait, which
	// ends at the connection timeout or with the context of the sign in, or
	// MaxWaiting sign ins waited already, so it did not wait. It opened no
	// connection and asked nothing.
	ErrBusy = errors.New("ldapauth: every place for a sign in of the connection is taken")
)

// MaxChecksInFlight bounds the sign ins that run against one connection at
// a time (finding A2 of the security assessment): without it every sign in
// with an unknown name opened a connection of its own. Parallel sign ins of
// one name and password share one check and repeated Basic credentials come
// from the cache of the identity code, so the checks in flight at one moment
// come from distinct accounts that sign in together, as after a start. A
// burst of more than 16 such accounts waits for places in the order of
// arrival (places), and a flood of unknown names keeps 16 connections per
// directory open.
const MaxChecksInFlight = 16

// MaxWaiting bounds the sign ins that wait for a place of one connection
// (finding T2 of review 3), four for each place. Every sign in that waits
// holds the reservation of its pair in the limiter of the identity code, so
// with 16 sign ins in flight and 64 waiting, the requests at one directory
// hold at most 80 pairs of its table per connection. 64 lets four times the
// places of distinct accounts that sign in together wait their turn; a
// flood of unknown names beyond them gets 429 at once.
const MaxWaiting = 4 * MaxChecksInFlight

// VariablePrefix starts the name of the environment variable that holds the
// bind password of one connection (T12).
const VariablePrefix = "PIPESMITH_REGISTRY_LDAP_PASSWORD_"

const (
	maxUsernameBytes  = 256
	maxPasswordBytes  = 1024
	defaultTimeout    = 30 * time.Second
	defaultRetryDelay = 300 * time.Second
	schemeNone        = "NONE"
	schemeSimple      = "SIMPLE"
)

// PasswordVariable is the environment variable for the bind password of the
// connection id: the id in upper case, every character other than an ASCII
// letter or digit as "_".
func PasswordVariable(id string) string {
	var b strings.Builder
	b.WriteString(VariablePrefix)
	for _, r := range id {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// PasswordFromEnv reads the bind password of the connection id from its
// environment variable. An empty value counts as not set.
func PasswordFromEnv(id string) (string, bool) {
	value, ok := os.LookupEnv(PasswordVariable(id))
	return value, ok && value != ""
}

type options struct {
	caPEM   [][]byte
	now     func() time.Time
	bounded bool
}

// Option changes how New builds a directory.
type Option func(*options)

// WithCACertificates adds the PEM certificates to the system roots that an
// ldaps connection is verified against (N18). There is no option that skips
// the verification.
func WithCACertificates(pem []byte) Option {
	return func(o *options) { o.caPEM = append(o.caPEM, pem) }
}

// The bounds of the reads of a check of a form (ruling I24): the entries of
// a search of users and of groups, and the bytes the connection reads in
// all, below TLS.
const (
	CheckUserEntries  = 21
	CheckGroupEntries = 1000
	CheckReadBytes    = 1 << 20
)

// ErrBound is wrapped by a check that a directory sent more than the bounds
// of WithCheckBounds allow; the error names the bound.
var ErrBound = errors.New("ldapauth: the directory sent more than a check reads")

// WithCheckBounds makes a directory for one check of a form: every search
// asks a size limit, the client stops reading past it, and the connection
// reads at most CheckReadBytes (ruling I24).
func WithCheckBounds() Option {
	return func(o *options) { o.bounded = true }
}

// withClock replaces the clock of the incident counter in tests.
func withClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// Directory is one directory connection. Login is safe for concurrent use;
// each sign in opens its own connection, at most MaxChecksInFlight at a
// time, and at most MaxWaiting others wait for a place.
type Directory struct {
	server       model.LDAPServer
	bindPassword string
	scheme       string
	addr         string
	tlsConfig    *tls.Config
	timeout      time.Duration
	userBase     string
	groupBase    string
	userScope    int
	groupScope   int
	dynamic      bool
	breaker      *breaker
	places       *places
	bounded      bool // the bounds of a check (WithCheckBounds)
	// memberBase is the group base a member of value must lie below to name
	// a group (review 3-61); nil when the connection names no group base
	memberBase *ldap.DN
	// lookups are the places of Lookup, apart from those of Login (review
	// 3-54), so the users of tokens and the sign ins wait for none of each
	// other's
	lookups *places
}

// New checks the settings of one connection and builds its directory. The
// bind password is used by the simple scheme only and must be set for it;
// PasswordFromEnv reads it from the environment.
func New(server model.LDAPServer, bindPassword string, opts ...Option) (*Directory, error) {
	o := options{now: time.Now}
	for _, opt := range opts {
		opt(&o)
	}
	refuse := func(format string, args ...any) (*Directory, error) {
		return nil, fmt.Errorf("%w: connection %q: %s", ErrConfiguration, server.Name, fmt.Sprintf(format, args...))
	}
	d := &Directory{server: server, bounded: o.bounded}

	switch strings.ToLower(strings.TrimSpace(server.Protocol)) {
	case "ldap":
	case "ldaps":
		d.tlsConfig = &tls.Config{ServerName: server.Host, MinVersion: tls.VersionTLS12}
	default:
		return refuse("protocol %q is neither ldap nor ldaps", server.Protocol)
	}
	if strings.TrimSpace(server.Host) == "" {
		return refuse("no host")
	}
	if server.Port < 1 || server.Port > 65535 {
		return refuse("port %d is outside 1 to 65535", server.Port)
	}
	d.addr = net.JoinHostPort(server.Host, strconv.Itoa(server.Port))

	switch scheme := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(server.AuthScheme), "-", "_")); scheme {
	case schemeNone:
		d.scheme = scheme
	case schemeSimple:
		if server.AuthUsername == "" {
			return refuse("the simple scheme needs a bind name")
		}
		if bindPassword == "" {
			return refuse("the simple scheme needs a bind password: set %s or enter it at start", PasswordVariable(server.ID))
		}
		d.scheme, d.bindPassword = scheme, bindPassword
	case "DIGEST_MD5", "CRAM_MD5":
		return refuse("the scheme %s is not built in this version", scheme)
	default:
		return refuse("unknown authentication scheme %q", server.AuthScheme)
	}

	if strings.TrimSpace(server.SearchBase) == "" {
		return refuse("no search base")
	}
	for _, dn := range [][2]string{{"search base", server.SearchBase}, {"user base", server.UserBaseDN}, {"group base", server.GroupBaseDN}} {
		if _, err := ldap.ParseDN(strings.TrimSpace(dn[1])); err != nil {
			return refuse("the %s %q is not a DN", dn[0], dn[1])
		}
	}
	d.userBase = joinDN(server.UserBaseDN, server.SearchBase)
	d.groupBase = joinDN(server.GroupBaseDN, server.SearchBase)
	d.userScope = scope(server.UserSubtree)
	d.groupScope = scope(server.GroupSubtree)

	if !attributeName.MatchString(server.UserIDAttribute) {
		return refuse("user id attribute %q is not an attribute name", server.UserIDAttribute)
	}
	for _, a := range [][2]string{
		{"user object class", server.UserObjectClass},
		{"user real name attribute", server.UserRealNameAttribute},
		{"user email attribute", server.UserEmailAddressAttribute},
	} {
		if a[1] != "" && !attributeName.MatchString(a[1]) {
			return refuse("%s %q is not a name", a[0], a[1])
		}
	}
	if _, err := ldap.CompileFilter(userFilter(server.UserObjectClass, server.UserIDAttribute, server.UserLDAPFilter, "x")); err != nil {
		return refuse("the user filter %q does not compile", server.UserLDAPFilter)
	}

	if server.LDAPGroupsAsRoles {
		switch strings.ToLower(strings.TrimSpace(server.GroupType)) {
		case "static":
			if !attributeName.MatchString(server.GroupIDAttribute) {
				return refuse("group id attribute %q is not an attribute name", server.GroupIDAttribute)
			}
			if !memberAttribute.MatchString(server.GroupMemberAttribute) {
				return refuse("group member attribute %q is not an attribute name or one with a matching rule", server.GroupMemberAttribute)
			}
			if server.GroupObjectClass != "" && !attributeName.MatchString(server.GroupObjectClass) {
				return refuse("group object class %q is not a name", server.GroupObjectClass)
			}
			if !strings.Contains(server.GroupMemberFormat, "${dn}") && !strings.Contains(server.GroupMemberFormat, "${username}") {
				return refuse("the group member format %q names neither ${dn} nor ${username}", server.GroupMemberFormat)
			}
		case "dynamic":
			if !attributeName.MatchString(server.UserMemberOfAttribute) {
				return refuse("member of attribute %q is not an attribute name", server.UserMemberOfAttribute)
			}
			d.dynamic = true
			if strings.TrimSpace(server.GroupBaseDN) != "" {
				base, err := ldap.ParseDN(d.groupBase)
				if err != nil {
					return refuse("the group base %q is not a DN", d.groupBase)
				}
				d.memberBase = base
			}
		default:
			return refuse("group type %q is neither static nor dynamic", server.GroupType)
		}
	}

	if len(o.caPEM) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		for _, pem := range o.caPEM {
			if !pool.AppendCertsFromPEM(pem) {
				return refuse("the CA option holds no certificate")
			}
		}
		if d.tlsConfig != nil {
			d.tlsConfig.RootCAs = pool
		}
	}

	d.timeout = seconds(server.ConnectionTimeoutSeconds, defaultTimeout)
	d.breaker = newBreaker(max(server.MaxIncidentsCount, 0), seconds(server.ConnectionRetryDelaySeconds, defaultRetryDelay), o.now)
	d.places = newPlaces(d.timeout)
	d.lookups = newPlaces(d.timeout)
	return d, nil
}

func scope(subtree bool) int {
	if subtree {
		return ldap.ScopeWholeSubtree
	}
	return ldap.ScopeSingleLevel
}

func seconds(value int, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return time.Duration(value) * time.Second
}

// String names the connection without its bind password.
func (d Directory) String() string {
	return fmt.Sprintf("ldap directory %q (%s, %s)", d.server.Name, d.server.ID, d.addr)
}

// Format prints String for every verb, so no verb can print the bind
// password; without it a verb that does not fit prints all fields.
func (d Directory) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, d.String()) }

// Login checks the pair against the directory and returns the user with the
// names of its groups, sorted. The user has the source "LDAP", the state
// active, the id in the spelling of the directory, the real name as first
// name and no roles: the caller maps groups and role mappings to roles. A
// refused pair gives ErrInvalidCredentials, a failure of the directory an
// error that wraps ErrUnavailable. A sign in beyond MaxChecksInFlight waits
// for a place, at most the connection timeout and while ctx lasts; one that
// gets none, and one that finds MaxWaiting sign ins waiting already, gives
// an error that wraps ErrBusy and counts no incident. An empty password is
// refused before any connection (N17).
func (d *Directory) Login(ctx context.Context, username, password string) (model.User, []string, error) {
	if username == "" || len(username) > maxUsernameBytes || !utf8.ValidString(username) ||
		password == "" || len(password) > maxPasswordBytes {
		return model.User{}, nil, ErrInvalidCredentials
	}
	if err := ctx.Err(); err != nil {
		return model.User{}, nil, d.unavailable(ctx, "sign in", err)
	}
	if !d.breaker.allow() {
		return model.User{}, nil, fmt.Errorf("%w: connection %q rests after %d failures in a row",
			ErrUnavailable, d.server.Name, d.breaker.failures())
	}
	back, err := d.places.take(ctx)
	if err != nil {
		return model.User{}, nil, fmt.Errorf("%w: connection %q runs %d sign ins: %w", ErrBusy, d.server.Name, MaxChecksInFlight, err)
	}
	defer back()
	user, groups, err := d.login(ctx, username, password)
	switch {
	case err == nil || err == ErrInvalidCredentials:
		d.breaker.success()
	case ctx.Err() != nil:
		// The caller gave up; that is no incident of the directory.
	case errors.Is(err, errOfTheName):
		// The directory answered the search or bind of this name with an
		// error: no incident of the connection (review 3-61).
	default:
		d.breaker.failure()
	}
	if err != nil {
		return model.User{}, nil, err
	}
	return user, groups, nil
}

// session is one connection of a sign in or a check: the connection and,
// for a directory of WithCheckBounds, the count of the bytes it read.
type session struct {
	conn  *ldap.Conn
	bound *boundConn // nil without bounds
}

// fail is the error of a step of the session: the bound of the bytes when
// the session read past it, an error of a bound as it is, else a failure of
// the directory.
func (d *Directory) fail(ctx context.Context, s session, step string, err error) error {
	switch {
	case s.bound != nil && s.bound.over.Load():
		return fmt.Errorf("%w: connection %q: %s: more than %d bytes in all", ErrBound, d.server.Name, step, CheckReadBytes)
	case errors.Is(err, ErrBound):
		return err
	}
	return d.unavailable(ctx, step, err)
}

// errOfTheName marks a failure of a step for one name that the directory
// answered with a result code that is no trouble of the directory itself.
var errOfTheName = errors.New("an answer about this name")

// ofTheName is fail for a step of one name: the user search, the user bind
// or the group search. The error is of the name, and counts no incident,
// unless serverTrouble says the directory itself failed or a bound of a
// check ended the step.
func (d *Directory) ofTheName(ctx context.Context, s session, step string, err error) error {
	failed := d.fail(ctx, s, step, err)
	if errors.Is(failed, ErrBound) || serverTrouble(err) {
		return failed
	}
	return fmt.Errorf("%w (%w)", failed, errOfTheName)
}

// serverTrouble reports an error that says the directory itself failed: no
// result code of the server (a lost connection, a timeout of the client),
// or a result code of a busy, unavailable or slow server. Any other result
// code answers the request of one name.
func serverTrouble(err error) bool {
	var le *ldap.Error
	if !errors.As(err, &le) {
		return true
	}
	switch le.ResultCode {
	case ldap.LDAPResultTimeLimitExceeded, ldap.LDAPResultBusy, ldap.LDAPResultUnavailable:
		return true
	}
	return le.ResultCode >= ldap.ErrorNetwork
}

// search runs a search of the session. With the bounds of a check it stops
// reading past the size limit (EnforceSizeLimit).
func (d *Directory) search(s session, base string, scope, sizeLimit int, filter string, attributes []string) (*ldap.SearchResult, error) {
	req := ldap.NewSearchRequest(base, scope, ldap.NeverDerefAliases, sizeLimit, d.timeLimit(), false, filter, attributes, nil)
	req.EnforceSizeLimit = d.bounded
	return s.conn.Search(req)
}

// open connects and binds as the service; done closes the connection. The
// stop of the request closes conn from a goroutine of its own, so conn is a
// variable that no return writes: a return with an error after the stop
// began leaves it to close the connection, not nil.
func (d *Directory) open(ctx context.Context) (session, func(), error) {
	s, err := d.dial(ctx)
	if err != nil {
		return session{}, nil, d.unavailable(ctx, "connect", err)
	}
	conn := s.conn
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	done := func() {
		stop()
		_ = conn.Close()
	}
	conn.SetTimeout(d.timeout)
	if d.scheme == schemeSimple {
		if err := conn.Bind(d.server.AuthUsername, d.bindPassword); err != nil {
			done()
			return session{}, nil, d.fail(ctx, s, "service bind", err)
		}
	}
	return s, done, nil
}

func (d *Directory) login(ctx context.Context, username, password string) (model.User, []string, error) {
	s, done, err := d.open(ctx)
	if err != nil {
		return model.User{}, nil, err
	}
	defer done()
	entry, err := d.findUser(s, username)
	if err == ErrInvalidCredentials {
		return model.User{}, nil, err
	}
	if err != nil {
		return model.User{}, nil, d.ofTheName(ctx, s, "user search", err)
	}
	if err := s.conn.Bind(entry.DN, password); err != nil {
		if refusesPair(err) {
			return model.User{}, nil, ErrInvalidCredentials
		}
		return model.User{}, nil, d.ofTheName(ctx, s, "user bind", err)
	}
	return d.userOf(ctx, s, entry, username)
}

// userOf is the user of an entry and the names of its groups.
func (d *Directory) userOf(ctx context.Context, s session, entry *ldap.Entry, username string) (model.User, []string, error) {
	id := pickID(entry.GetEqualFoldAttributeValues(d.server.UserIDAttribute), username)
	groups, err := d.groups(s, entry, id)
	if err != nil {
		return model.User{}, nil, d.ofTheName(ctx, s, "group search", err)
	}
	return model.User{
		ID:        id,
		FirstName: first(entry, d.server.UserRealNameAttribute),
		Email:     first(entry, d.server.UserEmailAddressAttribute),
		Status:    "active",
		Source:    "LDAP",
		Roles:     []string{},
		Entry:     entryID(entry),
	}, groups, nil
}

// Lookup finds the entry of a user name under the service bind, the search
// of Login before its bind, and returns the user with the names of its
// groups as Login does. It binds as no user and checks no password: the user
// of a token is found this way. A name of no single entry, which covers one
// the user filter of the connection leaves out, and an entry the directory
// marks disabled or locked (lockedEntry, review 3-40) give ErrNoEntry; a
// failure of the directory an error that wraps ErrUnavailable. It counts
// incidents as Login does. Lookups hold places of their own, as many as
// Login has and with the same wait (review 3-54); one that gets none gives
// an error that wraps ErrUnavailable and ErrBusy and counts no incident.
func (d *Directory) Lookup(ctx context.Context, username string) (model.User, []string, error) {
	if username == "" || len(username) > maxUsernameBytes || !utf8.ValidString(username) {
		return model.User{}, nil, ErrNoEntry
	}
	if err := ctx.Err(); err != nil {
		return model.User{}, nil, d.unavailable(ctx, "lookup", err)
	}
	if !d.breaker.allow() {
		return model.User{}, nil, fmt.Errorf("%w: connection %q rests after %d failures in a row",
			ErrUnavailable, d.server.Name, d.breaker.failures())
	}
	back, err := d.lookups.take(ctx)
	if err != nil {
		return model.User{}, nil, fmt.Errorf("%w: %w: connection %q runs %d lookups: %w", ErrUnavailable, ErrBusy,
			d.server.Name, MaxChecksInFlight, err)
	}
	defer back()
	user, groups, err := d.lookup(ctx, username)
	switch {
	case err == nil || err == ErrNoEntry:
		d.breaker.success()
	case ctx.Err() != nil:
	case errors.Is(err, errOfTheName):
	default:
		d.breaker.failure()
	}
	if err != nil {
		return model.User{}, nil, err
	}
	return user, groups, nil
}

func (d *Directory) lookup(ctx context.Context, username string) (model.User, []string, error) {
	s, done, err := d.open(ctx)
	if err != nil {
		return model.User{}, nil, err
	}
	defer done()
	entry, err := d.findUser(s, username)
	if err == ErrInvalidCredentials {
		return model.User{}, nil, ErrNoEntry
	}
	if err != nil {
		return model.User{}, nil, d.ofTheName(ctx, s, "user search", err)
	}
	if lockedEntry(entry) {
		return model.User{}, nil, ErrNoEntry
	}
	return d.userOf(ctx, s, entry, username)
}

// Check connects and binds as a sign in does, then reads the entry of the
// search base, for `ldap check`. It counts no incident. The text names what
// passed; an error wraps ErrUnavailable and names the step that failed.
func (d *Directory) Check(ctx context.Context) (string, error) {
	s, err := d.dial(ctx)
	if err != nil {
		return "", d.unavailable(ctx, "connect", err)
	}
	conn := s.conn
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	conn.SetTimeout(d.timeout)
	done := []string{"ldap://" + d.addr}
	if d.tlsConfig != nil {
		done = []string{"ldaps://" + d.addr, "certificate verified"}
	}
	if d.scheme == schemeSimple {
		if err := conn.Bind(d.server.AuthUsername, d.bindPassword); err != nil {
			return "", d.fail(ctx, s, "service bind", err)
		}
		done = append(done, "service bind as "+d.server.AuthUsername)
	} else {
		done = append(done, "no service bind (scheme NONE)")
	}
	base := strings.TrimSpace(d.server.SearchBase)
	_, err = d.search(s, base, ldap.ScopeBaseObject, 1, "(objectClass=*)", []string{"1.1"})
	if err == ldap.ErrSizeLimitExceeded {
		return "", fmt.Errorf("%w: connection %q: search base: more than 1 entry", ErrBound, d.server.Name)
	}
	if err != nil {
		return "", d.fail(ctx, s, "search base", err)
	}
	return strings.Join(append(done, "search base "+base+" found"), ", "), nil
}

// Member is a user of the directory with the names of its groups, sorted.
type Member struct {
	User   model.User
	Groups []string
}

// Users connects and binds as a sign in does and lists the users of the
// connection, the entries the user search of Login finds for any name, each
// with its groups as Login reads them: at most limit of them, sorted by
// id, and more reports that the directory holds further entries. It is the
// check of the user mapping of a form; it binds as no user and counts no
// incident. An error wraps ErrUnavailable and names the step that failed;
// with the bounds of a check, one past them wraps ErrBound.
func (d *Directory) Users(ctx context.Context, limit int) (members []Member, more bool, err error) {
	if limit < 1 || d.bounded && limit+1 > CheckUserEntries {
		return nil, false, fmt.Errorf("ldapauth: a list of users needs a limit of 1 to %d, not %d", CheckUserEntries-1, limit)
	}
	if err := ctx.Err(); err != nil {
		return nil, false, d.unavailable(ctx, "list users", err)
	}
	s, done, err := d.open(ctx)
	if err != nil {
		return nil, false, err
	}
	defer done()
	attributes := []string{d.server.UserIDAttribute}
	for _, a := range []string{d.server.UserRealNameAttribute, d.server.UserEmailAddressAttribute} {
		if a != "" {
			attributes = append(attributes, a)
		}
	}
	if d.dynamic {
		attributes = append(attributes, d.server.UserMemberOfAttribute)
	}
	filter := "(&" + classFilter(d.server.UserObjectClass) + "(" + d.server.UserIDAttribute + "=*)" + extraFilter(d.server.UserLDAPFilter) + ")"
	result, err := d.search(s, d.userBase, d.userScope, limit+1, filter, attributes)
	switch {
	case err == ldap.ErrSizeLimitExceeded:
		return nil, false, d.fail(ctx, s, "user search",
			fmt.Errorf("%w: connection %q: user search: more than %d user entries", ErrBound, d.server.Name, limit+1))
	case ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) && result != nil:
		more = true
	case err != nil:
		return nil, false, d.fail(ctx, s, "user search", err)
	}
	entries := result.Entries
	if len(entries) > limit {
		entries, more = entries[:limit], true
	}
	for _, entry := range entries {
		id := first(entry, d.server.UserIDAttribute)
		if id == "" || strings.TrimSpace(entry.DN) == "" {
			continue
		}
		user, groups, err := d.userOf(ctx, s, entry, id)
		if err != nil {
			return nil, false, err
		}
		members = append(members, Member{User: user, Groups: groups})
	}
	slices.SortStableFunc(members, func(a, b Member) int { return cmp.Compare(a.User.ID, b.User.ID) })
	return members, more, nil
}

// errReadBound is the error of a read past the bytes of a check.
var errReadBound = errors.New("ldapauth: the bytes of a check are read")

// boundConn is a connection that reads at most left bytes; a read past
// them fails and sets over. One goroutine reads at a time: the handshake of
// TLS before the reader of the LDAP module starts, then that reader.
type boundConn struct {
	net.Conn
	left int64
	over atomic.Bool
}

func (c *boundConn) Read(p []byte) (int, error) {
	if c.left <= 0 {
		c.over.Store(true)
		return 0, errReadBound
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.Conn.Read(p)
	c.left -= int64(n)
	return n, err
}

// dial connects, with the handshake of TLS for ldaps, within the
// connection timeout. A directory of WithCheckBounds counts the bytes the
// connection reads below TLS.
func (d *Directory) dial(ctx context.Context) (session, error) {
	raw, err := (&net.Dialer{Timeout: d.timeout}).DialContext(ctx, "tcp", d.addr)
	if err != nil {
		return session{}, err
	}
	var s session
	c := raw
	if d.bounded {
		s.bound = &boundConn{Conn: raw, left: CheckReadBytes}
		c = s.bound
	}
	if d.tlsConfig != nil {
		tc := tls.Client(c, d.tlsConfig)
		hctx, cancel := context.WithTimeout(ctx, d.timeout)
		err := tc.HandshakeContext(hctx)
		cancel()
		if err != nil {
			_ = raw.Close() // the handshake failed; its error counts
			return session{}, err
		}
		c = tc
	}
	s.conn = ldap.NewConn(c, d.tlsConfig != nil)
	s.conn.Start()
	return s, nil
}

// findUser gives the one entry of the user name, or ErrInvalidCredentials
// for none or more than one. Referrals are not followed (N17).
func (d *Directory) findUser(s session, username string) (*ldap.Entry, error) {
	attributes := []string{d.server.UserIDAttribute}
	for _, a := range []string{d.server.UserRealNameAttribute, d.server.UserEmailAddressAttribute} {
		if a != "" {
			attributes = append(attributes, a)
		}
	}
	if d.dynamic {
		attributes = append(attributes, d.server.UserMemberOfAttribute)
	}
	attributes = append(attributes, entryAttributes...)
	result, err := d.search(s, d.userBase, d.userScope, 2,
		userFilter(d.server.UserObjectClass, d.server.UserIDAttribute, d.server.UserLDAPFilter, username), attributes)
	if ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) || err == ldap.ErrSizeLimitExceeded {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	return pickEntry(result.Entries)
}

// pickEntry gives the one entry of a user search. None, more than one and an
// entry without a DN refuse the pair: a bind with an empty name and a
// password is anonymous on some servers.
func pickEntry(entries []*ldap.Entry) (*ldap.Entry, error) {
	if len(entries) != 1 || strings.TrimSpace(entries[0].DN) == "" {
		return nil, ErrInvalidCredentials
	}
	return entries[0], nil
}

// groups gives the group names of an entry. Static groups are read as the
// service again, since a user bind changed who the connection is. With the
// bounds of a check a search reads at most CheckGroupEntries groups; more
// wrap ErrBound.
func (d *Directory) groups(s session, entry *ldap.Entry, id string) ([]string, error) {
	if !d.server.LDAPGroupsAsRoles {
		return []string{}, nil
	}
	if d.dynamic {
		return dynamicGroupNames(entry.GetEqualFoldAttributeValues(d.server.UserMemberOfAttribute), d.memberBase), nil
	}
	var err error
	if d.scheme == schemeSimple {
		err = s.conn.Bind(d.server.AuthUsername, d.bindPassword)
	} else {
		err = s.conn.UnauthenticatedBind("")
	}
	if err != nil {
		return nil, err
	}
	limit := 0
	if d.bounded {
		limit = CheckGroupEntries
	}
	filter := groupFilter(d.server.GroupObjectClass, d.server.GroupMemberAttribute, memberValue(d.server.GroupMemberFormat, entry.DN, id))
	result, err := d.search(s, d.groupBase, d.groupScope, limit, filter, []string{d.server.GroupIDAttribute})
	if d.bounded && (err == ldap.ErrSizeLimitExceeded || ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded)) {
		return nil, fmt.Errorf("%w: connection %q: group search: more than %d group entries", ErrBound, d.server.Name, CheckGroupEntries)
	}
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, group := range result.Entries {
		if name := first(group, d.server.GroupIDAttribute); name != "" {
			names = append(names, name)
		}
	}
	return sortedUnique(names), nil
}

// timeLimit is the server side time limit of a search, in seconds.
func (d *Directory) timeLimit() int {
	if d.server.ConnectionTimeoutSeconds <= 0 {
		return int(defaultTimeout / time.Second)
	}
	return d.server.ConnectionTimeoutSeconds
}

func (d *Directory) unavailable(ctx context.Context, step string, err error) error {
	var out error
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		out = fmt.Errorf("%w: connection %q: %s: %w (%v)", ErrUnavailable, d.server.Name, step, ctxErr, err)
	} else {
		out = fmt.Errorf("%w: connection %q: %s: %w", ErrUnavailable, d.server.Name, step, err)
	}
	// no directory answered: the connect failed (refused, timed out, no TLS
	// handshake), or the step ended without a result code of the protocol
	var answer *ldap.Error
	answered := errors.As(err, &answer) && answer.ResultCode < ldap.ErrorNetwork
	if step == "connect" || ctx.Err() == nil && !answered {
		return unreachable{out}
	}
	return out
}

// ErrUnreachable is wrapped by a failure in which no directory answered:
// the connection was refused, timed out, failed its TLS handshake, or broke
// before an answer of the protocol (ruling I25). It wraps ErrUnavailable as
// well.
var ErrUnreachable = errors.New("ldapauth: the directory could not be reached")

// unreachable marks an error of ErrUnavailable as ErrUnreachable and keeps
// its text.
type unreachable struct{ err error }

func (e unreachable) Error() string        { return e.err.Error() }
func (e unreachable) Unwrap() error        { return e.err }
func (e unreachable) Is(target error) bool { return target == ErrUnreachable }

// refusesPair tells a result of the user bind that refuses the pair from a
// failure of the directory.
func refusesPair(err error) bool {
	return ldap.IsErrorAnyOf(err,
		ldap.LDAPResultInvalidCredentials,
		ldap.LDAPResultInappropriateAuthentication,
		ldap.LDAPResultInsufficientAccessRights,
		ldap.LDAPResultUnwillingToPerform,
		ldap.LDAPResultConstraintViolation,
		ldap.LDAPResultInvalidDNSyntax,
		ldap.LDAPResultNoSuchObject,
	)
}

// pickID is the value of the id attribute that matches the typed name
// without regard to case, else its first value.
func pickID(values []string, typed string) string {
	for _, v := range values {
		if strings.EqualFold(v, typed) {
			return v
		}
	}
	if len(values) > 0 && values[0] != "" {
		return values[0]
	}
	return typed
}

func first(entry *ldap.Entry, attribute string) string {
	if attribute == "" {
		return ""
	}
	if values := entry.GetEqualFoldAttributeValues(attribute); len(values) > 0 {
		return values[0]
	}
	return ""
}

// Chain asks directories in their stored order (T12) and gives the first
// success. It refuses with ErrInvalidCredentials when any directory refused
// the pair or there is none, and with the joined errors of the directories
// when none of them could answer.
type Chain struct {
	dirs []*Directory
}

// NewChain orders the directories by their Order setting, stable for equal
// values. Nil directories are left out.
func NewChain(dirs ...*Directory) *Chain {
	sorted := make([]*Directory, 0, len(dirs))
	for _, d := range dirs {
		if d != nil {
			sorted = append(sorted, d)
		}
	}
	slices.SortStableFunc(sorted, func(a, b *Directory) int { return cmp.Compare(a.server.Order, b.server.Order) })
	return &Chain{dirs: sorted}
}

// String names the directories of the chain in their order.
func (c Chain) String() string {
	names := make([]string, len(c.dirs))
	for i, d := range c.dirs {
		names[i] = d.String()
	}
	return "ldap chain [" + strings.Join(names, ", ") + "]"
}

// Format prints String for every verb, as Directory.Format does.
func (c Chain) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// Login is Directory.Login over the chain.
func (c *Chain) Login(ctx context.Context, username, password string) (model.User, []string, error) {
	var failures []error
	refused := false
	for _, d := range c.dirs {
		user, groups, err := d.Login(ctx, username, password)
		if err == nil {
			return user, groups, nil
		}
		if err == ErrInvalidCredentials {
			refused = true
		} else {
			failures = append(failures, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	if refused || len(failures) == 0 {
		return model.User{}, nil, ErrInvalidCredentials
	}
	return model.User{}, nil, errors.Join(failures...)
}
