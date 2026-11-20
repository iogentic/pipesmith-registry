package admin

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/authz"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

func TestIdentityVectors(t *testing.T) {
	var file struct {
		Family       string              `json:"family"`
		Note         string              `json:"note"`
		Users        []model.User        `json:"users"`
		Hashed       []string            `json:"hashed"`
		Roles        []model.Role        `json:"roles"`
		Privileges   []model.Privilege   `json:"privileges"`
		RoleMappings []model.RoleMapping `json:"roleMappings"`
		Anonymous    model.Anonymous     `json:"anonymous"`
		DefaultRole  string              `json:"defaultRole"`
		Cases        []struct {
			Row         string           `json:"row"`
			Op          string           `json:"op"`
			ID          string           `json:"id"`
			Source      string           `json:"source"`
			User        *model.User      `json:"user"`
			Body        *model.User      `json:"body"`
			Result      *model.User      `json:"result"`
			Own         bool             `json:"own"`
			Administers bool             `json:"administers"`
			Password    string           `json:"password"`
			Repeat      int              `json:"repeat"`
			Role        *model.Role      `json:"role"`
			Anonymous   *model.Anonymous `json:"anonymous"`
			Stored      *model.Anonymous `json:"stored"`
			What        string           `json:"what"`
			Roles       []string         `json:"roles"`
			Held        []string         `json:"held"`
			Realm       *string          `json:"realm"`
			StoredRealm []string         `json:"storedRealms"`
			Realms      []string         `json:"realms"`
			DefaultRole string           `json:"defaultRole"`
			Refusals    *[]wantRefusal   `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "identity", &file)
	counts := tally{}
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s %s)", i, c.Row, c.Op, c.ID)
		s := &model.Snapshot{Users: slices.Clone(file.Users), Roles: slices.Clone(file.Roles), Privileges: file.Privileges,
			RoleMappings: file.RoleMappings, Anonymous: file.Anonymous, DefaultRole: file.DefaultRole}
		for i := range s.Users {
			if slices.Contains(file.Hashed, s.Users[i].ID) {
				s.Users[i].PasswordHash = "$shiro1$SHA-512$1024$c2FsdA==$aGFzaA=="
			}
		}
		password := c.Password
		if c.Repeat > 0 {
			password = strings.Repeat(c.Password, c.Repeat)
		}
		var err error
		switch {
		case c.Op == "user-create" && c.User != nil:
			err = CheckUserCreate(s, *c.User, password)
		case c.Op == "password":
			err = CheckPassword(password)
		case c.Op == "role-in-use":
			err = CheckRoleInUse(s, c.ID)
		case c.Op == "role-create" && c.Role != nil:
			err = returns(t, label, func() error { return CheckRoleCreate(s, *c.Role) })
		case c.Op == "role-save" && c.Role != nil:
			err = returns(t, label, func() error { return CheckRoleSave(s, c.ID, *c.Role) })
		case c.Op == "user-save" && c.Body != nil:
			found := slices.IndexFunc(s.Users, func(u model.User) bool { return u.ID == c.ID && u.Source == c.Source })
			if found < 0 {
				t.Fatalf("%s: the file holds no such user", label)
			}
			var saved model.User
			saved, err = SaveUser(s, s.Users[found], *c.Body, c.Own, c.Administers)
			if saved.PasswordHash != s.Users[found].PasswordHash {
				t.Errorf("%s: the save changed the stored form of the password", label)
			}
			saved.PasswordHash = ""
			if err == nil && (c.Result == nil || !reflect.DeepEqual(saved, *c.Result)) {
				t.Errorf("%s: saved %+v, want %+v", label, saved, c.Result)
			}
		case c.Op == "anonymous" && c.Anonymous != nil:
			err = CheckAnonymous(*c.Anonymous, file.Anonymous)
		case c.Op == "anonymous-user" && c.Anonymous != nil:
			stored := file.Anonymous
			if c.Stored != nil {
				stored = *c.Stored
			}
			err = CheckAnonymousUser(s, *c.Anonymous, stored)
		case c.Op == "user-realm" && c.Realm != nil:
			var source string
			source, err = UserSource(*c.Realm)
			if err == nil && source != c.Source {
				t.Errorf("%s: source %q, want %q", label, source, c.Source)
			}
		case c.Op == "realm-administrators":
			if c.User != nil {
				i := slices.IndexFunc(s.Users, func(u model.User) bool { return u.ID == c.User.ID })
				s.Users[i] = *c.User
			}
			err = CheckRealmAdministrators(s, c.Realms)
		case c.Op == "anonymous-grant" && c.Anonymous != nil:
			if c.Role != nil {
				s.Roles = slices.DeleteFunc(s.Roles, func(r model.Role) bool { return r.ID == c.Role.ID })
				s.Roles = append(s.Roles, *c.Role)
			}
			err = CheckAnonymousGrant(s, *c.Anonymous, func(p string) bool { return authz.AnyImplies(c.Held, p) })
		case c.Op == "default-role-grant":
			if c.Role != nil {
				s.Roles = slices.DeleteFunc(s.Roles, func(r model.Role) bool { return r.ID == c.Role.ID })
				s.Roles = append(s.Roles, *c.Role)
			}
			s.Realms = c.StoredRealm
			err = CheckDefaultRoleGrant(s, c.Realms, func(p string) bool { return authz.AnyImplies(c.Held, p) })
		case c.Op == "grant":
			if c.Role != nil {
				s.Roles = slices.DeleteFunc(s.Roles, func(r model.Role) bool { return r.ID == c.Role.ID })
				s.Roles = append(s.Roles, *c.Role)
			}
			err = CheckGrant(s, c.What, c.Roles, func(p string) bool { return authz.AnyImplies(c.Held, p) })
		case c.Op == "anonymous-roles":
			err = CheckAnonymousRoles(s, c.ID)
		case c.Op == "realms":
			err = errors.Join(CheckRealmList(c.Realms), CheckRealmRole(c.Realms, c.DefaultRole))
		default:
			t.Fatalf("%s: unknown op, or a key missing", label)
		}
		checkRefusals(t, label, err, c.Refusals)
		if c.Refusals != nil {
			counts.add(c.Row, len(*c.Refusals) > 0)
		}
	}
	counts.check(t, "identity", "I1", "I4", "I5", "I7", "S22", "S25", "R9", "N16", "I10", "I9", "I11", "I12", "I15", "I16")
}

func TestLocalUser(t *testing.T) {
	s := &model.Snapshot{Users: []model.User{{ID: "demo-a", Source: "LDAP"}, {ID: "demo-a", Source: "default", Status: "active"}}}
	u, err := LocalUser(s, "demo-a")
	if err != nil || u.Source != "default" {
		t.Fatalf("LocalUser: %+v %v", u, err)
	}
	u.Status = "disabled"
	if s.Users[1].Status != "disabled" {
		t.Fatal("LocalUser gives no pointer into the snapshot")
	}
	_, err = LocalUser(s, "demo-b")
	var r *Refusal
	if !errors.As(err, &r) || r.Code != CodeNotFound || !strings.Contains(r.Message, "no local user demo-b") {
		t.Fatalf("missing user: %v", err)
	}
}

func TestAnonymousRolesOfRowAndMapping(t *testing.T) {
	s := &model.Snapshot{Users: []model.User{{ID: "demo-row", Source: "default", Status: "disabled", Roles: []string{"a"}}}}
	SetAnonymousRoles(s, "demo-row", []string{"b"})
	SetAnonymousRoles(s, "demo-guest", []string{"c"})
	SetAnonymousRoles(s, "demo-guest", []string{"d"})
	if roles, active := AnonymousRoles(s, "demo-row"); !slices.Equal(roles, []string{"b"}) || active {
		t.Fatalf("row: %v %v", roles, active)
	}
	if roles, active := AnonymousRoles(s, "Demo-Guest"); !slices.Equal(roles, []string{"d"}) || !active || len(s.RoleMappings) != 1 {
		t.Fatalf("mapping: %v %v %+v", roles, active, s.RoleMappings)
	}
}

// fakeStore is IdentityStore over a snapshot in memory, with the counts of
// its writes and of the resets of the credential cache.
type fakeStore struct {
	snap                 *model.Snapshot
	writes, reset, stale int
}

func (f *fakeStore) Load() (*model.Snapshot, error) { return cloneIdentity(f.snap), nil }
func (f *fakeStore) DataDir() string                { return "" }
func (f *fakeStore) ResetCredentials()              { f.reset++ }
func (f *fakeStore) MarkStale()                     { f.stale++ }
func (f *fakeStore) Update(kinds []state.Kind, fn func(*model.Snapshot) error) error {
	s := cloneIdentity(f.snap)
	if err := fn(s); err != nil {
		return err
	}
	f.snap, f.writes = s, f.writes+1
	return nil
}

func TestChange(t *testing.T) {
	store := &fakeStore{snap: &model.Snapshot{Users: []model.User{
		{ID: "admin", Source: "default", Status: "active", Roles: []string{"nx-admin"}},
		{ID: "viewer", Source: "default", Status: "active", Roles: []string{}}}}}
	disable := func(id string) func(*model.Snapshot) error {
		return func(s *model.Snapshot) error {
			u, err := LocalUser(s, id)
			if err == nil {
				u.Status = "disabled"
			}
			return err
		}
	}
	after, err := Change(store, []state.Kind{state.KindUsers}, disable("viewer"))
	if err != nil || store.writes != 1 || store.reset != 1 || store.stale != 1 || after.Users[1].Status != "disabled" {
		t.Fatalf("a change: %v, %d writes, %d resets, %d stale marks, %+v", err, store.writes, store.reset, store.stale, after)
	}
	// A10: the last administrator is not disabled, and nothing is written
	_, err = Change(store, []state.Kind{state.KindUsers}, disable("admin"))
	if status, _ := Answer(err); status != 409 || store.writes != 1 || store.reset != 1 || store.stale != 1 ||
		store.snap.Users[0].Status != "active" {
		t.Fatalf("last administrator: %v (%d), %d writes, %d resets", err, status, store.writes, store.reset)
	}
	// the roles taken in place count as well
	_, err = Change(store, []state.Kind{state.KindUsers}, func(s *model.Snapshot) error {
		s.Users[0].Roles[0] = "nx-anonymous"
		return nil
	})
	if status, _ := Answer(err); status != 409 || store.snap.Users[0].Roles[0] != "nx-admin" {
		t.Fatalf("roles changed in place: %v (%d), %+v", err, status, store.snap.Users[0])
	}
	// an error of the change passes unchanged
	boom := errors.New("boom")
	if _, err := Change(store, []state.Kind{state.KindUsers}, func(*model.Snapshot) error { return boom }); err != boom || store.reset != 1 {
		t.Fatalf("error of the change: %v, %d resets", err, store.reset)
	}
}
