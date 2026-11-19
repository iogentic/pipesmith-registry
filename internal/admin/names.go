package admin

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/iogentic/pipesmith-registry/internal/authz"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The pattern of every name and of a role id is the one of store add, with a
// bound of 256 characters. A user id may also hold @ and +, so that a mail
// address is an id.
var (
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$`)
	userIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,255}$`)
)

// NameField is the field of a request body that carries the name or id of
// a record of the kind: id for a role, userId for a user, name otherwise.
func NameField(kind Kind) string {
	switch kind {
	case KindRole:
		return "id"
	case KindUser:
		return "userId"
	}
	return "name"
}

// word is "id" for the kinds whose records have an id, else "name".
func word(kind Kind) string {
	if kind == KindRole || kind == KindUser {
		return "id"
	}
	return "name"
}

// CheckName refuses a name or id outside the pattern of its kind, with 400
// at the field that carries it.
func CheckName(kind Kind, name string) error {
	pattern, chars := namePattern, "letters, digits, dots, hyphens and underscores"
	if kind == KindUser {
		pattern, chars = userIDPattern, "letters, digits, dots, hyphens, underscores, @ and +"
	}
	if pattern.MatchString(name) {
		return nil
	}
	return &Refusal{Code: CodeNameInvalid, Field: NameField(kind), Message: fmt.Sprintf(
		"the %s %s %q is not allowed; use %s, starting with a letter or a digit, at most 256 characters",
		kind.noun(), word(kind), name, chars)}
}

// Exists reports whether the snapshot holds a record of the kind with the
// name or id: a repository, a blob store, an LDAP connection, a user, or a
// role, where a built in role counts. Names are compared as they are. The
// snapshot holds no cleanup policy and no routing rule; for those kinds it
// reports false, and the caller compares with the names it has.
func Exists(s *model.Snapshot, kind Kind, name string) bool {
	switch kind {
	case KindRepository:
		return slices.ContainsFunc(s.Repositories, func(r model.Repository) bool { return r.Name == name })
	case KindBlobStore:
		return slices.ContainsFunc(s.BlobStores, func(b model.BlobStore) bool { return b.Name == name })
	case KindLDAPConnection:
		return slices.ContainsFunc(s.LDAP, func(l model.LDAPServer) bool { return l.Name == name })
	case KindUser:
		return slices.ContainsFunc(s.Users, func(u model.User) bool { return u.ID == name })
	case KindRole:
		return len(authz.NewResolver(s).Roles([]string{name})) > 0
	}
	return false
}

// CheckCreate refuses the create of a record whose name or id is outside
// the pattern of its kind (400) or exists (409).
func CheckCreate(kind Kind, name string, exists bool) error {
	if err := CheckName(kind, name); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	what := "named " + name
	if word(kind) == "id" {
		what = "with the id " + name
	}
	return &Refusal{Code: CodeNameExists, Message: fmt.Sprintf("%s %s %s exists", kind.article(), kind.noun(), what)}
}

// CheckRename refuses a save whose body names another name or id than the
// stored one, with 400 at the name field. sent is nil when the body names
// none. The name of an LDAP connection may change; CheckLDAPSave checks its
// save.
func CheckRename(kind Kind, stored string, sent *string) error {
	if sent == nil || *sent == stored || kind == KindLDAPConnection {
		return nil
	}
	return &Refusal{Code: CodeNameChanged, Field: NameField(kind), Message: fmt.Sprintf(
		"the %s of the %s %s cannot change", word(kind), kind.noun(), stored)}
}

// CheckLDAPSave checks the id and the name of the body of a save of the
// stored LDAP connection (ruling R2b, which replaces R2 for this kind). The
// id is the fixed key: another id is refused with 400 at id. The name may
// change: a new name outside the pattern is refused with 400 at name, a new
// name that another connection of the snapshot holds with 409. A nil id or
// name, and the stored one, are no change. The refusals travel together.
func CheckLDAPSave(s *model.Snapshot, stored model.LDAPServer, sentID, sentName *string) error {
	var list Refusals
	if sentID != nil && *sentID != stored.ID {
		list = append(list, &Refusal{Code: CodeNameChanged, Field: "id", Message: fmt.Sprintf(
			"the id of the LDAP connection %s cannot change", stored.Name)})
	}
	if sentName != nil && *sentName != stored.Name {
		taken := slices.ContainsFunc(s.LDAP, func(l model.LDAPServer) bool {
			return l.ID != stored.ID && l.Name == *sentName
		})
		if err := CheckCreate(KindLDAPConnection, *sentName, taken); err != nil {
			list = append(list, err.(*Refusal))
		}
	}
	return list.err()
}
