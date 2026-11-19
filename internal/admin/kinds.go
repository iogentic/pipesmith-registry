package admin

// Kind is a kind of record of a contract body.
type Kind string

// Kinds of record.
const (
	KindRepository     Kind = "repository"
	KindBlobStore      Kind = "blob-store"
	KindCleanupPolicy  Kind = "cleanup-policy"
	KindRoutingRule    Kind = "routing-rule"
	KindLDAPConnection Kind = "ldap-connection"
	KindRole           Kind = "role"
	KindUser           Kind = "user"
	KindLDAPLoginCheck Kind = "ldap-login-check" // the body of the sign in check of an LDAP connection
	KindAccount        Kind = "account"          // the body of the own password change
	KindToken          Kind = "token"
)

// nouns name the kinds in messages.
var nouns = map[Kind]string{
	KindRepository:     "repository",
	KindBlobStore:      "blob store",
	KindCleanupPolicy:  "cleanup policy",
	KindRoutingRule:    "routing rule",
	KindLDAPConnection: "LDAP connection",
	KindRole:           "role",
	KindUser:           "user",
	KindLDAPLoginCheck: "LDAP sign in check",
	KindAccount:        "account",
	KindToken:          "token",
}

func (k Kind) noun() string {
	if n, ok := nouns[k]; ok {
		return n
	}
	return string(k)
}

// article is the indefinite article of the noun of the kind.
func (k Kind) article() string {
	if k == KindLDAPConnection || k == KindLDAPLoginCheck || k == KindAccount {
		return "an"
	}
	return "a"
}
