package rest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The writes of the area security (leg 6-identity, contract S22 to S25 and
// S28): roles, users, the password of a user, anonymous access and the
// active realms. Every change goes through admin.Change, the one place of
// the identity changes, and every refusal is answered by admin.Answer in
// the shapes of section Errors of the contract. The server checks the
// permission of the route and the request token before it calls a function
// here.

// IdentityWrite is what a write of the area security needs besides Env:
// the store of the identity state and the acting subject.
type IdentityWrite struct {
	Store   admin.IdentityStore
	Subject *model.Subject
}

// administerUsers is the permission of a subject that may administer
// users: every action on them. A save of the own user by a subject without
// it keeps the roles (ruling I7).
const administerUsers = "nexus:users:*"

// refused answers an error of a rule; an error that holds no refusal is
// returned, and the server answers it with 500.
func refused(err error) (Answer, error) {
	status, body := admin.Answer(err)
	if status == http.StatusInternalServerError {
		return Answer{}, err
	}
	return Answer{Status: status, Body: body}, nil
}

func noContent() (Answer, error) { return Answer{Status: http.StatusNoContent}, nil }

// decode reads a JSON body into v, whose JSON form starts with open: { for
// an object, [ for a list. Fields the shape does not name are ignored. A
// body that is empty, no JSON of that form, or holds a value of another
// type is refused with 400 (body-invalid), at the field of a wrong value.
func decode(body []byte, open byte, v any) error {
	trimmed := bytes.TrimSpace(body)
	what := map[byte]string{'{': "object", '[': "list"}[open]
	if len(trimmed) == 0 || trimmed[0] != open {
		return &admin.Refusal{Code: admin.CodeBodyInvalid, Field: "*", Message: "the body is no JSON " + what}
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	err := dec.Decode(v)
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &typeErr) && typeErr.Field != "":
		return &admin.Refusal{Code: admin.CodeBodyInvalid, Field: typeErr.Field,
			Message: fmt.Sprintf("the value of %s is no %s", typeErr.Field, jsonKind(typeErr.Type.Kind().String()))}
	case err != nil:
		return &admin.Refusal{Code: admin.CodeBodyInvalid, Field: "*", Message: "the body is no JSON " + what}
	case dec.More():
		return &admin.Refusal{Code: admin.CodeBodyInvalid, Field: "*", Message: "the body holds more than one JSON " + what}
	}
	return nil
}

// jsonKind names a Go kind in the words of JSON.
func jsonKind(kind string) string {
	switch kind {
	case "string":
		return "string"
	case "bool":
		return "boolean"
	case "slice":
		return "list"
	case "struct", "map":
		return "object"
	}
	return "value of the expected type"
}

func notFound(what string) error {
	return &admin.Refusal{Code: admin.CodeNotFound, Message: what + " not found"}
}

// Roles

// roleBody is RoleXORequest.
type roleBody struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Privileges  []string `json:"privileges"`
	Roles       []string `json:"roles"`
}

// CreateRole is POST /v1/security/roles: 200 with RoleXOResponse.
func CreateRole(env *Env, w IdentityWrite, body []byte) (Answer, error) {
	var b roleBody
	if err := decode(body, '{', &b); err != nil {
		return refused(err)
	}
	role := model.Role{ID: b.ID, Source: localSource, Name: b.Name, Description: b.Description,
		Privileges: nonNil(b.Privileges), Roles: nonNil(b.Roles)}
	_, err := admin.Change(w.Store, []state.Kind{state.KindRoles}, func(s *model.Snapshot) error {
		if err := admin.CheckRoleCreate(s, role); err != nil {
			return err
		}
		s.Roles = append(s.Roles, role)
		return admin.CheckGrant(s, "the role "+role.ID, []string{role.ID}, env.permits)
	})
	if err != nil {
		return refused(err)
	}
	return ok(roleXO(role))
}

