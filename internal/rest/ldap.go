package rest

import (
	"net/http"
	"slices"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The writes of the LDAP connections (leg 6-identity, contract S26 and
// S27): create, save, delete, the order and the cache of sign ins. Every
// change goes through admin.Change, which empties the credential cache and
// marks the view of the service stale (ruling I21). The bind password goes
// into the secret file of the connection (admin.WriteLDAPSecret, mode 0600)
// and into no answer and no row of state.sqlite (A11). A change of a
// directory needs the full permission (ruling I17).

var ldapKinds = []state.Kind{state.KindLDAP}

// ldapForm reads a form body: the decoded object, the connection and its
// bind password.
func ldapForm(body []byte, prefix string) (map[string]any, model.LDAPServer, *string, error) {
	var m map[string]any
	if err := decode(body, '{', &m); err != nil {
		return nil, model.LDAPServer{}, nil, err
	}
	server, err := admin.LDAPConnection(m, prefix)
	if err != nil {
		return nil, server, nil, err
	}
	password, err := admin.LDAPPassword(m, prefix)
	return m, server, password, err
}

// ldapIndex is the index of the stored connection of the name, -1 for none.
func ldapIndex(s *model.Snapshot, name string) int {
	return slices.IndexFunc(s.LDAP, func(l model.LDAPServer) bool { return l.Name == name })
}

// secretChange runs the write of a secret file inside a change and keeps
// the function that undoes it, for a change whose write of the state fails.
type secretChange struct{ undo func() }

func (c *secretChange) run(write func() (func(), error)) error {
	undo, err := write()
	if err == nil {
		c.undo = undo
	}
	return err
}

func (c *secretChange) failed() {
	if c.undo != nil {
		c.undo()
	}
}

// CreateLDAP is POST /v1/security/ldap: 201 without a body. The id is new,
// the order one past the last connection. A scheme with a bind needs the
// bind password (ruling I18).
func CreateLDAP(env *Env, w IdentityWrite, body []byte) (Answer, error) {
	m, server, password, err := ldapForm(body, "")
	if err != nil {
		return refused(err)
	}
	if err := admin.CheckLDAPPassword(server.AuthScheme, password, true, ""); err != nil {
		return refused(err)
	}
	if server.ID, err = admin.NewLDAPID(); err != nil {
		return Answer{}, err
	}
	var secret secretChange
	_, err = admin.Change(w.Store, ldapKinds, func(s *model.Snapshot) error {
		if err := admin.CheckCreate(admin.KindLDAPConnection, server.Name, admin.Exists(s, admin.KindLDAPConnection, server.Name)); err != nil {
			return err
		}
		if err := admin.CheckPassThrough(s, admin.KindLDAPConnection, "", m); err != nil {
			return err
		}
		if err := admin.CheckLDAPGrant("the create of the LDAP connection "+server.Name, env.permits); err != nil {
			return err
		}
		server.Order = admin.NextLDAPOrder(s)
		s.LDAP = append(s.LDAP, server)
		if !admin.NeedsBind(server.AuthScheme) {
			return nil
		}
		return secret.run(func() (func(), error) { return admin.WriteLDAPSecret(w.Store.DataDir(), server.ID, *password) })
	})
	if err != nil {
		secret.failed()
		return refused(err)
	}
	return Answer{Status: http.StatusCreated}, nil
}

// SaveLDAP is PUT /v1/security/ldap/{name}: 204. The body may carry a new
// name (ruling R2b); the id, the order and the CA file stay. A body without
// authPassword keeps the stored secret file, an empty one is refused for a
// scheme with a bind (I18). A save that leaves a scheme with a bind removes
// the secret file; one to a scheme with a bind needs authPassword while the
// connection has no secret file (I23). A save that changes the address, the base, the
// user or the group settings needs the full permission (I17).
func SaveLDAP(env *Env, w IdentityWrite, name string, body []byte) (Answer, error) {
	m, server, password, err := ldapForm(body, "")
	if err != nil {
		return refused(err)
	}
	if err := admin.CheckLDAPPassword(server.AuthScheme, password, false, ""); err != nil {
		return refused(err)
	}
	var sentID *string
	if id, ok := m["id"].(string); ok {
		sentID = &id
	}
	var secret secretChange
	_, err = admin.Change(w.Store, ldapKinds, func(s *model.Snapshot) error {
		i := ldapIndex(s, name)
		if i < 0 {
			return notFound("LDAP connection " + name)
		}
		stored := s.LDAP[i]
		if err := admin.CheckLDAPSave(s, stored, sentID, &server.Name); err != nil {
			return err
		}
		if err := admin.CheckPassThrough(s, admin.KindLDAPConnection, stored.Name, m); err != nil {
			return err
		}
		if admin.LDAPDirectoryChanged(stored, server) {
			if err := admin.CheckLDAPGrant("a save that changes the directory of the LDAP connection "+name, env.permits); err != nil {
				return err
			}
		}
		server.ID, server.Order, server.CAFile = stored.ID, stored.Order, stored.CAFile
		s.LDAP[i] = server
		dir := w.Store.DataDir()
		switch {
		case !admin.NeedsBind(server.AuthScheme) && admin.NeedsBind(stored.AuthScheme):
			return secret.run(func() (func(), error) { return admin.RemoveLDAPSecret(dir, server.ID) })
		case !admin.NeedsBind(server.AuthScheme):
			return nil
		case password != nil:
			return secret.run(func() (func(), error) { return admin.WriteLDAPSecret(dir, server.ID, *password) })
		}
		has, err := admin.HasLDAPSecretFile(dir, server.ID)
		if err != nil || has {
			return err
		}
		return admin.CheckLDAPPassword(server.AuthScheme, nil, true, "")
	})
	if err != nil {
		secret.failed()
		return refused(err)
	}
	return noContent()
}

// DeleteLDAP is DELETE /v1/security/ldap/{name}: 204; the secret file of
// the connection is removed with it. It needs the full permission (I17).
func DeleteLDAP(env *Env, w IdentityWrite, name string) (Answer, error) {
	var secret secretChange
	_, err := admin.Change(w.Store, ldapKinds, func(s *model.Snapshot) error {
		i := ldapIndex(s, name)
		if i < 0 {
			return notFound("LDAP connection " + name)
		}
		if err := admin.CheckLDAPGrant("the delete of the LDAP connection "+name, env.permits); err != nil {
			return err
		}
		id := s.LDAP[i].ID
		s.LDAP = slices.Delete(s.LDAP, i, i+1)
		return secret.run(func() (func(), error) { return admin.RemoveLDAPSecret(w.Store.DataDir(), id) })
	})
	if err != nil {
		secret.failed()
		return refused(err)
	}
	return noContent()
}

// OrderLDAP is POST /v1/security/ldap/change-order: the names of every
// connection in the new order, 204; the connections get the numbers 1 and
// up. A change of the sequence needs the full permission (I17); the stored
// sequence passes whatever its numbers (I26).
func OrderLDAP(env *Env, w IdentityWrite, body []byte) (Answer, error) {
	var names []string
	if err := decode(body, '[', &names); err != nil {
		return refused(err)
	}
	_, err := admin.Change(w.Store, ldapKinds, func(s *model.Snapshot) error {
		if err := admin.CheckLDAPOrder(s, names); err != nil {
			return err
		}
		changed := admin.LDAPOrderChanged(s, names)
		admin.SetLDAPOrder(s, names)
		if !changed {
			return nil
		}
		return admin.CheckLDAPGrant("a change of the order of the LDAP connections", env.permits)
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}

// ClearLDAPCache is POST /api/ldap/clear-cache: 204. It passes
// admin.Change with no change of a record, which empties the credential
// cache and marks the view stale (ruling I21); the body is not read.
func ClearLDAPCache(env *Env, w IdentityWrite) (Answer, error) {
	if _, err := admin.Change(w.Store, ldapKinds, func(*model.Snapshot) error { return nil }); err != nil {
		return refused(err)
	}
	return noContent()
}
