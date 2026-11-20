package rest

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/authz"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// identityStore is admin.IdentityStore over a snapshot in memory.
type identityStore struct {
	snap          *model.Snapshot
	writes, reset int
}

func (f *identityStore) Load() (*model.Snapshot, error) { return copyIdentity(f.snap), nil }
func (f *identityStore) DataDir() string                { return "" }
func (f *identityStore) ResetCredentials()              { f.reset++ }
func (f *identityStore) MarkStale()                     {}
func (f *identityStore) Update(_ []state.Kind, fn func(*model.Snapshot) error) error {
	s := copyIdentity(f.snap)
	if err := fn(s); err != nil {
		return err
	}
	f.snap, f.writes = s, f.writes+1
	return nil
}

func copyIdentity(s *model.Snapshot) *model.Snapshot {
	c := *s
	c.Users = slices.Clone(s.Users)
	for i := range c.Users {
		c.Users[i].Roles = slices.Clone(c.Users[i].Roles)
	}
	c.Roles = slices.Clone(s.Roles)
	c.RoleMappings = slices.Clone(s.RoleMappings)
	c.Realms = slices.Clone(s.Realms)
	return &c
}

const oldHash = "$shiro1$SHA-512$1024$c2FsdA==$aGFzaA=="

func identityFixture() *identityStore {
	return &identityStore{snap: &model.Snapshot{
		Users: []model.User{
			{ID: "admin", Status: "active", Source: "default", Roles: []string{"nx-admin"}, PasswordHash: oldHash},
			{ID: "viewer", FirstName: "View", LastName: "Er", Email: "viewer@example.com", Status: "active", Source: "default",
				Roles: []string{"demo-viewer"}, PasswordHash: oldHash},
		},
		Roles:        []model.Role{{ID: "demo-viewer", Source: "default", Name: "Viewer", Privileges: []string{"nx-search-read"}}},
		RoleMappings: []model.RoleMapping{{UserID: "ldap.person", Source: "LDAP", Roles: []string{"demo-viewer"}}},
		Realms:       []string{"NexusAuthenticatingRealm"},
		Anonymous:    model.Anonymous{UserID: "anonymous", RealmName: "NexusAuthorizingRealm"},
	}}
}

// as is the write of the subject id with the permissions.
func as(store *identityStore, id string, permissions ...string) (*Env, IdentityWrite) {
	env := &Env{Snapshot: store.snap, Permits: func(p string) bool {
		for _, g := range permissions {
			if g == p || g == "nexus:*" {
				return true
			}
		}
		return false
	}}
	return env, IdentityWrite{Store: store, Subject: &model.Subject{User: model.User{ID: id, Source: "default", Status: "active"}}}
}

// answered checks the status of an answer, and for a 400 the field ids of
// its list, and that it holds no secret (A11).
func answered(t *testing.T, label string, a Answer, err error, status int, fields ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	data, _ := a.JSON()
	if a.Status != status {
		t.Fatalf("%s: %d %s, want %d", label, a.Status, data, status)
	}
	if strings.Contains(string(data), "$shiro1$") || strings.Contains(string(data), "example-new-1") || len(admin.Leaks(a.Body, "")) > 0 {
		t.Fatalf("%s: a secret in %s", label, data)
	}
	if status == http.StatusBadRequest {
		list, ok := a.Body.([]admin.FieldMessage)
		if !ok {
			t.Fatalf("%s: a 400 without a list: %s", label, data)
		}
		var ids []string
		for _, f := range list {
			ids = append(ids, f.ID)
		}
		if !slices.Equal(ids, fields) {
			t.Fatalf("%s: fields %v, want %v", label, ids, fields)
		}
	}
}

func userOf(s *model.Snapshot, id string) *model.User {
	for i := range s.Users {
		if s.Users[i].ID == id {
			return &s.Users[i]
		}
	}
	return nil
}

