package rest

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/mirosso25/pipesmith-registry/internal/model"
)

const (
	localSource = "default"
	ldapSource  = "LDAP"
)

// realms are the realms this server signs subjects in with: local users
// and LDAP. The token realms wait for phase 3.
var realms = []realmXO{
	{ID: "NexusAuthenticatingRealm", Name: "Local users"},
	{ID: "LdapRealm", Name: "LDAP directories"},
}

type realmXO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// privilegeFields are the properties of each privilege type under their
// REST names; the stored name comes first, the REST name second.
var privilegeFields = map[string][][2]string{
	"application":                 {{"domain", "domain"}},
	"repository-view":             {{"format", "format"}, {"repository", "repository"}},
	"repository-admin":            {{"format", "format"}, {"repository", "repository"}},
	"repository-content-selector": {{"contentSelector", "contentSelector"}, {"format", "format"}, {"repository", "repository"}},
	"wildcard":                    {{"pattern", "pattern"}},
	"script":                      {{"name", "scriptName"}},
}

// actionTypes are the types whose privileges carry actions.
var actionTypes = map[string]bool{"application": true, "repository-view": true, "repository-admin": true,
	"repository-content-selector": true, "script": true}

// actions are the stored actions as the REST list: upper case, "*" as ALL.
// The import keeps them as text, comma separated or as a JSON list.
func actions(stored string) []string {
	var parts []string
	if strings.HasPrefix(strings.TrimSpace(stored), "[") {
		if err := json.Unmarshal([]byte(stored), &parts); err != nil {
			parts = nil
		}
	} else {
		parts = strings.Split(stored, ",")
	}
	out := []string{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		switch p {
		case "":
		case "*":
			out = append(out, "ALL")
		default:
			out = append(out, strings.ToUpper(p))
		}
	}
	return out
}

// privilegeXO is ApiPrivilege: the common fields and those of its type.
func privilegeXO(p model.Privilege) map[string]any {
	out := map[string]any{"type": p.Type, "name": p.Name, "description": p.Description, "readOnly": p.ReadOnly}
	fields, known := privilegeFields[p.Type]
	if !known {
		for key, value := range p.Properties {
			if !secret(key) {
				out[key] = value
			}
		}
		return out
	}
	for _, f := range fields {
		out[f[1]] = p.Properties[f[0]]
	}
	if actionTypes[p.Type] {
		out["actions"] = actions(p.Properties["actions"])
	}
	return out
}

func (env *Env) snapshot() *model.Snapshot {
	if env.Snapshot == nil {
		return &model.Snapshot{}
	}
	return env.Snapshot
}

// Privileges is GET /v1/security/privileges, sorted by name.
func Privileges(env *Env) (Answer, error) {
	list := []map[string]any{}
	for _, p := range env.snapshot().Privileges {
		list = append(list, privilegeXO(p))
	}
	sort.Slice(list, func(i, j int) bool { return list[i]["name"].(string) < list[j]["name"].(string) })
	return ok(list)
}

// Privilege is GET /v1/security/privileges/{name}.
func Privilege(env *Env, name string) (Answer, error) {
	for _, p := range env.snapshot().Privileges {
		if p.Name == name {
			return ok(privilegeXO(p))
		}
	}
	return refuse(http.StatusNotFound, "privilege "+name+" not found")
}

func roleXO(r model.Role) model.Role {
	r.Privileges, r.Roles = nonNil(r.Privileges), nonNil(r.Roles)
	if r.Source == "" {
		r.Source = localSource
	}
	return r
}

