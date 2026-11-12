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
)

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
	caPEM [][]byte
	now   func() time.Time
}

// Option changes how New builds a directory.
type Option func(*options)

// WithCACertificates adds the PEM certificates to the system roots that an
// ldaps connection is verified against (N18). There is no option that skips
// the verification.
func WithCACertificates(pem []byte) Option {
	return func(o *options) { o.caPEM = append(o.caPEM, pem) }
}

// withClock replaces the clock of the incident counter in tests.
func withClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// Directory is one directory connection. Login is safe for concurrent use;
// each sign in opens its own connection.
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
	d := &Directory{server: server}

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
			for _, a := range [][2]string{
				{"group id attribute", server.GroupIDAttribute},
				{"group member attribute", server.GroupMemberAttribute},
			} {
				if !attributeName.MatchString(a[1]) {
					return refuse("%s %q is not an attribute name", a[0], a[1])
				}
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
// error that wraps ErrUnavailable. An empty password is refused before any
// connection (N17).
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
	user, groups, err := d.login(ctx, username, password)
	switch {
	case err == nil || err == ErrInvalidCredentials:
		d.breaker.success()
	case ctx.Err() != nil:
		// The caller gave up; that is no incident of the directory.
	default:
		d.breaker.failure()
	}
	if err != nil {
		return model.User{}, nil, err
	}
	return user, groups, nil
}

func (d *Directory) login(ctx context.Context, username, password string) (model.User, []string, error) {
	conn, err := d.dial(ctx)
	if err != nil {
		return model.User{}, nil, d.unavailable(ctx, "connect", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	conn.SetTimeout(d.timeout)

	if d.scheme == schemeSimple {
		if err := conn.Bind(d.server.AuthUsername, d.bindPassword); err != nil {
			return model.User{}, nil, d.unavailable(ctx, "service bind", err)
		}
	}
	entry, err := d.findUser(conn, username)
	if err == ErrInvalidCredentials {
		return model.User{}, nil, err
	}
	if err != nil {
		return model.User{}, nil, d.unavailable(ctx, "user search", err)
	}
	if err := conn.Bind(entry.DN, password); err != nil {
		if refusesPair(err) {
			return model.User{}, nil, ErrInvalidCredentials
		}
		return model.User{}, nil, d.unavailable(ctx, "user bind", err)
	}
	id := pickID(entry.GetEqualFoldAttributeValues(d.server.UserIDAttribute), username)
	groups, err := d.groups(conn, entry, id)
	if err != nil {
		return model.User{}, nil, d.unavailable(ctx, "group search", err)
	}
	return model.User{
		ID:        id,
		FirstName: first(entry, d.server.UserRealNameAttribute),
		Email:     first(entry, d.server.UserEmailAddressAttribute),
		Status:    "active",
		Source:    "LDAP",
		Roles:     []string{},
	}, groups, nil
}

func (d *Directory) dial(ctx context.Context) (*ldap.Conn, error) {
	dialer := &net.Dialer{Timeout: d.timeout}
	var c net.Conn
	var err error
	if d.tlsConfig != nil {
		c, err = (&tls.Dialer{NetDialer: dialer, Config: d.tlsConfig}).DialContext(ctx, "tcp", d.addr)
	} else {
		c, err = dialer.DialContext(ctx, "tcp", d.addr)
	}
	if err != nil {
		return nil, err
	}
	conn := ldap.NewConn(c, d.tlsConfig != nil)
	conn.Start()
	return conn, nil
}

// findUser gives the one entry of the user name, or ErrInvalidCredentials
// for none or more than one. Referrals are not followed (N17).
func (d *Directory) findUser(conn *ldap.Conn, username string) (*ldap.Entry, error) {
	attributes := []string{d.server.UserIDAttribute}
	for _, a := range []string{d.server.UserRealNameAttribute, d.server.UserEmailAddressAttribute} {
		if a != "" {
			attributes = append(attributes, a)
		}
	}
	if d.dynamic {
		attributes = append(attributes, d.server.UserMemberOfAttribute)
	}
	result, err := conn.Search(ldap.NewSearchRequest(d.userBase, d.userScope, ldap.NeverDerefAliases, 2, d.timeLimit(), false,
		userFilter(d.server.UserObjectClass, d.server.UserIDAttribute, d.server.UserLDAPFilter, username), attributes, nil))
	if ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
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

// groups gives the group names of a signed in entry. Static groups are read
// as the service again, since the user bind changed who the connection is.
func (d *Directory) groups(conn *ldap.Conn, entry *ldap.Entry, id string) ([]string, error) {
	if !d.server.LDAPGroupsAsRoles {
		return []string{}, nil
	}
	if d.dynamic {
		return dynamicGroupNames(entry.GetEqualFoldAttributeValues(d.server.UserMemberOfAttribute)), nil
	}
	var err error
	if d.scheme == schemeSimple {
		err = conn.Bind(d.server.AuthUsername, d.bindPassword)
	} else {
		err = conn.UnauthenticatedBind("")
	}
	if err != nil {
		return nil, err
	}
	filter := groupFilter(d.server.GroupObjectClass, d.server.GroupMemberAttribute, memberValue(d.server.GroupMemberFormat, entry.DN, id))
	result, err := conn.Search(ldap.NewSearchRequest(d.groupBase, d.groupScope, ldap.NeverDerefAliases, 0, d.timeLimit(), false,
		filter, []string{d.server.GroupIDAttribute}, nil))
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
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return fmt.Errorf("%w: connection %q: %s: %w (%v)", ErrUnavailable, d.server.Name, step, ctxErr, err)
	}
	return fmt.Errorf("%w: connection %q: %s: %w", ErrUnavailable, d.server.Name, step, err)
}

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