func TestRoleWrites(t *testing.T) {
	store := identityFixture()
	env, w := as(store, "admin", "nexus:*")
	a, err := CreateRole(env, w, []byte(`{"id":"demo-new","name":"New","description":"d","privileges":["nx-search-read"],"roles":["demo-viewer"]}`))
	answered(t, "create", a, err, 200)
	if r, ok := a.Body.(model.Role); !ok || r.ID != "demo-new" || r.Source != "default" || !slices.Equal(r.Roles, []string{"demo-viewer"}) {
		t.Fatalf("create answers %+v", a.Body)
	}
	if len(store.snap.Roles) != 2 || store.reset != 1 {
		t.Fatalf("after create: %+v, %d resets", store.snap.Roles, store.reset)
	}
	for _, c := range []struct {
		label  string
		body   string
		status int
		fields []string
	}{
		{"empty body", ``, 400, []string{"*"}},
		{"no object", `[1]`, 400, []string{"*"}},
		{"null", `null`, 400, []string{"*"}},
		{"wrong type", `{"id":"x","privileges":"nx-all"}`, 400, []string{"privileges"}},
		{"exists", `{"id":"demo-viewer","name":"Again"}`, 409, nil},
		{"unknown privilege and role", `{"id":"demo-x","privileges":["no-such"],"roles":["gone"]}`, 400, []string{"roles", "privileges"}},
	} {
		a, err := CreateRole(env, w, []byte(c.body))
		answered(t, "create "+c.label, a, err, c.status, c.fields...)
	}
	a, err = SaveRole(env, w, "demo-new", []byte(`{"id":"demo-new","name":"Renamed","privileges":[],"roles":[]}`))
	answered(t, "save", a, err, 204)
	if store.snap.Roles[1].Name != "Renamed" {
		t.Fatalf("after save: %+v", store.snap.Roles[1])
	}
	a, err = SaveRole(env, w, "no-such", []byte(`{"name":"x"}`))
	answered(t, "save missing", a, err, 404)
	a, err = SaveRole(env, w, "nx-admin", []byte(`{"name":"x"}`))
	answered(t, "save built in", a, err, 400, "*")
	a, err = SaveRole(env, w, "demo-new", []byte(`{"id":"demo-other"}`))
	answered(t, "save another id", a, err, 400, "id")
	a, err = DeleteRole(env, w, "demo-viewer")
	answered(t, "delete in use", a, err, 409)
	if !strings.Contains(a.Body.(admin.Message).Message, "user viewer") {
		t.Fatalf("in use: %+v", a.Body)
	}
	a, err = DeleteRole(env, w, "nx-anonymous")
	answered(t, "delete built in", a, err, 400, "*")
	a, err = DeleteRole(env, w, "no-such")
	answered(t, "delete missing", a, err, 404)
	writes := store.writes
	a, err = DeleteRole(env, w, "demo-new")
	answered(t, "delete", a, err, 204)
	if len(store.snap.Roles) != 1 || store.writes != writes+1 {
		t.Fatalf("after delete: %+v", store.snap.Roles)
	}
}

