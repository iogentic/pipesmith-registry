// Package model holds the plain records of the identity and configuration
// state, and the one interface between the server and the identity code.
// The import writes these records, the server and the identity code read
// them. The package imports nothing of this repository.
package model

import (
	"crypto/hmac"
	"crypto/sha256"
	"net/http"
	"time"
)

// User is a local user, or an external user known through a role mapping.
type User struct {
	ID        string   `json:"userId"`
	FirstName string   `json:"firstName"`
	LastName  string   `json:"lastName"`
	Email     string   `json:"emailAddress"`
	Status    string   `json:"status"` // active, locked, disabled, changepassword
	Source    string   `json:"source"` // "default" for local users, "LDAP" for directory users
	Roles     []string `json:"roles"`
	// PasswordHash is the stored form of the old product. It never leaves
	// the process: it has no JSON key and String leaves it out.
	PasswordHash string `json:"-" audit:"secret"`
	// Entry names the directory entry of a directory user: its entryUUID or
	// objectGUID, else its DN. Tokens are bound to it (review 3-40). It is
	// no stored field and has no JSON key.
	Entry string `json:"-"`
}

// String names the user without the hash, so %v and %+v cannot print it.
func (u User) String() string { return "user " + u.ID + " (" + u.Source + ")" }

// GoString is String, so %#v cannot print the hash either.
func (u User) GoString() string { return u.String() }