// SaveRole is PUT /v1/security/roles/{id}: 204. The id cannot change; a
// built in or read only role is refused with 400 at *.
func SaveRole(env *Env, w IdentityWrite, id string, body []byte) (Answer, error) {
	var b roleBody
	if err := decode(body, '{', &b); err != nil {
		return refused(err)
	}
	_, err := admin.Change(w.Store, []state.Kind{state.KindRoles}, func(s *model.Snapshot) error {
		if !admin.Exists(s, admin.KindRole, id) {
			return notFound("role " + id)
		}
		if err := admin.CheckRoleSave(s, id, model.Role{ID: b.ID, Privileges: b.Privileges, Roles: b.Roles}); err != nil {
			return err
		}
		i := slices.IndexFunc(s.Roles, func(r model.Role) bool { return r.ID == id })
		r := &s.Roles[i]
		r.Name, r.Description, r.Privileges, r.Roles = b.Name, b.Description, nonNil(b.Privileges), nonNil(b.Roles)
		return admin.CheckGrant(s, "the role "+id, []string{id}, env.permits)
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}

// DeleteRole is DELETE /v1/security/roles/{id}: 204. A built in or read
// only role is refused with 400 at *, a role in use with 409 (ruling I4).
func DeleteRole(env *Env, w IdentityWrite, id string) (Answer, error) {
	_, err := admin.Change(w.Store, []state.Kind{state.KindRoles}, func(s *model.Snapshot) error {
		if !admin.Exists(s, admin.KindRole, id) {
			return notFound("role " + id)
		}
		if err := admin.CheckRoleDelete(s, id); err != nil {
			return err
		}
		if err := admin.CheckRoleInUse(s, id); err != nil {
			return err
		}
		s.Roles = slices.DeleteFunc(s.Roles, func(r model.Role) bool { return r.ID == id })
		return nil
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}

// Users

// userBody is ApiCreateUser and ApiUser; the fields of the answer only,
// readOnly and externalRoles, are ignored.
type userBody struct {
	UserID       string   `json:"userId"`
	FirstName    string   `json:"firstName"`
	LastName     string   `json:"lastName"`
	EmailAddress string   `json:"emailAddress"`
	Password     string   `json:"password"`
	Source       string   `json:"source"`
	Status       string   `json:"status"`
	Roles        []string `json:"roles"`
}

func (b userBody) user() model.User {
	return model.User{ID: b.UserID, FirstName: b.FirstName, LastName: b.LastName, Email: b.EmailAddress, Status: b.Status,
		Source: localSource, Roles: nonNil(b.Roles)}
}

// xoOf is the answer shape of a stored user.
func xoOf(u model.User) userXO {
	return userXO{UserID: u.ID, FirstName: u.FirstName, LastName: u.LastName, EmailAddress: u.Email, Source: u.Source,
		Status: u.Status, ReadOnly: u.Source != localSource, Roles: nonNil(u.Roles), ExternalRoles: []string{}}
}

// CreateUser is POST /v1/security/users: a local user (ruling I1) with
// its password (I5), 200 with ApiUser, which holds no password.
func CreateUser(env *Env, w IdentityWrite, body []byte) (Answer, error) {
	var b userBody
	if err := decode(body, '{', &b); err != nil {
		return refused(err)
	}
	user := b.user()
	if _, err := admin.CreateUser(w.Store, user, b.Password, env.permits); err != nil {
		return refused(err)
	}
	return ok(xoOf(user))
}

// storedUser is the user of the id and the source: a stored row, else for
// a source other than the local one the role mapping, as GET users lists
// it. ok is false when the state knows neither.
func storedUser(s *model.Snapshot, id, source string) (model.User, bool) {
	for _, u := range s.Users {
		if u.ID == id && sourceOf(u.Source) == source {
			return u, true
		}
	}
	if source != localSource {
		for _, m := range s.RoleMappings {
			if m.UserID == id && m.Source == source {
				return model.User{ID: id, Source: source, Status: "active", Roles: m.Roles}, true
			}
		}
	}
	return model.User{}, false
}

func sourceOf(source string) string {
	if source == "" {
		return localSource
	}
	return source
}

// own reports that the user is the one of the acting subject.
func (w IdentityWrite) own(u model.User) bool {
	return w.Subject != nil && w.Subject.User.ID == u.ID && sourceOf(w.Subject.User.Source) == sourceOf(u.Source)
}

// SaveUser is PUT /v1/security/users/{userId}: the whole ApiUser, 204
// (ruling I7). The source of the body picks the user, the local one when
// it names none.
func SaveUser(env *Env, w IdentityWrite, id string, body []byte) (Answer, error) {
	var b userBody
	if err := decode(body, '{', &b); err != nil {
		return refused(err)
	}
	source := sourceOf(b.Source)
	sent := b.user()
	sent.Source = source
	administers := env.permits(administerUsers)
	_, err := admin.Change(w.Store, []state.Kind{state.KindUsers}, func(s *model.Snapshot) error {
		stored, found := storedUser(s, id, source)
		if !found {
			return notFound("user " + id)
		}
		saved, err := admin.SaveUser(s, stored, sent, w.own(stored), administers)
		if err != nil {
			return err
		}
		// I9: a save of another user, or a change of the own roles, gives
		// no permission the subject does not hold
		if !w.own(stored) || !slices.Equal(stored.Roles, saved.Roles) {
			if err := admin.CheckGrant(s, "the user "+id, saved.Roles, env.permits); err != nil {
				return err
			}
		}
		if i := slices.IndexFunc(s.Users, func(u model.User) bool { return u.ID == id && sourceOf(u.Source) == source }); i >= 0 {
			s.Users[i] = saved
			return nil
		}
		i := slices.IndexFunc(s.RoleMappings, func(m model.RoleMapping) bool { return m.UserID == id && m.Source == source })
		s.RoleMappings[i].Roles = saved.Roles
		return nil
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}

// DeleteUser is DELETE /v1/security/users/{userId}?realm=: 204. The query
// realm names the source by its realm id or its source name, the local one
// when it is absent; another value is 400 at realm (ruling I12). A directory
// user, the own user and the user anonymous access names are refused with
// 409 (ruling Q5), the last administrator too (A10).
func DeleteUser(env *Env, w IdentityWrite, id string, q url.Values) (Answer, error) {
	source, err := admin.UserSource(q.Get("realm"))
	if err != nil {
		return refused(err)
	}
	_, err = admin.Change(w.Store, []state.Kind{state.KindUsers}, func(s *model.Snapshot) error {
		stored, found := storedUser(s, id, source)
		if !found {
			return notFound("user " + id)
		}
		if err := admin.CheckUserDelete(s, w.Subject, stored); err != nil {
			return err
		}
		s.Users = slices.DeleteFunc(s.Users, func(u model.User) bool { return u.ID == id && sourceOf(u.Source) == source })
		return nil
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}

// ChangePassword is PUT /v1/security/users/{userId}/change-password: the
// body is the new password as it is (text/plain), 204. A user of another
// source is refused with 409 (ruling Q18), a password the rules refuse
// with 400 (I5).
func ChangePassword(env *Env, w IdentityWrite, id string, body []byte) (Answer, error) {
	password := string(body)
	snap, err := w.Store.Load()
	if err != nil {
		return Answer{}, err
	}
	if err := passwordTarget(snap, id); err != nil {
		return refused(err)
	}
	if err := passwordGrant(env, snap, id); err != nil {
		return refused(err)
	}
	if err := admin.CheckPassword(password); err != nil {
		return refused(err)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return Answer{}, err
	}
	_, err = admin.Change(w.Store, []state.Kind{state.KindUsers}, func(s *model.Snapshot) error {
		if err := passwordTarget(s, id); err != nil {
			return err
		}
		if err := passwordGrant(env, s, id); err != nil {
			return err
		}
		u, err := admin.LocalUser(s, id)
		if err == nil {
			u.PasswordHash = hash
		}
		return err
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}

// passwordTarget refuses the password change of a user that is not local:
// 409 for a user of another source, 404 for an id the state does not know.
func passwordTarget(s *model.Snapshot, id string) error {
	if _, err := admin.LocalUser(s, id); err == nil {
		return nil
	}
	for _, u := range envOf(s).users() {
		if u.UserID == id {
			return admin.CheckPasswordChange(model.User{ID: id, Source: u.Source})
		}
	}
	return notFound("user " + id)
}

// passwordGrant refuses the password change of a user whose permissions
// the subject does not hold: the one who sets a password can sign in as
// that user (ruling I9).
func passwordGrant(env *Env, s *model.Snapshot, id string) error {
	u, err := admin.LocalUser(s, id)
	if err != nil {
		return err
	}
	return admin.CheckGrant(s, "the user "+id+" through its password", u.Roles, env.permits)
}

// envOf is an Env over a snapshot, for the list of users.
func envOf(s *model.Snapshot) *Env { return &Env{Snapshot: s} }

// Anonymous access and realms

// SaveAnonymous is PUT /v1/security/anonymous: {enabled, userId,
// realmName}, 200 with the saved setting (contract S25). A userId of a user
// that signs in is refused with 409 (ruling I10); a save that turns access
// on is a grant of the rights of its user (I16).
func SaveAnonymous(env *Env, w IdentityWrite, body []byte) (Answer, error) {
	var a model.Anonymous
	if err := decode(body, '{', &a); err != nil {
		return refused(err)
	}
	after, err := admin.Change(w.Store, []state.Kind{state.KindAnonymous}, func(s *model.Snapshot) error {
		if err := admin.CheckAnonymous(a, s.Anonymous); err != nil {
			return err
		}
		if err := admin.CheckAnonymousUser(s, a, s.Anonymous); err != nil {
			return err
		}
		if err := admin.CheckAnonymousGrant(s, a, env.permits); err != nil {
			return err
		}
		s.Anonymous = a
		return nil
	})
	if err != nil {
		return refused(err)
	}
	return ok(after.Anonymous)
}

// SaveRealms is PUT /v1/security/realms/active: the active realm ids in
// their order, 204 (decision N16). An order without the local realm is
// refused with 409 while a local user holds the full permission set
// (ruling I11); an order that makes DefaultRole active is a grant of the
// default role (I15). No REST write sets the default role itself.
func SaveRealms(env *Env, w IdentityWrite, body []byte) (Answer, error) {
	var realms []string
	if err := decode(body, '[', &realms); err != nil {
		return refused(err)
	}
	if err := admin.CheckRealmList(realms); err != nil {
		return refused(err)
	}
	_, err := admin.Change(w.Store, []state.Kind{state.KindRealms}, func(s *model.Snapshot) error {
		if err := admin.CheckRealmRole(realms, s.DefaultRole); err != nil {
			return err
		}
		if err := admin.CheckRealmAdministrators(s, realms); err != nil {
			return err
		}
		if err := admin.CheckDefaultRoleGrant(s, realms, env.permits); err != nil {
			return err
		}
		s.Realms = realms
		return nil
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}