func TestUserWrites(t *testing.T) {
	store := identityFixture()
	env, w := as(store, "admin", "nexus:*")
	a, err := CreateUser(env, w, []byte(`{"userId":"demo-new@example.com","firstName":"N","lastName":"U","emailAddress":"n@example.com",`+
		`"password":"example-new-1","status":"active","roles":["demo-viewer"]}`))
	answered(t, "create", a, err, 200)
	if u, ok := a.Body.(userXO); !ok || u.UserID != "demo-new@example.com" || u.Source != "default" || u.ReadOnly {
		t.Fatalf("create answers %+v", a.Body)
	}
	created := userOf(store.snap, "demo-new@example.com")
	if created == nil || !auth.VerifyPassword("example-new-1", created.PasswordHash) || created.Source != "default" {
		t.Fatalf("created %+v", created)
	}
	for _, c := range []struct {
		label  string
		body   string
		status int
		fields []string
	}{
		{"empty body", ``, 400, []string{"*"}},
		{"no password", `{"userId":"demo-b","status":"active","roles":[]}`, 400, []string{"password"}},
		{"exists", `{"userId":"viewer","status":"active","password":"p"}`, 409, nil},
		{"bad id", `{"userId":"a:b","status":"active","password":"p"}`, 400, []string{"userId"}},
	} {
		a, err := CreateUser(env, w, []byte(c.body))
		answered(t, "create "+c.label, a, err, c.status, c.fields...)
	}
	a, err = SaveUser(env, w, "viewer", []byte(`{"userId":"viewer","firstName":"V","lastName":"E","emailAddress":"v@example.com",`+
		`"source":"default","status":"disabled","readOnly":false,"roles":["nx-anonymous"],"externalRoles":[]}`))
	answered(t, "save", a, err, 204)
	if u := userOf(store.snap, "viewer"); u.Status != "disabled" || u.FirstName != "V" || !slices.Equal(u.Roles, []string{"nx-anonymous"}) ||
		u.PasswordHash != oldHash {
		t.Fatalf("after save: %+v", u)
	}
	a, err = SaveUser(env, w, "nobody", []byte(`{"userId":"nobody","status":"active"}`))
	answered(t, "save missing", a, err, 404)
	a, err = SaveUser(env, w, "ldap.person", []byte(`{"userId":"ldap.person","source":"LDAP","status":"active","roles":["nx-anonymous"]}`))
	answered(t, "save directory roles", a, err, 204)
	if !slices.Equal(store.snap.RoleMappings[0].Roles, []string{"nx-anonymous"}) {
		t.Fatalf("mapping %+v", store.snap.RoleMappings)
	}
	// A10: the last administrator keeps its status and roles
	a, err = SaveUser(env, w, "admin", []byte(`{"userId":"admin","status":"disabled","roles":["nx-admin"]}`))
	answered(t, "disable the last administrator", a, err, 409)
	if userOf(store.snap, "admin").Status != "active" {
		t.Fatal("the last administrator was disabled")
	}
	// I7: the own save of a subject that may not administer users keeps
	// its roles; by the wildcard rule nexus:users:update does not imply
	// every action on users, nexus:users:* does
	if authz.Implies("nexus:users:update", administerUsers) || !authz.Implies("nexus:users:*", administerUsers) ||
		!authz.Implies("nexus:*", administerUsers) {
		t.Fatalf("the wildcard rule reads %s otherwise", administerUsers)
	}
	ownEnv, own := as(store, "viewer", "nexus:users:update")
	a, err = SaveUser(ownEnv, own, "viewer", []byte(`{"userId":"viewer","status":"disabled","roles":["nx-admin"]}`))
	answered(t, "own save", a, err, 204)
	if u := userOf(store.snap, "viewer"); !slices.Equal(u.Roles, []string{"nx-anonymous"}) {
		t.Fatalf("the own save changed the roles: %+v", u)
	}
	// I7: one who may administer users sets its own roles; I9: admin holds
	// nx-admin, so its subject holds the full permission set
	adminEnv, adminSelf := as(store, "admin", "nexus:users:*", "nexus:*")
	a, err = SaveUser(adminEnv, adminSelf, "admin", []byte(`{"userId":"admin","status":"active","roles":["nx-admin","demo-viewer"]}`))
	answered(t, "own save with users:*", a, err, 204)
	if u := userOf(store.snap, "admin"); !slices.Equal(u.Roles, []string{"nx-admin", "demo-viewer"}) {
		t.Fatalf("own save with users:*: %+v", u)
	}

	for _, c := range []struct {
		label, id, query string
		status           int
	}{
		{"own", "admin", "", 409},
		{"directory", "ldap.person", "realm=LDAP", 409},
		{"directory by its realm", "ldap.person", "realm=LdapRealm", 409},
		{"missing", "nobody", "", 404},
	} {
		q, _ := url.ParseQuery(c.query)
		a, err := DeleteUser(env, w, c.id, q)
		answered(t, "delete "+c.label, a, err, c.status)
	}
	a, err = DeleteUser(env, w, "viewer", url.Values{"realm": {"NoSuchRealm"}})
	answered(t, "delete unknown realm", a, err, 400, "realm")
	store.snap.Anonymous.UserID = "demo-new@example.com"
	a, err = DeleteUser(env, w, "demo-new@example.com", url.Values{})
	answered(t, "delete anonymous user", a, err, 409)
	store.snap.Anonymous.UserID = "anonymous"
	a, err = DeleteUser(env, w, "demo-new@example.com", url.Values{"realm": {"NexusAuthenticatingRealm"}})
	answered(t, "delete by the realm the interface sends", a, err, 204)
	if userOf(store.snap, "demo-new@example.com") != nil {
		t.Fatal("the user is still stored")
	}

	a, err = ChangePassword(env, w, "viewer", []byte("example-new-1"))
	answered(t, "password", a, err, 204)
	if !auth.VerifyPassword("example-new-1", userOf(store.snap, "viewer").PasswordHash) {
		t.Fatal("the password did not change")
	}
	a, err = ChangePassword(env, w, "viewer", nil)
	answered(t, "empty password", a, err, 400, "password")
	a, err = ChangePassword(env, w, "ldap.person", []byte("p"))
	answered(t, "password of a directory user", a, err, 409)
	a, err = ChangePassword(env, w, "nobody", []byte("p"))
	answered(t, "password of nobody", a, err, 404)
}

