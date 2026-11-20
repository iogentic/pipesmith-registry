package admin

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/authz"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The rules and the write of the identity changes (leg 6-identity, decision
// A1): users, roles, anonymous access and realms. The identity commands of
// internal/cli and the REST writes of the area security call them.

// IdentityStore is what an identity change reads and writes: state.sqlite
// of the data directory, and the credential cache of a running service.
// internal/cli fulfils it with its store; server.Options carries it.
type IdentityStore interface {
	// Load reads every record of the state.
	Load() (*model.Snapshot, error)
	// Update replaces the rows of the kinds with what fn makes of the
	// records, in one transaction that raises the generation (state.Update).
	Update(kinds []state.Kind, fn func(s *model.Snapshot) error) error
	// DataDir is the data directory of the state file.
	DataDir() string
	// ResetCredentials empties the credential cache of the running
	// service; a store of a command without a service does nothing.
	ResetCredentials()
	// MarkStale makes the running service load the state again at its next
	// request (ruling I13); a store of a command does nothing.
	MarkStale()
}

// Change applies one identity change and is the one place every identity
// change of the commands and of REST passes. fn changes the records read in
// the transaction of Update. The change is refused when it leaves no active
// local user with the full permission set (A10, ruling I3); then nothing is
// written. After a write the credential cache is emptied (ruling I8) and the
// view of the service is marked stale, so the next request loads the state
// the write raised the generation of (I13). Change returns the records after the change.
func Change(store IdentityStore, kinds []state.Kind, fn func(s *model.Snapshot) error) (*model.Snapshot, error) {
	var after *model.Snapshot
	err := store.Update(kinds, func(s *model.Snapshot) error {
		before := cloneIdentity(s)
		if err := fn(s); err != nil {
			return err
		}
		if err := CheckAdministrators(before, s); err != nil {
			return err
		}
		after = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	committed(store)
	return after, nil
}

// CreateUser stores a new local user with the hash of its password: the one
// path of POST /v1/security/users and of the command user add. It refuses
// what CheckUserCreate refuses before the password is hashed, and checks
// again in the transaction. holds is the permission check of the acting
// subject (ruling I9); nil for a command, which acts with the file rights of
// the data directory. CreateUser returns the records after the change.
func CreateUser(store IdentityStore, user model.User, password string, holds func(permission string) bool) (*model.Snapshot, error) {
	check := func(s *model.Snapshot) error {
		if err := CheckUserCreate(s, user, password); err != nil {
			return err
		}
		if holds == nil {
			return nil
		}
		return CheckGrant(s, "the user "+user.ID, user.Roles, holds)
	}
	snap, err := store.Load()
	if err != nil {
		return nil, err
	}
	if err := check(snap); err != nil {
		return nil, err
	}
	if user.PasswordHash, err = auth.HashPassword(password); err != nil {
		return nil, err
	}
	return Change(store, []state.Kind{state.KindUsers}, func(s *model.Snapshot) error {
		if err := check(s); err != nil {
			return err
		}
		s.Users = append(s.Users, user)
		return nil
	})
}

// committed is the end of every change that was written: Change,
// ChangeRouting and Act call it. It empties the credential cache (ruling I8) and
// marks the view of the service stale (I13), so the next request loads the
// state the write raised the generation of.
func committed(store IdentityStore) {
	store.ResetCredentials()
	store.MarkStale()
}

// cloneIdentity copies the snapshot with its own lists of users, roles,
// privileges and mappings, so that a change in place leaves the copy as it
// was.
func cloneIdentity(s *model.Snapshot) *model.Snapshot {
	c := *s
	c.Users = slices.Clone(s.Users)
	for i := range c.Users {
		c.Users[i].Roles = slices.Clone(c.Users[i].Roles)
	}
	c.Roles = slices.Clone(s.Roles)
	for i := range c.Roles {
		c.Roles[i].Privileges = slices.Clone(c.Roles[i].Privileges)
		c.Roles[i].Roles = slices.Clone(c.Roles[i].Roles)
	}
	c.Privileges = slices.Clone(s.Privileges)
	c.RoleMappings = slices.Clone(s.RoleMappings)
	for i := range c.RoleMappings {
		c.RoleMappings[i].Roles = slices.Clone(c.RoleMappings[i].Roles)
	}
	c.Realms = slices.Clone(s.Realms)
	return &c
}

// Users

// LocalUser is the local user of the id in s, as a pointer into s, or the
// refusal not-found.
func LocalUser(s *model.Snapshot, id string) (*model.User, error) {
	for i := range s.Users {
		if u := &s.Users[i]; u.ID == id && u.Source == sourceLocal {
			return u, nil
		}
	}
	return nil, &Refusal{Code: CodeNotFound, Message: fmt.Sprintf("no local user %s", id)}
}

// CheckNewUser checks a new local user before its password: the id of the
// user pattern (ruling I1), an id that no user of any source and no role
// mapping of the local source holds (409), and its roles (CheckUserRoles).
func CheckNewUser(s *model.Snapshot, id string, roles []string) error {
	if err := CheckName(KindUser, id); err != nil {
		return err
	}
	if slices.ContainsFunc(s.Users, func(u model.User) bool { return u.ID == id }) {
		return &Refusal{Code: CodeNameExists, Message: fmt.Sprintf("the user %s exists already", id)}
	}
	if slices.ContainsFunc(s.RoleMappings, func(m model.RoleMapping) bool { return m.UserID == id && m.Source == sourceLocal }) {
		return &Refusal{Code: CodeNameExists, Message: fmt.Sprintf("the user %s has roles of the local source already", id)}
	}
	return CheckUserRoles(s, roles, nil)
}

// CheckUserCreate checks the create of a local user (ruling I1):
// CheckNewUser, the status (Q7) and the password (I5). The refusals of 400
// travel together in the order roles, status, password.
func CheckUserCreate(s *model.Snapshot, u model.User, password string) error {
	first := CheckNewUser(s, u.ID, u.Roles)
	var r *Refusal
	if errors.As(first, &r) && r.Code != CodeRoleMissing {
		return first
	}
	return errors.Join(first, CheckUserStatus(u.Status, nil), CheckPassword(password))
}

// CheckUserRoles refuses a role that is neither stored nor built in, with
// 400 at roles. A role in held, the roles the stored record holds, is never
// refused (ruling R6).
func CheckUserRoles(s *model.Snapshot, roles, held []string) error {
	resolver := authz.NewResolver(s)
	var list Refusals
	for _, r := range roles {
		if !slices.Contains(held, r) && !slices.Contains(resolver.Roles([]string{r}), r) {
			list = append(list, &Refusal{Code: CodeRoleMissing, Field: rolesField, Message: fmt.Sprintf(
				"no role %s; the roles are the stored ones and the built in nx-admin and nx-anonymous", r)})
		}
	}
	return list.err()
}

// CheckPassword checks a new password (ruling I5): not empty, at most
// auth.MaxPasswordBytes bytes, and no control character. There is no least
// length.
func CheckPassword(password string) error {
	words := ""
	switch {
	case password == "":
		words = "the password is empty"
	case len(password) > auth.MaxPasswordBytes:
		words = fmt.Sprintf("a password has 1 to %d bytes", auth.MaxPasswordBytes)
	case strings.ContainsFunc(password, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		words = "the password holds a control character"
	default:
		return nil
	}
	return &Refusal{Code: CodePasswordInvalid, Field: "password", Message: words}
}

// SaveUser applies the body of a save to the stored user and gives the
// record to store (ruling I7). The id stays (400 at userId). A local user
// takes names, mail, status (Q7, R6) and roles from the body. A user of
// another source keeps names, mail and status; a change of them is refused
// with 409 (Q6), and it takes its roles. A save of the own user (own) keeps
// its roles unless the subject may administer users (administers). Roles
// are checked with CheckUserRoles against those the user holds.
func SaveUser(s *model.Snapshot, stored, body model.User, own, administers bool) (model.User, error) {
	var sent *string
	if body.ID != "" {
		sent = &body.ID
	}
	if err := CheckRename(KindUser, stored.ID, sent); err != nil {
		return stored, err
	}
	saved := stored
	var errs []error
	if stored.Source == sourceLocal {
		errs = append(errs, CheckUserStatus(body.Status, &stored))
		saved.FirstName, saved.LastName, saved.Email, saved.Status = body.FirstName, body.LastName, body.Email, body.Status
	} else if body.FirstName != stored.FirstName || body.LastName != stored.LastName || body.Email != stored.Email ||
		body.Status != stored.Status {
		return stored, CheckAccountSave(stored)
	}
	if !own || administers {
		errs = append(errs, CheckUserRoles(s, body.Roles, stored.Roles))
		saved.Roles = slices.Clone(body.Roles)
	}
	if saved.Roles == nil {
		saved.Roles = []string{}
	}
	if err := errors.Join(errs...); err != nil {
		return stored, err
	}
	return saved, nil
}

// UserSource is the source of the user a delete names with its query realm
// (ruling I12): the realm id the interface sends, NexusAuthenticatingRealm
// for the local source and LdapRealm for the directory source, or a source
// name; the local source when it is empty. Any other value is refused with
// 400 at realm.
func UserSource(realm string) (string, error) {
	switch realm {
	case "", auth.RealmLocal, auth.SourceLocal:
		return auth.SourceLocal, nil
	case auth.RealmLDAP, auth.SourceLDAP:
		return auth.SourceLDAP, nil
	}
	return "", &Refusal{Code: CodeRealmInvalid, Field: "realm", Message: fmt.Sprintf(
		"the realm %q names no source of users; use %s or %s", realm, auth.RealmLocal, auth.RealmLDAP)}
}

// SignInUser is the local user of id that signs in with a password: active
// and with a stored form. Nil for any other id (review N5).
func SignInUser(s *model.Snapshot, id string) *model.User {
	for i := range s.Users {
		if u := &s.Users[i]; u.ID == id && SignsInWithPassword(*u) {
			return u
		}
	}
	return nil
}

// SignsInWithPassword reports a local user that signs in with a password:
// active and with a stored form.
func SignsInWithPassword(u model.User) bool {
	return u.Source == sourceLocal && u.Status == statusActive && u.PasswordHash != ""
}

// Anonymous access

// CheckAnonymous checks a save of the anonymous access setting (contract
// S25): userId of the user pattern (400 at userId), realmName a realm this
// server builds (400 at realmName). A value the stored setting holds is
// never refused (ruling R6).
func CheckAnonymous(a, stored model.Anonymous) error {
	var list Refusals
	if a.UserID != stored.UserID {
		if err := CheckName(KindUser, a.UserID); err != nil {
			list = append(list, err.(*Refusal))
		}
	}
	if _, known := auth.RealmByID(a.RealmName); !known && a.RealmName != stored.RealmName {
		list = append(list, &Refusal{Code: CodeRealmInvalid, Field: "realmName", Message: fmt.Sprintf(
			"realm %q is not one this server builds; realms: %s", a.RealmName, realmIDs())})
	}
	return list.err()
}

// CheckAnonymousUser refuses a save of anonymous access whose user signs
// in, with 409 (ruling I10): a local user with a stored password, whatever
// its status, or a user of a directory source, a stored row or a role
// mapping. Ids are matched without regard to letter case. A save that turns
// anonymous access off and keeps the stored user is not refused.
func CheckAnonymousUser(s *model.Snapshot, a, stored model.Anonymous) error {
	if !a.Enabled && a.UserID == stored.UserID {
		return nil
	}
	how := ""
	for _, u := range s.Users {
		switch {
		case how != "" || !strings.EqualFold(u.ID, a.UserID):
		case u.Source == sourceLocal && u.PasswordHash != "":
			how = "with a password"
		case u.Source != sourceLocal:
			how = "through the directory of the source " + u.Source
		}
	}
	for _, m := range s.RoleMappings {
		if how == "" && m.Source != sourceLocal && strings.EqualFold(m.UserID, a.UserID) {
			how = "through the directory of the source " + m.Source
		}
	}
	if how == "" {
		return nil
	}
	return &Refusal{Code: CodeAnonymousSignIn, Message: fmt.Sprintf(
		"user %s signs in %s, and requests without credentials would hold its rights; name an id that does not sign in",
		a.UserID, how)}
}

// CheckAnonymousGrant checks a save that turns anonymous access on as a
// grant of the rights of the user it names to requests without credentials
// (ruling I16, CheckGrant): the roles of its stored local row, whatever its
// status, else those of the role mappings of the local source. A save that
// turns access off is not checked.
func CheckAnonymousGrant(s *model.Snapshot, a model.Anonymous, holds func(permission string) bool) error {
	if !a.Enabled {
		return nil
	}
	roles, _ := AnonymousRoles(s, a.UserID)
	return CheckGrant(s, "requests without credentials through the user "+a.UserID, roles, holds)
}

// CheckAnonymousRoles refuses roles of anonymous access for id when id is
// a local user that signs in with a password, as they would replace the
// roles of that user (review R9): requests without credentials should not
// hold the rights of a user that signs in.
func CheckAnonymousRoles(s *model.Snapshot, id string) error {
	u := SignInUser(s, id)
	if u == nil {
		return nil
	}
	return &Refusal{Code: CodeAnonymousSignIn, Message: fmt.Sprintf(
		"user %s signs in with the roles %s, and the roles of anonymous access would replace them", id, roleText(u.Roles))}
}

// SetAnonymousRoles gives the anonymous user its roles: those of its local
// row, else a role mapping of the local source, which the server reads for
// a user without a row.
func SetAnonymousRoles(s *model.Snapshot, id string, roles []string) {
	for i := range s.Users {
		if s.Users[i].ID == id && s.Users[i].Source == sourceLocal {
			s.Users[i].Roles = roles
			return
		}
	}
	for i := range s.RoleMappings {
		if s.RoleMappings[i].UserID == id && s.RoleMappings[i].Source == sourceLocal {
			s.RoleMappings[i].Roles = roles
			return
		}
	}
	s.RoleMappings = append(s.RoleMappings, model.RoleMapping{UserID: id, Source: sourceLocal, Roles: roles})
}

// AnonymousRoles are the roles the server gives the anonymous user, and
// false for a local row that is not active.
func AnonymousRoles(s *model.Snapshot, id string) ([]string, bool) {
	for _, u := range s.Users {
		if u.ID == id && u.Source == sourceLocal {
			return u.Roles, u.Status == statusActive
		}
	}
	var roles []string
	for _, m := range s.RoleMappings {
		if m.Source == sourceLocal && strings.EqualFold(m.UserID, id) {
			roles = append(roles, m.Roles...)
		}
	}
	return roles, true
}

// RoleText is a list of roles for a message, "none" for no role.
func RoleText(roles []string) string { return roleText(roles) }

func roleText(roles []string) string {
	if len(roles) == 0 {
		return "none"
	}
	return strings.Join(roles, ", ")
}

// Realms

func realmIDs() string {
	ids := make([]string, len(auth.Realms))
	for i, r := range auth.Realms {
		ids[i] = r.ID
	}
	return strings.Join(ids, ", ")
}

// CheckRealmList checks the active realms in their order (decision N16):
// at least one, none empty, each one this server builds and none twice; 400
// at *, the first refusal alone.
func CheckRealmList(realms []string) error {
	if len(realms) == 0 {
		return realmRefusal("the list names no realm; give the active realms in their order")
	}
	var seen []string
	for _, id := range realms {
		switch _, known := auth.RealmByID(id); {
		case id == "":
			return realmRefusal("the list holds an empty realm name")
		case !known:
			return realmRefusal(fmt.Sprintf("realm %s is not one this server builds; realms: %s", id, realmIDs()))
		case slices.Contains(seen, id):
			return realmRefusal(fmt.Sprintf("realm %s is named twice", id))
		}
		seen = append(seen, id)
	}
	return nil
}

// CheckRealmRole refuses the realm DefaultRole without a default role (400
// at *).
func CheckRealmRole(realms []string, defaultRole string) error {
	if slices.Contains(realms, auth.RealmDefaultRole) && defaultRole == "" {
		return realmRefusal(fmt.Sprintf("the realm %s needs a role", auth.RealmDefaultRole))
	}
	return nil
}

// CheckRealmAdministrators refuses an order of active realms in which no
// administrator of A10 signs in, with 409 (ruling I11). The administrators
// are active local users, so the order keeps the local realm while one
// exists.
func CheckRealmAdministrators(s *model.Snapshot, realms []string) error {
	held := Administrators(s)
	if len(held) == 0 || slices.Contains(realms, auth.RealmLocal) {
		return nil
	}
	return &Refusal{Code: CodeLastAdministrator, Message: fmt.Sprintf(
		"the realms leave out %s, through which the users with the full permission set sign in: %s",
		auth.RealmLocal, strings.Join(held, ", "))}
}

func realmRefusal(message string) error {
	return &Refusal{Code: CodeRealmInvalid, Field: "*", Message: message}
}

// Grants

// CheckGrant refuses a change that gives a role or a user a permission the
// acting subject does not hold, with 403 (ruling I9). s is the snapshot
// after the change; roles are the roles of the changed record, which
// expand through internal/authz with their contained roles. holds reports a
// permission of the subject by the wildcard rule. A subject that holds the
// full permission set passes. A role named like a directory group and the
// password of a user count the same: what names the record in the message.
func CheckGrant(s *model.Snapshot, what string, roles []string, holds func(permission string) bool) error {
	if holds(fullPermission) {
		return nil
	}
	for _, p := range authz.NewResolver(s).Permissions(roles) {
		if !holds(p) {
			return &Refusal{Code: CodeGrantExceeds, Message: fmt.Sprintf(
				"the change gives %s the permission %s, which the signed in user does not hold", what, p)}
		}
	}
	return nil
}

// CheckDefaultRoleGrant checks a new order of active realms that makes the
// realm DefaultRole active, which s.Realms, the stored order, does not
// hold: every signed in user then holds the stored default role, so the
// subject holds each permission it expands to (ruling I15, CheckGrant). An
// order that keeps an active DefaultRole is not checked (R6).
func CheckDefaultRoleGrant(s *model.Snapshot, realms []string, holds func(permission string) bool) error {
	if !slices.Contains(realms, auth.RealmDefaultRole) || slices.Contains(s.Realms, auth.RealmDefaultRole) || s.DefaultRole == "" {
		return nil
	}
	return CheckGrant(s, "every signed in user through the default role "+s.DefaultRole, []string{s.DefaultRole}, holds)
}

// Roles

// CheckRoleCreate checks the create of a role (contract S22): an id of the
// name pattern that no stored or built in role holds (CheckCreate), the
// contained roles (CheckRole, ruling R5) and the privileges
// (CheckRolePrivileges). The refusals of 400 travel together.
func CheckRoleCreate(s *model.Snapshot, r model.Role) error {
	if err := CheckCreate(KindRole, r.ID, Exists(s, KindRole, r.ID)); err != nil {
		return err
	}
	return errors.Join(CheckRole(s, r.ID, r.Roles), CheckRolePrivileges(s, r.Privileges, nil))
}

// CheckRoleSave checks the save of the role id: the id of the body, when it
// names one, is the id (400 at id); a built in or read only role does not
// change (400 at *); then the contained roles and the privileges, those the
// stored role holds never refused (R6).
func CheckRoleSave(s *model.Snapshot, id string, body model.Role) error {
	var sent *string
	if body.ID != "" {
		sent = &body.ID
	}
	if err := CheckRename(KindRole, id, sent); err != nil {
		return err
	}
	var held []string
	for _, r := range s.Roles {
		if r.ID == id {
			held = r.Privileges
		}
	}
	if err := CheckRole(s, id, body.Roles); err != nil {
		var r *Refusal
		if errors.As(err, &r) && r.Code == CodeRoleReadOnly {
			return err
		}
		return errors.Join(err, CheckRolePrivileges(s, body.Privileges, held))
	}
	return CheckRolePrivileges(s, body.Privileges, held)
}

// CheckRolePrivileges refuses a privilege of a role that does not exist,
// with 400 at privileges: one that is neither stored nor built in nor
// generated for a repository. A privilege in held is never refused (R6).
func CheckRolePrivileges(s *model.Snapshot, privileges, held []string) error {
	var list Refusals
	for _, p := range privileges {
		if !slices.Contains(held, p) && !privilegeExists(s, p) {
			list = append(list, &Refusal{Code: CodePrivilegeMissing, Field: "privileges",
				Message: fmt.Sprintf("the privilege %s does not exist", p)})
		}
	}
	return list.err()
}

// privilegeExists reports a stored privilege of the name, or one that
// internal/authz builds for the snapshot: a role of that one privilege then
// expands to a permission.
func privilegeExists(s *model.Snapshot, name string) bool {
	if slices.ContainsFunc(s.Privileges, func(p model.Privilege) bool { return p.Name == name }) {
		return true
	}
	probe := &model.Snapshot{Repositories: s.Repositories, Roles: []model.Role{{ID: "\x00probe", Privileges: []string{name}}}}
	return len(authz.NewResolver(probe).Permissions([]string{"\x00probe"})) > 0
}

// CheckRoleInUse refuses the delete of a role that something holds, with
// 409 (ruling I4). The holders are looked for in this order, and the
// message names the first by kind and id: a user, a role mapping, a role
// that contains it, the anonymous user, the default role of the realms. The
// user row or the mapping of the user that anonymous access names counts as
// the anonymous user.
func CheckRoleInUse(s *model.Snapshot, id string) error {
	anonymous := s.Anonymous.UserID
	holder := ""
	for _, u := range s.Users {
		if holder == "" && slices.Contains(u.Roles, id) && !(u.ID == anonymous && u.Source == sourceLocal) {
			holder = "the user " + u.ID
		}
	}
	for _, m := range s.RoleMappings {
		if holder == "" && slices.Contains(m.Roles, id) && !(m.Source == sourceLocal && strings.EqualFold(m.UserID, anonymous)) {
			holder = fmt.Sprintf("the role mapping of %s (%s)", m.UserID, m.Source)
		}
	}
	for _, r := range s.Roles {
		if holder == "" && slices.Contains(r.Roles, id) {
			holder = "the role " + r.ID
		}
	}
	if roles, _ := AnonymousRoles(s, anonymous); holder == "" && anonymous != "" && slices.Contains(roles, id) {
		holder = "the anonymous user " + anonymous
	}
	if holder == "" && s.DefaultRole == id && id != "" {
		holder = "the default role of the realms"
	}
	if holder == "" {
		return nil
	}
	return &Refusal{Code: CodeRoleInUse, Message: fmt.Sprintf("the role %s is in use by %s", id, holder)}
}

// Act runs an action that changes no record of the state, a run or a stop
// of a task or a change of the read only mode, and ends it as a change ends
// (ruling SB3 of leg 6-system): after an action that succeeded the hook of
// committed runs. It refuses to act without a store, so that no action
// passes by the hook.
func Act(store IdentityStore, action func() error) error {
	if store == nil {
		return errors.New("an action needs the identity store")
	}
	if err := action(); err != nil {
		return err
	}
	committed(store)
	return nil
}
