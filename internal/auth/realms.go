package auth

// RealmAuthorizing is the realm of the roles of every user in the old
// product. This server maps roles for every realm, so the id has no further
// effect here; it is read without a warning.
const RealmAuthorizing = "NexusAuthorizingRealm"

// RealmDefaultRole gives the default role of the snapshot to every signed in
// subject while it is in the stored order, and never to the anonymous one
// (lab research 05-identity.md, section Roles). Its place in the order does
// not matter, as it checks no credential.
const RealmDefaultRole = "DefaultRole"

// Realm is a realm id of the stored order that this server builds.
type Realm struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Realms are the realms this server builds. Another id of the stored order
// signs nobody in and is named in a warning at each load (decision N16).
// `realms set` takes these ids only, and GET /v1/security/realms/available
// lists them.
var Realms = []Realm{
	{RealmLocal, "Local users"},
	{RealmAuthorizing, "Local authorization"},
	{RealmLDAP, "LDAP directories"},
	{RealmNpm, "npm tokens"},
	{RealmDocker, "Docker bearer tokens"},
	{RealmUserToken, "User tokens"},
	{RealmDefaultRole, "Default role"},
}

// RealmByID is the realm of an id this server builds.
func RealmByID(id string) (Realm, bool) {
	for _, r := range Realms {
		if r.ID == id {
			return r, true
		}
	}
	return Realm{}, false
}

// TokenRealm is the realm that lets the tokens of a kind in.
func TokenRealm(kind string) string { return tokenRealms[kind] }