func TestAnonymousAndRealmWrites(t *testing.T) {
	store := identityFixture()
	env, w := as(store, "admin", "nexus:*")
	a, err := SaveAnonymous(env, w, []byte(`{"enabled":true,"userId":"anonymous","realmName":"NexusAuthorizingRealm"}`))
	answered(t, "anonymous", a, err, 200)
	if got, _ := a.Body.(model.Anonymous); !got.Enabled || !store.snap.Anonymous.Enabled {
		t.Fatalf("anonymous answers %+v, stored %+v", a.Body, store.snap.Anonymous)
	}
	a, err = SaveAnonymous(env, w, []byte(`{"enabled":true,"userId":"","realmName":"NoSuchRealm"}`))
	answered(t, "anonymous refused", a, err, 400, "userId", "realmName")
	a, err = SaveAnonymous(env, w, []byte(`{"enabled":"yes"}`))
	answered(t, "anonymous wrong type", a, err, 400, "enabled")
	// I10: anonymous access does not act as a user that signs in
	for _, id := range []string{"viewer", "ldap.person"} {
		writes := store.writes
		a, err = SaveAnonymous(env, w, []byte(`{"enabled":true,"userId":"`+id+`","realmName":"NexusAuthorizingRealm"}`))
		answered(t, "anonymous as "+id, a, err, 409)
		if m, _ := a.Body.(admin.Message); !strings.Contains(m.Message, id) || store.writes != writes ||
			store.snap.Anonymous.UserID != "anonymous" {
			t.Fatalf("anonymous as %s: %+v, stored %+v", id, a.Body, store.snap.Anonymous)
		}
	}
	// I16: anonymous access on gives requests without credentials the
	// rights of its user, so the subject holds them
	store.snap.RoleMappings = append(store.snap.RoleMappings, model.RoleMapping{UserID: "demo-ghost", Source: "default",
		Roles: []string{"nx-admin"}})
	settingsEnv, settings := holding(store, local("viewer"), "nexus:settings:update,read")
	writes := store.writes
	a, err = SaveAnonymous(settingsEnv, settings, []byte(`{"enabled":true,"userId":"demo-ghost","realmName":"NexusAuthorizingRealm"}`))
	grantRefused(t, "anonymous as demo-ghost with nx-admin", store, writes, a, err, "nexus:*")
	a, err = SaveAnonymous(settingsEnv, settings, []byte(`{"enabled":false,"userId":"demo-ghost","realmName":"NexusAuthorizingRealm"}`))
	answered(t, "anonymous off as demo-ghost", a, err, 200)
	if store.snap.Anonymous.Enabled || store.snap.Anonymous.UserID != "demo-ghost" {
		t.Fatalf("anonymous off: %+v", store.snap.Anonymous)
	}
	a, err = SaveAnonymous(env, w, []byte(`{"enabled":true,"userId":"anonymous","realmName":"NexusAuthorizingRealm"}`))
	answered(t, "anonymous back", a, err, 200)
	a, err = SaveRealms(env, w, []byte(`["NexusAuthenticatingRealm","LdapRealm"]`))
	answered(t, "realms", a, err, 204)
	if !slices.Equal(store.snap.Realms, []string{"NexusAuthenticatingRealm", "LdapRealm"}) {
		t.Fatalf("realms %v", store.snap.Realms)
	}
	for _, body := range []string{``, `[]`, `{}`, `["NoSuchRealm"]`, `["DefaultRole"]`} {
		a, err = SaveRealms(env, w, []byte(body))
		answered(t, "realms "+body, a, err, 400, "*")
	}
	// I11: the local realm stays while admin is the only administrator
	writes = store.writes
	a, err = SaveRealms(env, w, []byte(`["LdapRealm","NexusAuthorizingRealm"]`))
	answered(t, "realms without the local realm", a, err, 409)
	if m, _ := a.Body.(admin.Message); !strings.Contains(m.Message, "NexusAuthenticatingRealm") || store.writes != writes {
		t.Fatalf("realms without the local realm: %+v, %d writes", a.Body, store.writes)
	}
	// I15: DefaultRole made active gives the default role to every signed
	// in user, so the subject holds its permissions
	store.snap.DefaultRole = "nx-admin"
	writes = store.writes
	a, err = SaveRealms(settingsEnv, settings, []byte(`["NexusAuthenticatingRealm","DefaultRole"]`))
	grantRefused(t, "DefaultRole with nx-admin", store, writes, a, err, "nexus:*")
	data, _ := json.Marshal(store.snap.Realms)
	if string(data) != `["NexusAuthenticatingRealm","LdapRealm"]` {
		t.Fatalf("a refusal changed the realms: %s", data)
	}
}

