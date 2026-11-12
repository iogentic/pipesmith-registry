package state_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/model"
	"github.com/mirosso25/pipesmith-registry/internal/state"
	_ "modernc.org/sqlite"
)

// An invented hash of the Shiro 1 layout; it belongs to no real password.
const testHash = "$shiro1$SHA-512$1024$c3RhdGUtdGVzdC1zYWx0IQ==$aW52ZW50ZWQtaGFzaC1vZi10aGUtc3RhdGUtdGVzdA=="

func sample() *model.Snapshot {
	return &model.Snapshot{
		Users: []model.User{
			{ID: "admin", FirstName: "Ada", LastName: "Example", Email: "admin@example.com", Status: "active",
				Source: "default", Roles: []string{"nx-admin"}, PasswordHash: testHash},
			{ID: "viewer", FirstName: "Vic", LastName: "Example", Email: "viewer@example.com", Status: "locked",
				Source: "default", Roles: []string{"demo-viewer", "demo-extra"}, PasswordHash: ""},
		},
		Roles: []model.Role{
			{ID: "demo-lead", Source: "default", Name: "Lead", Description: "", ReadOnly: false,
				Privileges: []string{"p-b", "p-a"}, Roles: []string{"demo-viewer", "missing-role"}},
			{ID: "demo-viewer", Source: "default", Name: "Viewer", Description: "reads", ReadOnly: true,
				Privileges: []string{"nx-search-read"}, Roles: []string{}},
		},
		Privileges: []model.Privilege{
			{Name: "p-a", Type: "wildcard", Description: "all", ReadOnly: false, Properties: map[string]string{"pattern": "nexus:*"}},
			{Name: "p-b", Type: "repository-view", Description: "", ReadOnly: true,
				Properties: map[string]string{"format": "raw", "repository": "demo-raw", "actions": "read,browse"}},
		},
		RoleMappings: []model.RoleMapping{
			{UserID: "ldap.person", Source: "LDAP", Roles: []string{"demo-viewer"}},
		},
		LDAP: []model.LDAPServer{
			{ID: "b-second", Name: "second", Order: 2, Protocol: "ldaps", Host: "ldap2.example.com", Port: 636},
			{ID: "a-first", Name: "first", Order: 1, Protocol: "ldap", Host: "ldap.example.com", Port: 389,
				SearchBase: "dc=example,dc=com", AuthScheme: "SIMPLE", AuthUsername: "cn=reader,dc=example,dc=com",
				UserBaseDN: "ou=people", UserSubtree: true, LDAPGroupsAsRoles: true, GroupType: "static"},
		},
		Realms:    []string{"NexusAuthenticatingRealm", "LdapRealm"},
		Anonymous: model.Anonymous{Enabled: true, UserID: "anonymous", RealmName: "NexusAuthorizingRealm"},
		BlobStores: []model.BlobStore{
			{Name: "demo-files", Type: "File", Path: "/srv/blobs/demo-files"},
			{Name: "demo-quota", Type: "File", Path: "/srv/blobs/demo-quota",
				SoftQuota: &model.SoftQuota{Type: "spaceUsedQuota", Limit: 10737418240}},
		},
		Repositories: []model.Repository{
			{Name: "demo-docker", Format: "docker", Type: "hosted", Online: false, Attributes: map[string]any{
				"docker": map[string]any{"httpPort": json.Number("14510"), "forceBasicAuth": true}}},
			{Name: "demo-group", Format: "maven2", Type: "group", Online: true, Attributes: map[string]any{
				"group": map[string]any{"memberNames": []any{"demo-b", "demo-a"}}}},
		},
	}
}

// sorted is what Load returns for sample(): records in key order, LDAP by
// its order.
func sorted() *model.Snapshot {
	s := sample()
	s.LDAP[0], s.LDAP[1] = s.LDAP[1], s.LDAP[0]
	return s
}

func create(t *testing.T) (string, *state.Store) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	st, err := state.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return dir, st
}

func load(t *testing.T, st *state.Store) *model.Snapshot {
	t.Helper()
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func same(t *testing.T, got, want *model.Snapshot) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("snapshot differs\ngot  %s\nwant %s", g, w)
	}
	for i := range got.Users {
		if got.Users[i].PasswordHash != want.Users[i].PasswordHash {
			t.Fatalf("hash of %s differs", got.Users[i].ID)
		}
	}
}

func TestOpenNeverCreates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	st, err := state.Open(dir)
	if !errors.Is(err, state.ErrAbsent) || st != nil {
		t.Fatalf("Open of an empty data directory: %v, %v; want ErrAbsent", st, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open created %s: %v", dir, err)
	}
}

func TestCreateModesAndSchemaRow(t *testing.T) {
	dir, st := create(t)
	if err := st.Write(state.Kinds, sample()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o750 {
		t.Fatalf("data directory mode %o, want 750", perm)
	}
	for _, name := range []string{state.FileName, state.FileName + "-wal", state.FileName + "-shm"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) && name != state.FileName {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s mode %o, want 600", name, perm)
		}
	}
	db := raw(t, dir)
	var version int
	if err := db.QueryRow("SELECT value FROM meta WHERE key = 'schema'").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != state.SchemaVersion || state.SchemaVersion != 1 {
		t.Fatalf("schema row %d, constant %d; want 1", version, state.SchemaVersion)
	}
	var tables []string
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	want := "blob_stores ldap_servers meta privileges repositories role_privileges role_roles roles settings user_roles users"
	if strings.Join(tables, " ") != want {
		t.Fatalf("tables %v, want %s", tables, want)
	}
}

