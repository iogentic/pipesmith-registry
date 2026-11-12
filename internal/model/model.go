// Package model holds the plain records of the identity and configuration
// state, and the one interface between the server and the identity code.
// The import writes these records, the server and the identity code read
// them. The package imports nothing of this repository.
package model

import "net/http"

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
	PasswordHash string `json:"-"`
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
	BlobStores   []BlobStore
	Repositories []Repository
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
)