// holding is the write of a subject that holds the permissions, checked
// with the wildcard rule of internal/authz as the server checks them.
func holding(store *identityStore, user model.User, permissions ...string) (*Env, IdentityWrite) {
	env := &Env{Snapshot: store.snap, Permits: func(p string) bool { return authz.AnyImplies(permissions, p) }}
	return env, IdentityWrite{Store: store, Subject: &model.Subject{User: user, Permissions: permissions}}
}

func local(id string) model.User { return model.User{ID: id, Source: "default", Status: "active"} }

// grantRefused checks a 403 grant-exceeds that names the permission, and
// that the store did not change.
func grantRefused(t *testing.T, label string, store *identityStore, writes int, a Answer, err error, permission string) {
	t.Helper()
	answered(t, label, a, err, 403)
	if m, _ := a.Body.(admin.Message); !strings.Contains(m.Message, permission) || store.writes != writes {
		t.Fatalf("%s: %+v, %d writes, want %d", label, a.Body, store.writes, writes)
	}
}

// Ruling I9: no write gives a role, a user or a directory group a
// permission the acting subject does not hold.
func TestGrantsBeyondTheOwnRights(t *testing.T) {
	store := identityFixture()
	store.snap.Roles = append(store.snap.Roles,
		model.Role{ID: "demo-keeper", Source: "default", Name: "Keeper", Privileges: []string{"nx-roles-update", "nx-search-read"}})
	store.snap.Users = append(store.snap.Users, model.User{ID: "keeper", Status: "active", Source: "default",
		Roles: []string{"demo-keeper"}, PasswordHash: oldHash})
	// the permissions the server gives keeper: nx-roles-update is update,read
	keeperPermissions := authz.NewResolver(store.snap).Permissions([]string{"demo-keeper"})

	// a role save that adds nx-all to a role the subject holds
	env, w := holding(store, local("keeper"), keeperPermissions...)
	writes := store.writes
	a, err := SaveRole(env, w, "demo-keeper", []byte(`{"id":"demo-keeper","name":"Keeper","privileges":["nx-roles-update","nx-search-read","nx-all"],"roles":[]}`))
	grantRefused(t, "role save with nx-all", store, writes, a, err, "nexus:*")
	if r := store.snap.Roles[1]; len(r.Privileges) != 2 {
		t.Fatalf("the role changed: %+v", r)
	}
	// through a contained role
	a, err = SaveRole(env, w, "demo-keeper", []byte(`{"id":"demo-keeper","name":"Keeper","privileges":["nx-roles-update"],"roles":["nx-admin"]}`))
	grantRefused(t, "role save containing nx-admin", store, writes, a, err, "nexus:*")
	// a save within the own rights passes
	a, err = SaveRole(env, w, "demo-keeper", []byte(`{"id":"demo-keeper","name":"Keeper 2","privileges":["nx-roles-update","nx-search-read"],"roles":["demo-viewer"]}`))
	answered(t, "role save within the own rights", a, err, 204)
	if store.snap.Roles[1].Name != "Keeper 2" {
		t.Fatalf("the save within the own rights: %+v", store.snap.Roles[1])
	}

	// a role create named like the directory group of the subject
	ldapUser := model.User{ID: "ldap.person", Source: "LDAP", Status: "active"}
	env, w = holding(store, ldapUser, "nexus:roles:create", "nexus:search:read")
	writes = store.writes
	a, err = CreateRole(env, w, []byte(`{"id":"developers","name":"developers","privileges":["nx-all"],"roles":[]}`))
	grantRefused(t, "role create named like a directory group", store, writes, a, err, "nexus:*")
	if admin.Exists(store.snap, admin.KindRole, "developers") {
		t.Fatal("the role developers was stored")
	}
	a, err = CreateRole(env, w, []byte(`{"id":"developers","name":"developers","privileges":["nx-search-read"],"roles":[]}`))
	answered(t, "role create within the own rights", a, err, 200)

	// a password change of admin by a subject with only the change password permission
	env, w = holding(store, local("viewer"), "nexus:userschangepw:create")
	writes = store.writes
	a, err = ChangePassword(env, w, "admin", []byte("example-new-1"))
	grantRefused(t, "password of admin", store, writes, a, err, "nexus:*")
	if userOf(store.snap, "admin").PasswordHash != oldHash {
		t.Fatal("the password of admin changed")
	}
	env, w = holding(store, local("admin"), "nexus:userschangepw:create", "nexus:search:read")
	a, err = ChangePassword(env, w, "viewer", []byte("example-new-1"))
	answered(t, "password of viewer within the own rights", a, err, 204)

	// a user create or save of another user with nx-admin
	env, w = holding(store, local("keeper"), "nexus:users:*", "nexus:search:read")
	writes = store.writes
	a, err = CreateUser(env, w, []byte(`{"userId":"demo-up","status":"active","password":"example-new-1","roles":["nx-admin"]}`))
	grantRefused(t, "user create with nx-admin", store, writes, a, err, "nexus:*")
	a, err = SaveUser(env, w, "viewer", []byte(`{"userId":"viewer","status":"active","roles":["demo-viewer","nx-admin"]}`))
	grantRefused(t, "user save with nx-admin", store, writes, a, err, "nexus:*")
	a, err = SaveUser(env, w, "ldap.person", []byte(`{"userId":"ldap.person","source":"LDAP","status":"active","roles":["nx-admin"]}`))
	grantRefused(t, "directory user save with nx-admin", store, writes, a, err, "nexus:*")
	// the own roles: a subject that administers users does not give itself nx-admin
	a, err = SaveUser(env, w, "keeper", []byte(`{"userId":"keeper","status":"active","roles":["demo-keeper","nx-admin"]}`))
	grantRefused(t, "own save with nx-admin", store, writes, a, err, "nexus:*")
	a, err = CreateUser(env, w, []byte(`{"userId":"demo-up","status":"active","password":"example-new-1","roles":["demo-viewer"]}`))
	answered(t, "user create within the own rights", a, err, 200)

	// a subject that holds nexus:* passes
	env, w = holding(store, local("admin"), "nexus:*")
	a, err = SaveUser(env, w, "demo-up", []byte(`{"userId":"demo-up","status":"active","roles":["nx-admin"]}`))
	answered(t, "user save by the full permission set", a, err, 204)
}