func raw(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, state.FileName)+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestWriteLoadRoundTrip(t *testing.T) {
	dir, st := create(t)
	if err := st.Write(state.Kinds, sample()); err != nil {
		t.Fatal(err)
	}
	same(t, load(t, st), sorted())
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	same(t, load(t, again), sorted())
}

func TestEmptyStateLoadsEmptyLists(t *testing.T) {
	_, st := create(t)
	got := load(t, st)
	want := &model.Snapshot{Users: []model.User{}, Roles: []model.Role{}, Privileges: []model.Privilege{},
		RoleMappings: []model.RoleMapping{}, LDAP: []model.LDAPServer{}, Realms: []string{},
		BlobStores: []model.BlobStore{}, Repositories: []model.Repository{}}
	same(t, got, want)
}

func TestWriteReplacesOnlyTheKindsNamed(t *testing.T) {
	_, st := create(t)
	if err := st.Write(state.Kinds, sample()); err != nil {
		t.Fatal(err)
	}
	only := &model.Snapshot{Repositories: []model.Repository{
		{Name: "real-list", Format: "raw", Type: "hosted", Online: true, Attributes: map[string]any{}}}}
	if err := st.Write([]state.Kind{state.KindRepositories}, only); err != nil {
		t.Fatal(err)
	}
	want := sorted()
	want.Repositories = only.Repositories
	same(t, load(t, st), want)

	identity := sample()
	identity.Users = identity.Users[:1]
	identity.Repositories = nil
	identity.RoleMappings = nil
	if err := st.Write([]state.Kind{state.KindUsers}, identity); err != nil {
		t.Fatal(err)
	}
	want.Users = want.Users[:1]
	want.RoleMappings = []model.RoleMapping{}
	same(t, load(t, st), want)
}

func TestWriteRefusesUnknownOrNoKind(t *testing.T) {
	_, st := create(t)
	if err := st.Write(nil, sample()); err == nil {
		t.Fatal("Write with no kind succeeded")
	}
	if err := st.Write([]state.Kind{"tokens"}, sample()); err == nil || !strings.Contains(err.Error(), "tokens") {
		t.Fatalf("Write of an unknown kind: %v", err)
	}
}

// A failing write leaves every table as it was, and its error names no hash.
func TestWriteIsOneTransaction(t *testing.T) {
	_, st := create(t)
	if err := st.Write(state.Kinds, sample()); err != nil {
		t.Fatal(err)
	}
	broken := sample()
	broken.Users[1].ID = broken.Users[0].ID
	broken.Repositories = broken.Repositories[:1]
	err := st.Write([]state.Kind{state.KindRepositories, state.KindUsers}, broken)
	if err == nil {
		t.Fatal("a snapshot with two users of one id was written")
	}
	if strings.Contains(err.Error(), testHash) || strings.Contains(fmt.Sprintf("%+v", err), "shiro1") {
		t.Fatal("the error of a failed write carries the hash")
	}
	same(t, load(t, st), sorted())
}

// A role mapping of a local user's own source would hide the user's roles.
func TestWriteRefusesMappingOfLocalSource(t *testing.T) {
	_, st := create(t)
	s := sample()
	s.RoleMappings = append(s.RoleMappings, model.RoleMapping{UserID: "admin", Source: "default", Roles: []string{"x"}})
	if err := st.Write(state.Kinds, s); err == nil {
		t.Fatal("a role mapping that duplicates the roles of a local user was written")
	}
}

func TestNewerSchemaIsRefused(t *testing.T) {
	dir, st := create(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := raw(t, dir).Exec("UPDATE meta SET value = 2 WHERE key = 'schema'"); err != nil {
		t.Fatal(err)
	}
	for _, open := range []func(string) (*state.Store, error){state.Open, state.Create} {
		got, err := open(dir)
		if err == nil {
			_ = got.Close()
			t.Fatal("a file of schema 2 was opened")
		}
		if !strings.Contains(err.Error(), "schema 2") || !strings.Contains(err.Error(), "schema 1") {
			t.Fatalf("refusal %q does not name both versions", err)
		}
	}
}

func TestOpenRefusesAFileWithoutMeta(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, state.FileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db := raw(t, dir)
	if _, err := db.Exec("CREATE TABLE other (x)"); err != nil {
		t.Fatal(err)
	}
	if st, err := state.Open(dir); err == nil {
		_ = st.Close()
		t.Fatal("a SQLite file without the meta table was opened as state")
	}
}

func TestPathAndFileName(t *testing.T) {
	if state.FileName != "state.sqlite" || state.Path("/d") != filepath.Join("/d", "state.sqlite") {
		t.Fatalf("FileName %q, Path %q", state.FileName, state.Path("/d"))
	}
}