// Role is a role of the old product. A role whose ID equals the name of an
// external group maps that group.
type Role struct {
	ID          string   `json:"id"`
	Source      string   `json:"source"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	ReadOnly    bool     `json:"readOnly"`
	Privileges  []string `json:"privileges"`
	Roles       []string `json:"roles"`
}

// Privilege is a stored privilege. Properties hold the fields of its type:
// domain and actions; format, repository and actions; contentSelector;
// pattern; scriptName.
type Privilege struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Description string            `json:"description"`
	ReadOnly    bool              `json:"readOnly"`
	Properties  map[string]string `json:"properties"`
}

// RoleMapping gives the users of one source their roles. Local users carry
// their roles in User.Roles; this holds the other sources.
type RoleMapping struct {
	UserID string   `json:"userId"`
	Source string   `json:"source"`
	Roles  []string `json:"roles"`
}

// LDAPServer is one directory connection in the read shape of the contract.
// The bind password is not part of it: the operator enters it at start.
type LDAPServer struct {
	ID                          string `json:"id"`
	Name                        string `json:"name"`
	Order                       int    `json:"order"`
	Protocol                    string `json:"protocol"` // ldap, ldaps
	UseTrustStore               bool   `json:"useTrustStore"`
	Host                        string `json:"host"`
	Port                        int    `json:"port"`
	SearchBase                  string `json:"searchBase"`
	AuthScheme                  string `json:"authScheme"` // NONE, SIMPLE, DIGEST_MD5, CRAM_MD5
	AuthRealm                   string `json:"authRealm"`
	AuthUsername                string `json:"authUsername"`
	ConnectionTimeoutSeconds    int    `json:"connectionTimeoutSeconds"`
	ConnectionRetryDelaySeconds int    `json:"connectionRetryDelaySeconds"`
	MaxIncidentsCount           int    `json:"maxIncidentsCount"`
	UserBaseDN                  string `json:"userBaseDn"`
	UserSubtree                 bool   `json:"userSubtree"`
	UserObjectClass             string `json:"userObjectClass"`
	UserLDAPFilter              string `json:"userLdapFilter"`
	UserIDAttribute             string `json:"userIdAttribute"`
	UserRealNameAttribute       string `json:"userRealNameAttribute"`
	UserEmailAddressAttribute   string `json:"userEmailAddressAttribute"`
	UserPasswordAttribute       string `json:"userPasswordAttribute"`
	LDAPGroupsAsRoles           bool   `json:"ldapGroupsAsRoles"`
	GroupType                   string `json:"groupType"` // static, dynamic
	GroupBaseDN                 string `json:"groupBaseDn"`
	GroupSubtree                bool   `json:"groupSubtree"`
	GroupObjectClass            string `json:"groupObjectClass"`
	GroupIDAttribute            string `json:"groupIdAttribute"`
	GroupMemberAttribute        string `json:"groupMemberAttribute"`
	GroupMemberFormat           string `json:"groupMemberFormat"`
	UserMemberOfAttribute       string `json:"userMemberOfAttribute"`
	// CAFile names a file of PEM certificates that an ldaps connection is
	// verified against besides the system roots (decision N18 of the phase 3
	// plan). The old product has no such field; `ldap ca` sets it.
	CAFile string `json:"caFile,omitempty"`
}

// Anonymous is the anonymous access setting.
type Anonymous struct {
	Enabled   bool   `json:"enabled"`
	UserID    string `json:"userId"`
	RealmName string `json:"realmName"`
}

// SoftQuota is the soft quota of a blob store.
type SoftQuota struct {
	Type  string `json:"type"` // spaceRemainingQuota, spaceUsedQuota
	Limit int64  `json:"limit"`
}

// BlobStore is a blob store definition of the old product. Path is the path
// on the old host; the local copy is a store of config.json.
type BlobStore struct {
	Name      string     `json:"name"`
	Type      string     `json:"type"` // File, S3, Group
	Path      string     `json:"path"`
	SoftQuota *SoftQuota `json:"softQuota"`
}

// Repository is a repository definition with its stored attributes, in the
// shape of the old product's settings (storage, group, proxy, maven, docker
// and the others). Secret fields are removed before a record is written.
type Repository struct {
	Name       string         `json:"name"`
	Format     string         `json:"format"` // maven2, npm, docker, raw, ...
	Type       string         `json:"type"`   // hosted, proxy, group
	Online     bool           `json:"online"`
	Attributes map[string]any `json:"attributes"`
}

// RoutingRule is a routing rule of the old product (decision 5.15 of the
// phase 5 plan): its mode, ALLOW or BLOCK as stored, decides over the
// request paths its regular expressions match.
type RoutingRule struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Mode        string   `json:"mode"`
	Matchers    []string `json:"matchers"`
}

// Routing is the routing rules and the rule each repository names. It is
// no part of Snapshot: state.sqlite keeps both in tables of their own
// (decision P5-1 of the phase 5 session).
type Routing struct {
	Rules []RoutingRule `json:"rules"`
	// Links maps the name of a repository to the name of its rule.
	Links map[string]string `json:"links"`
}

// Snapshot is the whole state at one point in time. It is never changed
// after it is handed out; a reload builds a new one.
type Snapshot struct {
	Users        []User
	Roles        []Role
	Privileges   []Privilege
	RoleMappings []RoleMapping
	LDAP         []LDAPServer
	Realms       []string // active realm ids in their order
	Anonymous    Anonymous
	// DefaultRole is the role the realm DefaultRole gives every signed in
	// subject while it is in Realms; empty for none.
	DefaultRole  string
	BlobStores   []BlobStore
	Repositories []Repository
	// The records of phase 6, each row with its origin.
	CleanupPolicies []CleanupPolicy
	Tasks           []Task
	Capabilities    []Capability
}

// The origins of a cleanup policy, a task or a capability (ruling S2 of
// leg 6-state): a row of the import, or one an administrator made.
const (
	OriginImport = "import"
	OriginLocal  = "local"
)

// CleanupPolicy is a cleanup policy. Criteria hold the stored names and
// values, known to this server or not (A9).
type CleanupPolicy struct {
	Name     string         `json:"name"`
	Format   string         `json:"format"`
	Notes    string         `json:"notes"`
	Criteria map[string]any `json:"criteria"`
	Origin   string         `json:"origin"`
}

// TaskSchedule is the schedule of a task in the shape of tasks.Schedule:
// a type and the fields that type uses. StartAt is zero when it is not
// given. Cron holds the expression as stored, also one the cron parser
// refuses.
type TaskSchedule struct {
	Type      string    `json:"type"`
	StartAt   time.Time `json:"startAt,omitzero"`
	WeekDays  []string  `json:"weekDays,omitempty"`
	MonthDays []int     `json:"monthDays,omitempty"`
	Cron      string    `json:"cron,omitempty"`
}

// Task is a scheduled task. A property whose key holds a secret word is
// never part of Properties (A11). ImportedState is the state the old row
// held, nil for a task of origin local; an imported task arrives with
// Enabled false (I12).
type Task struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	TypeID        string            `json:"typeId"`
	Enabled       bool              `json:"enabled"`
	Schedule      TaskSchedule      `json:"schedule"`
	Properties    map[string]string `json:"properties"`
	ImportedState map[string]string `json:"importedState"`
	Origin        string            `json:"origin"`
}

// Capability is a capability of the old product. A property whose key
// holds a secret word is never part of Properties (A11).
type Capability struct {
	ID         string            `json:"id"`
	TypeID     string            `json:"typeId"`
	Enabled    bool              `json:"enabled"`
	Notes      string            `json:"notes"`
	Properties map[string]string `json:"properties"`
	Origin     string            `json:"origin"`
}

// Token is a token this server issued, as a row of the tokens table holds
// it: the public half in clear, the secret half only as its SHA-256
// (decisions N9 and N10 of the phase 3 plan). Docker bearer tokens live in
// memory and are no rows (N11).
type Token struct {
	Public   string   // the public half, the key of the row
	Kind     string   // "npm" or "user"
	UserID   string   // the user the token signs in
	Source   string   // the source of that user: "default" or "LDAP"
	Digest   [32]byte `audit:"secret"` // SHA-256 of the secret half
	Created  time.Time
	LastUsed time.Time // zero while the token was never used
}

// String names the token by its public half and owner, without the digest.
func (t Token) String() string {
	return "token " + t.Kind + " " + t.Public + " of " + t.UserID + " (" + t.Source + ")"
}

// GoString is String.
func (t Token) GoString() string { return t.String() }

// IssuedToken is a token in its wire form, the one time it is shown.
type IssuedToken struct {
	Text      string
	ExpiresIn int // seconds; 0 for a token that lives until it is revoked
	IssuedAt  time.Time
}

// Subject is a signed in caller.
type Subject struct {
	User        User // PasswordHash is always empty here
	Realm       string
	Permissions []string // expanded permission strings of the old product
	Session     string   // the session id for a cookie session, empty for Basic
}

// Security is what the server needs from the identity code. A nil Security
// means security is off and the server behaves as in port step 2.
type Security interface {
	// Subject resolves the caller from the session cookie, else from Basic
	// credentials. It returns nil and no error for an anonymous caller,
	// ErrUnauthorized for credentials that do not verify and ErrThrottled
	// while sign in is slowed down for this caller.
	Subject(r *http.Request) (*Subject, error)
	// Permits reports whether the subject holds the permission, by the
	// wildcard rule of the old product. A nil subject is the anonymous user
	// when anonymous access is on, and holds nothing otherwise.
	Permits(s *Subject, permission string) bool
	// AnonymousEnabled reports whether anonymous access is on.
	AnonymousEnabled() bool
	// CSRF reports whether a POST, PUT or DELETE carries the request token
	// of its session. Requests with Basic credentials need none.
	CSRF(r *http.Request, s *Subject) bool
	// SessionHandler serves POST, GET and DELETE /api/session.
	SessionHandler() http.Handler
	// Snapshot is the state the subjects were resolved against.
	Snapshot() *Snapshot
	// DockerAccess is how a Docker repository asks for credentials and
	// whether it may be pulled without any (decision N12 of the phase 3
	// plan).
	DockerAccess(repository string) DockerAccess
	// DockerToken answers the token route of the Docker flow below a
	// repository: a bearer token for the Basic credentials of r, or for the
	// anonymous subject when the repository may be pulled anonymously. The
	// scope a client asks for decides nothing; the rights are checked at
	// each request.
	DockerToken(r *http.Request, repository string) (IssuedToken, error)
	// NpmLogin signs a user name and password in and issues an npm token.
	NpmLogin(r *http.Request, user, password string) (IssuedToken, error)
	// RevokeToken revokes a token the subject holds; ErrNoToken when it holds
	// no token of that text.
	RevokeToken(s *Subject, token string) error
}

// DockerAccess is the sign in a Docker repository asks for (decision N12 of
// the phase 3 plan).
type DockerAccess struct {
	// Bearer means the challenge names the token route; else it is Basic.
	Bearer bool
	// AnonymousPull means the realm of Docker tokens, anonymous access and
	// the attribute of the repository all let a pull in without credentials.
	AnonymousPull bool
}

// Error is an error of Security with the status the server answers.
type Error struct {
	Status     int
	Message    string
	RetryAfter int // seconds, for 429
}

func (e *Error) Error() string { return e.Message }

var (
	// ErrUnauthorized is a credential that does not verify.
	ErrUnauthorized = &Error{Status: http.StatusUnauthorized, Message: "authentication required"}
	// ErrThrottled is returned while sign in is slowed down; the caller
	// copies RetryAfter from the error it gets, which may be a new *Error.
	ErrThrottled = &Error{Status: http.StatusTooManyRequests, Message: "too many failed sign ins"}
	// ErrOldToken is a token in the shape of the server this one replaces,
	// whose tokens are not taken over (decision N9 of the phase 3 plan).
	ErrOldToken = &Error{Status: http.StatusUnauthorized,
		Message: "this token comes from the server this one replaces and is not accepted: this server issues tokens new, sign in again"}
	// ErrNoToken is a token to revoke that the subject does not hold.
	ErrNoToken = &Error{Status: http.StatusNotFound, Message: "no such token"}
)

// ImportedKey is a key of the old product taken over by an import with
// secrets (decision K4 of docs/plans/secrets-import.md): no value of the key,
// only a digest under the install key of the SHA-256 of the key.
type ImportedKey struct {
	Kind     string   // "npm", "docker" or "nuget"
	UserID   string   // the user the key signs in
	Source   string   // the source of that user: "default" or "LDAP"
	Digest   [32]byte `audit:"secret"` // HMAC-SHA-256 under the install key of the SHA-256 of the key
	Created  time.Time
	LastUsed time.Time // zero while the key was not used on this server
}

// String names the key by its kind and owner, without the digest.
func (k ImportedKey) String() string {
	return "imported key " + k.Kind + " of " + k.UserID + " (" + k.Source + ")"
}

// GoString is String.
func (k ImportedKey) GoString() string { return k.String() }

// ImportedDigest is the digest an imported key is stored and found by:
// HMAC-SHA-256 under the install key of the SHA-256 of the key.
func ImportedDigest(installKey []byte, sum [32]byte) [32]byte {
	m := hmac.New(sha256.New, installKey)
	m.Write(sum[:])
	var d [32]byte
	copy(d[:], m.Sum(nil))
	return d
}