// A10 through a role: a save that takes the full permission from the role
// of the only administrator is refused with 409, and nothing changes.
func TestLastAdministratorThroughARoleSave(t *testing.T) {
	store := identityFixture()
	store.snap.Roles = append(store.snap.Roles, model.Role{ID: "demo-super", Source: "default", Name: "Super", Privileges: []string{"nx-all"}})
	userOf(store.snap, "admin").Roles = []string{"demo-super"}
	env, w := holding(store, local("admin"), "nexus:*")
	writes := store.writes
	a, err := SaveRole(env, w, "demo-super", []byte(`{"id":"demo-super","name":"Super","privileges":[],"roles":[]}`))
	answered(t, "strip the full permission", a, err, 409)
	if m, _ := a.Body.(admin.Message); !strings.Contains(m.Message, "full permission set") || store.writes != writes ||
		!slices.Equal(store.snap.Roles[1].Privileges, []string{"nx-all"}) {
		t.Fatalf("strip the full permission: %+v, %d writes, %+v", a.Body, store.writes, store.snap.Roles[1])
	}
	// the full permission through a contained role keeps the administrator
	a, err = SaveRole(env, w, "demo-super", []byte(`{"id":"demo-super","name":"Super","privileges":[],"roles":["nx-admin"]}`))
	answered(t, "the full permission through nx-admin", a, err, 204)
	if r := store.snap.Roles[1]; len(r.Privileges) != 0 || !slices.Equal(r.Roles, []string{"nx-admin"}) {
		t.Fatalf("the save through nx-admin: %+v", r)
	}
}