// Roles is GET /v1/security/roles: the stored roles for the local source.
// The groups of a directory are read from the directory, which this server
// does not do yet (501); another source is unknown (404).
func Roles(env *Env, q url.Values) (Answer, error) {
	switch source := q.Get("source"); source {
	case "", localSource:
	case ldapSource:
		return NotImplemented("security", "reading the groups of an LDAP directory"), nil
	default:
		return refuse(http.StatusNotFound, "user source "+source+" not found")
	}
	list := []model.Role{}
	for _, r := range env.snapshot().Roles {
		list = append(list, roleXO(r))
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return ok(list)
}

// Role is GET /v1/security/roles/{id}.
func Role(env *Env, id string) (Answer, error) {
	for _, r := range env.snapshot().Roles {
		if r.ID == id {
			return ok(roleXO(r))
		}
	}
	return refuse(http.StatusNotFound, "role "+id+" not found")
}

// UserSources is GET /v1/security/user-sources: the local source, and LDAP
// when a connection is stored.
func UserSources(env *Env) (Answer, error) {
	list := []realmXO{{ID: localSource, Name: "Local"}}
	if len(env.snapshot().LDAP) > 0 {
		list = append(list, realmXO{ID: ldapSource, Name: ldapSource})
	}
	return ok(list)
}

// userXO is ApiUser. It has no field for a password or its hash.
type userXO struct {
	UserID        string   `json:"userId"`
	FirstName     string   `json:"firstName"`
	LastName      string   `json:"lastName"`
	EmailAddress  string   `json:"emailAddress"`
	Source        string   `json:"source"`
	Status        string   `json:"status"`
	ReadOnly      bool     `json:"readOnly"`
	Roles         []string `json:"roles"`
	ExternalRoles []string `json:"externalRoles"`
}

// users are the local users and the users of other sources the state
// knows: those stored as users and those with a role mapping. A directory
// user that has neither is not listed; this server does not search the
// directory yet.
func (env *Env) users() []userXO {
	snap := env.snapshot()
	byKey := map[[2]string]bool{}
	var list []userXO
	for _, u := range snap.Users {
		source := u.Source
		if source == "" {
			source = localSource
		}
		byKey[[2]string{source, u.ID}] = true
		list = append(list, userXO{UserID: u.ID, FirstName: u.FirstName, LastName: u.LastName, EmailAddress: u.Email, Source: source,
			Status: u.Status, ReadOnly: source != localSource, Roles: nonNil(u.Roles), ExternalRoles: []string{}})
	}
	for _, m := range snap.RoleMappings {
		if m.Source == localSource || byKey[[2]string{m.Source, m.UserID}] {
			continue
		}
		byKey[[2]string{m.Source, m.UserID}] = true
		list = append(list, userXO{UserID: m.UserID, Source: m.Source, Status: "active", ReadOnly: true, Roles: nonNil(m.Roles),
			ExternalRoles: []string{}})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].UserID != list[j].UserID {
			return list[i].UserID < list[j].UserID
		}
		return list[i].Source < list[j].Source
	})
	return list
}

// Users is GET /v1/security/users?source=&userId=: the users of the source,
// of every source without one; userId keeps those whose id holds the text,
// without regard to case.
func Users(env *Env, q url.Values) (Answer, error) {
	source, text := q.Get("source"), strings.ToLower(q.Get("userId"))
	list := []userXO{}
	for _, u := range env.users() {
		if (source == "" || u.Source == source) && strings.Contains(strings.ToLower(u.UserID), text) {
			list = append(list, u)
		}
	}
	return ok(list)
}

// Anonymous is GET /v1/security/anonymous.
func Anonymous(env *Env) (Answer, error) {
	return ok(env.snapshot().Anonymous)
}

// RealmsAvailable is GET /v1/security/realms/available.
func RealmsAvailable(env *Env) (Answer, error) {
	return ok(realms)
}

// RealmsActive is GET /v1/security/realms/active: the stored order, also
// with ids this server does not build.
func RealmsActive(env *Env) (Answer, error) {
	return ok(nonNil(env.snapshot().Realms))
}

// LDAPServers is GET /v1/security/ldap in the order of the connections.
// The read shape has no bind password.
func LDAPServers(env *Env) (Answer, error) {
	list := append([]model.LDAPServer{}, env.snapshot().LDAP...)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Order < list[j].Order })
	return ok(list)
}

// LDAPServer is GET /v1/security/ldap/{name}.
func LDAPServer(env *Env, name string) (Answer, error) {
	for _, s := range env.snapshot().LDAP {
		if s.Name == name {
			return ok(s)
		}
	}
	return refuse(http.StatusNotFound, "LDAP connection "+name+" not found")
}
