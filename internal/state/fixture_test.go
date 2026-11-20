package state

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	testflag "flag" // state.go has a function flag
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The fixtures in testdata/state: v<N>.sqlite is a state file of schema N
// with invented rows. v1.sqlite is a state file as release 0.1.1 writes it,
// v2.sqlite one as release 0.1.3 writes it, v3.sqlite one with the routing
// tables of step 3 (phase 5), v4.sqlite one with the imported keys of step 4. The generator below holds each
// schema as its release creates it and writes the rows with plain SQL, so
// the steps of migrations.go never change a fixture. The files are written
// again with
//
//	tools/go.sh run 'go test ./internal/state -run "^TestFixturesAreTheirGeneratorOutput$" -write-fixture'
var writeFixture = testflag.Bool("write-fixture", false, "write testdata/state/v<N>.sqlite from its generator")

const fixtures = "../../testdata/state"

// schemaV1 is schema 1 as release 0.1.1 creates it, the meta table first.
var schemaV1 = []string{
	"CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, first_name TEXT NOT NULL, last_name TEXT NOT NULL," +
		" email TEXT NOT NULL, status TEXT NOT NULL, source TEXT NOT NULL, password_hash TEXT NOT NULL) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS user_roles (user_id TEXT NOT NULL, source TEXT NOT NULL, position INTEGER NOT NULL," +
		" role_id TEXT NOT NULL, PRIMARY KEY (user_id, source, position)) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS roles (id TEXT PRIMARY KEY, source TEXT NOT NULL, name TEXT NOT NULL," +
		" description TEXT NOT NULL, read_only INTEGER NOT NULL) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS role_privileges (role_id TEXT NOT NULL REFERENCES roles (id) ON DELETE CASCADE," +
		" position INTEGER NOT NULL, privilege_id TEXT NOT NULL, PRIMARY KEY (role_id, position)) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS role_roles (role_id TEXT NOT NULL REFERENCES roles (id) ON DELETE CASCADE," +
		" position INTEGER NOT NULL, member_id TEXT NOT NULL, PRIMARY KEY (role_id, position)) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS privileges (name TEXT PRIMARY KEY, type TEXT NOT NULL, description TEXT NOT NULL," +
		" read_only INTEGER NOT NULL, properties TEXT NOT NULL) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS ldap_servers (id TEXT PRIMARY KEY, name TEXT NOT NULL, position INTEGER NOT NULL," +
		" record TEXT NOT NULL) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS blob_stores (name TEXT PRIMARY KEY, type TEXT NOT NULL, path TEXT NOT NULL," +
		" soft_quota TEXT) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS repositories (name TEXT PRIMARY KEY, format TEXT NOT NULL, type TEXT NOT NULL," +
		" online INTEGER NOT NULL, attributes TEXT NOT NULL) WITHOUT ROWID",
}

// schemaV2 is what step 2 adds as release 0.1.3 creates it: the tokens and
// the generation row.
var schemaV2 = []string{
	"CREATE TABLE IF NOT EXISTS tokens (public TEXT NOT NULL UNIQUE, kind TEXT NOT NULL, user_id TEXT NOT NULL," +
		" source TEXT NOT NULL, digest BLOB NOT NULL, created INTEGER NOT NULL, last_used INTEGER)",
	"CREATE INDEX IF NOT EXISTS tokens_owner ON tokens (user_id, source, kind)",
	"INSERT INTO meta VALUES ('generation', 0) ON CONFLICT (key) DO NOTHING",
}

// schemaV3 is what step 3 adds: the routing rules and the links of the
// repositories to them.
var schemaV3 = []string{
	"CREATE TABLE IF NOT EXISTS routing_rules (name TEXT PRIMARY KEY, description TEXT NOT NULL, mode TEXT NOT NULL," +
		" matchers TEXT NOT NULL) WITHOUT ROWID",
	"CREATE TABLE IF NOT EXISTS routing_links (repository TEXT PRIMARY KEY, rule TEXT NOT NULL) WITHOUT ROWID",
}

// schemaV4 is what step 4 adds: the keys an import took over.
var schemaV4 = []string{
	"CREATE TABLE IF NOT EXISTS imported_keys (digest BLOB PRIMARY KEY, kind TEXT NOT NULL," +
		" user_id TEXT NOT NULL, source TEXT NOT NULL, created INTEGER NOT NULL, last_used INTEGER) WITHOUT ROWID",
}

// rowsV4 are the rows v4.sqlite holds beyond those of v3.sqlite: one
// imported npm key of demo-admin with an invented digest. Load gives none.
var rowsV4 = []struct {
	table  string
	values []any
}{
	{"imported_keys", []any{
		[]byte{0x5a, 0x11, 0x0e, 0x93, 0x27, 0xc4, 0x80, 0x6b, 0x3d, 0xf2, 0x19, 0xa7, 0x44, 0x08, 0xbe, 0x71,
			0xcc, 0x02, 0x6d, 0x95, 0xe3, 0x4a, 0x1f, 0xb8, 0x70, 0x2e, 0xd6, 0x59, 0x13, 0x8f, 0xa4, 0x36},
		"npm", "demo-admin", "default", int64(1795121000000), nil}},
}

// rowsV3 are the rows v3.sqlite holds beyond those of v2.sqlite: one
// routing rule and the link of demo-docker to it. Load gives neither.
var rowsV3 = []struct {
	table  string
	values []any
}{
	{"routing_rules", []any{"demo-block-internal", "Keeps the internal names off the remote", "BLOCK",
		`["^/com/example/internal/.*"]`}},
	{"routing_links", []any{"demo-docker", "demo-block-internal"}},
}

// rowsV2 are the rows v2.sqlite holds beyond those of v1.sqlite: one npm
// token of demo-admin with invented digest bytes. The generation stays 0,
// as in a fresh file, so that the schema comparison holds.
var rowsV2 = []struct {
	table  string
	values []any
}{
	{"tokens", []any{"AAAAAAAAAAA", "npm", "demo-admin", "default",
		[]byte{0x0b, 0x2f, 0xc3, 0x5c, 0x8e, 0x4e, 0x86, 0x58, 0x62, 0x3c, 0x1c, 0x1a, 0x8e, 0x8b, 0xe8, 0xf5,
			0x97, 0x1d, 0x9b, 0x21, 0x0c, 0x9c, 0xb5, 0x55, 0x50, 0x88, 0xe1, 0x28, 0x6f, 0x23, 0x0a, 0x93},
		int64(1795120000000), int64(1795120600000)}},
}

// adminHashV1 is the Shiro 1 hash (SHA-512, 1024 rounds) of the invented
// password adminPasswordV1 of the fixture user demo-admin.
const (
	adminHashV1     = "$shiro1$SHA-512$1024$aVCupuZtQaCVizihjEss6A==$i/uxTUmM1MSLO0DHH93gTZJO8CXjclDbziswFD15y2cWkDNYAOD9juFj1PKYU0JuDwdkmt5CdPeZ57IFdsWgVA=="
	adminPasswordV1 = "example-v1-admin"
)

const ldapRecordV1 = `{"id":"demo-ldap-1","name":"demo-directory","order":1,"protocol":"ldaps","useTrustStore":false,` +
	`"host":"ldap.example.com","port":636,"searchBase":"dc=example,dc=com","authScheme":"SIMPLE","authRealm":"",` +
	`"authUsername":"cn=reader,dc=example,dc=com","connectionTimeoutSeconds":30,"connectionRetryDelaySeconds":300,` +
	`"maxIncidentsCount":3,"userBaseDn":"ou=people","userSubtree":true,"userObjectClass":"inetOrgPerson",` +
	`"userLdapFilter":"","userIdAttribute":"uid","userRealNameAttribute":"cn","userEmailAddressAttribute":"mail",` +
	`"userPasswordAttribute":"","ldapGroupsAsRoles":true,"groupType":"static","groupBaseDn":"ou=groups",` +
	`"groupSubtree":false,"groupObjectClass":"groupOfUniqueNames","groupIdAttribute":"cn",` +
	`"groupMemberAttribute":"uniqueMember","groupMemberFormat":"uid=${username},ou=people,dc=example,dc=com",` +
	`"userMemberOfAttribute":""}`

// rowsV1 are the invented rows of v1.sqlite in the order and the encoding
// release 0.1.1 writes them: flags as 0 or 1, lists as positions, settings,
// properties, records and attributes as JSON, no soft quota as NULL.
var rowsV1 = []struct {
	table  string
	values []any
}{
	{"users", []any{"demo-admin", "Ada", "Admin", "demo-admin@example.com", "active", "default", adminHashV1}},
	{"user_roles", []any{"demo-admin", "default", 0, "demo-lead"}},
	{"users", []any{"demo-reader", "Rex", "Reader", "demo-reader@example.com", "locked", "default", ""}},
	{"user_roles", []any{"demo-reader", "default", 0, "demo-viewer"}},
	{"user_roles", []any{"demo-reader", "default", 1, "demo-extra"}},
	{"user_roles", []any{"dir.person", "LDAP", 0, "demo-viewer"}},
	{"roles", []any{"demo-lead", "default", "Lead", "", 0}},
	{"role_privileges", []any{"demo-lead", 0, "demo-all"}},
	{"role_roles", []any{"demo-lead", 0, "demo-viewer"}},
	{"role_roles", []any{"demo-lead", 1, "missing-role"}},
	{"roles", []any{"demo-viewer", "default", "Viewer", "reads every repository", 1}},
	{"role_privileges", []any{"demo-viewer", 0, "demo-read"}},
	{"role_privileges", []any{"demo-viewer", 1, "demo-browse"}},
	{"privileges", []any{"demo-all", "wildcard", "every permission", 0, `{"pattern":"demo:*"}`}},
	{"privileges", []any{"demo-browse", "repository-view", "", 1,
		`{"actions":"browse","format":"raw","repository":"demo-files-raw"}`}},
	{"privileges", []any{"demo-read", "repository-view", "read every repository", 1,
		`{"actions":"read","format":"*","repository":"*"}`}},
	{"ldap_servers", []any{"demo-ldap-1", "demo-directory", 1, ldapRecordV1}},
	{"settings", []any{"realms", `["demo-local-realm","demo-directory-realm"]`}},
	{"settings", []any{"anonymous", `{"enabled":false,"userId":"anonymous","realmName":"demo-local-realm"}`}},
	{"blob_stores", []any{"demo-files", "File", "/srv/blobs/demo-files", nil}},
	{"blob_stores", []any{"demo-quota", "File", "/srv/blobs/demo-quota", `{"type":"spaceUsedQuota","limit":10737418240}`}},
	{"repositories", []any{"demo-docker", "docker", "hosted", 0,
		`{"docker":{"forceBasicAuth":true,"httpPort":14399.0,"v1Enabled":false},` +
			`"storage":{"blobStoreName":"demo-files","strictContentTypeValidation":true,"writePolicy":"ALLOW_ONCE"}}`}},
	{"repositories", []any{"demo-group", "maven2", "group", 1,
		`{"group":{"memberNames":["demo-hosted","demo-files-raw"]},"storage":{"blobStoreName":"demo-files"}}`}},
	{"repositories", []any{"demo-hosted", "maven2", "hosted", 1,
		`{"maven":{"layoutPolicy":"STRICT","versionPolicy":"RELEASE"},` +
			`"storage":{"blobStoreName":"demo-quota","writePolicy":"ALLOW"}}`}},
}

// recordsV1 is what Load gives for the rows of v1.sqlite.
func recordsV1() *model.Snapshot {
	return &model.Snapshot{
		Users: []model.User{
			{ID: "demo-admin", FirstName: "Ada", LastName: "Admin", Email: "demo-admin@example.com", Status: "active",
				Source: "default", Roles: []string{"demo-lead"}, PasswordHash: adminHashV1},
			{ID: "demo-reader", FirstName: "Rex", LastName: "Reader", Email: "demo-reader@example.com", Status: "locked",
				Source: "default", Roles: []string{"demo-viewer", "demo-extra"}},
		},
		RoleMappings: []model.RoleMapping{{UserID: "dir.person", Source: "LDAP", Roles: []string{"demo-viewer"}}},
		Roles: []model.Role{
			{ID: "demo-lead", Source: "default", Name: "Lead", Privileges: []string{"demo-all"},
				Roles: []string{"demo-viewer", "missing-role"}},
			{ID: "demo-viewer", Source: "default", Name: "Viewer", Description: "reads every repository", ReadOnly: true,
				Privileges: []string{"demo-read", "demo-browse"}, Roles: []string{}},
		},
		Privileges: []model.Privilege{
			{Name: "demo-all", Type: "wildcard", Description: "every permission", Properties: map[string]string{"pattern": "demo:*"}},
			{Name: "demo-browse", Type: "repository-view", ReadOnly: true,
				Properties: map[string]string{"actions": "browse", "format": "raw", "repository": "demo-files-raw"}},
			{Name: "demo-read", Type: "repository-view", Description: "read every repository", ReadOnly: true,
				Properties: map[string]string{"actions": "read", "format": "*", "repository": "*"}},
		},
		LDAP: []model.LDAPServer{{ID: "demo-ldap-1", Name: "demo-directory", Order: 1, Protocol: "ldaps",
			Host: "ldap.example.com", Port: 636, SearchBase: "dc=example,dc=com", AuthScheme: "SIMPLE",
			AuthUsername: "cn=reader,dc=example,dc=com", ConnectionTimeoutSeconds: 30, ConnectionRetryDelaySeconds: 300,
			MaxIncidentsCount: 3, UserBaseDN: "ou=people", UserSubtree: true, UserObjectClass: "inetOrgPerson",
			UserIDAttribute: "uid", UserRealNameAttribute: "cn", UserEmailAddressAttribute: "mail", LDAPGroupsAsRoles: true,
			GroupType: "static", GroupBaseDN: "ou=groups", GroupObjectClass: "groupOfUniqueNames", GroupIDAttribute: "cn",
			GroupMemberAttribute: "uniqueMember", GroupMemberFormat: "uid=${username},ou=people,dc=example,dc=com"}},
		Realms:    []string{"demo-local-realm", "demo-directory-realm"},
		Anonymous: model.Anonymous{UserID: "anonymous", RealmName: "demo-local-realm"},
		BlobStores: []model.BlobStore{
			{Name: "demo-files", Type: "File", Path: "/srv/blobs/demo-files"},
			{Name: "demo-quota", Type: "File", Path: "/srv/blobs/demo-quota",
				SoftQuota: &model.SoftQuota{Type: "spaceUsedQuota", Limit: 10737418240}},
		},
		Repositories: []model.Repository{
			{Name: "demo-docker", Format: "docker", Type: "hosted", Attributes: map[string]any{
				"docker": map[string]any{"forceBasicAuth": true, "httpPort": json.Number("14399.0"), "v1Enabled": false},
				"storage": map[string]any{"blobStoreName": "demo-files", "strictContentTypeValidation": true,
					"writePolicy": "ALLOW_ONCE"}}},
			{Name: "demo-group", Format: "maven2", Type: "group", Online: true, Attributes: map[string]any{
				"group":   map[string]any{"memberNames": []any{"demo-hosted", "demo-files-raw"}},
				"storage": map[string]any{"blobStoreName": "demo-files"}}},
			{Name: "demo-hosted", Format: "maven2", Type: "hosted", Online: true, Attributes: map[string]any{
				"maven":   map[string]any{"layoutPolicy": "STRICT", "versionPolicy": "RELEASE"},
				"storage": map[string]any{"blobStoreName": "demo-quota", "writePolicy": "ALLOW"}}},
		},
		CleanupPolicies: []model.CleanupPolicy{},
		Tasks:           []model.Task{},
		Capabilities:    []model.Capability{},
	}
}

// fixtureRecords are the records Load gives for each fixture, by version.
// The token of v2.sqlite, the routing rows of v3.sqlite and the imported
// key of v4.sqlite are no records of Load.
var fixtureRecords = map[int]func() *model.Snapshot{1: recordsV1, 2: recordsV1, 3: recordsV1, 4: recordsV1}

// fixtureVersions are the schemas of the committed fixtures.
var fixtureVersions = []int{1, 2, 3, 4}

// generateFixture writes v<version>.sqlite at path, which must not exist:
// WAL mode, the schema up to version and the schema row in one
// transaction, the rows in a second one.
func generateFixture(path string, version int) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	name, err := dsn(path, url.Values{"mode": {"rw"}, "_pragma": {"foreign_keys(1)"}})
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", name)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	inTx := func(fn func(*sql.Tx) error) error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	_, err = db.Exec("PRAGMA journal_mode = WAL")
	if err == nil {
		err = inTx(func(tx *sql.Tx) error {
			statements := slices.Clone(schemaV1)
			if version >= 2 {
				statements = append(statements, schemaV2...)
			}
			if version >= 3 {
				statements = append(statements, schemaV3...)
			}
			if version >= 4 {
				statements = append(statements, schemaV4...)
			}
			for _, statement := range statements {
				if _, err := tx.Exec(statement); err != nil {
					return err
				}
			}
			_, err := tx.Exec("INSERT INTO meta VALUES ('schema', ?)", version)
			return err
		})
	}
	if err == nil {
		err = inTx(func(tx *sql.Tx) error {
			rows := slices.Clone(rowsV1)
			if version >= 2 {
				rows = append(rows, rowsV2...)
			}
			if version >= 3 {
				rows = append(rows, rowsV3...)
			}
			if version >= 4 {
				rows = append(rows, rowsV4...)
			}
			for _, row := range rows {
				marks := strings.TrimSuffix(strings.Repeat("?, ", len(row.values)), ", ")
				if _, err := tx.Exec("INSERT INTO "+row.table+" VALUES ("+marks+")", row.values...); err != nil {
					return fmt.Errorf("%s %v: %w", row.table, row.values[0], err)
				}
			}
			return nil
		})
	}
	// the last connection to close folds the WAL into the file
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	return err
}

// copied copies the file at from to a new file of mode 0600 at to. A fixture
// is never opened where it lies, so that no WAL file appears beside it.
func copied(t *testing.T, from, to string) string {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return to
}

// dataDirOf makes a data directory whose state.sqlite is a copy of the
// fixture name.
func dataDirOf(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	copied(t, filepath.Join(fixtures, name), Path(dir))
	return dir
}

// dump is the text of a SQLite file: every object of the schema with its SQL
// and, with rows, every row of every table, sorted, with its value types.
func dump(t *testing.T, path string, rows bool) string {
	t.Helper()
	name, err := dsn(path, url.Values{"mode": {"ro"}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", name)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out []string
	var tables []string
	objects, err := db.Query("SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_master ORDER BY type, name")
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	for objects.Next() {
		var kind, name, table, text string
		if err := objects.Scan(&kind, &name, &table, &text); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s on %s: %s", kind, name, table, text))
		if kind == "table" {
			tables = append(tables, name)
		}
	}
	if err := objects.Close(); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		if !rows && table != "meta" {
			continue
		}
		out = append(out, "rows of "+table)
		var lines []string
		r, err := db.Query("SELECT * FROM " + strconv.Quote(table))
		if err != nil {
			t.Fatal(err)
		}
		columns, err := r.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for r.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := r.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			cells := make([]string, len(values))
			for i, v := range values {
				switch v := v.(type) {
				case nil:
					cells[i] = "NULL"
				case []byte:
					cells[i] = "blob " + hex.EncodeToString(v)
				case string:
					cells[i] = "text " + strconv.Quote(v)
				default:
					cells[i] = fmt.Sprintf("%T %v", v, v)
				}
			}
			lines = append(lines, "  "+strings.Join(cells, ", "))
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		slices.Sort(lines)
		out = append(out, lines...)
	}
	return strings.Join(out, "\n")
}

// keptTables are the tables of each schema whose rows the records of
// fixtureRecords do not cover: the tokens of step 2, the routing rules of
// step 3 and the imported keys of step 4. A migration keeps their rows as
// they are.
var keptTables = map[int][]string{2: {"tokens"}, 3: {"tokens", "routing_rules", "routing_links"},
	4: {"tokens", "routing_rules", "routing_links", "imported_keys"}}

// keptRows are the rows of the kept tables of the file at path, by table.
func keptRows(t *testing.T, path string, version int) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range keptTables[version] {
		out[table] = tableRows(t, path, table)
	}
	return out
}

func sameRecords(t *testing.T, got, want *model.Snapshot) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("records differ\ngot  %s\nwant %s", g, w)
	}
	for i := range got.Users {
		if got.Users[i].PasswordHash != want.Users[i].PasswordHash {
			t.Fatalf("the hash of %s differs", got.Users[i].ID)
		}
	}
}

// Each committed fixture holds what its generator writes.
func TestFixturesAreTheirGeneratorOutput(t *testing.T) {
	for _, version := range fixtureVersions {
		name := "v" + strconv.Itoa(version) + ".sqlite"
		committed := filepath.Join(fixtures, name)
		if *writeFixture {
			if err := os.Remove(committed); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := generateFixture(committed, version); err != nil {
				t.Fatal(err)
			}
		}
		fresh := filepath.Join(t.TempDir(), name)
		if err := generateFixture(fresh, version); err != nil {
			t.Fatal(err)
		}
		got := dump(t, copied(t, committed, filepath.Join(t.TempDir(), name)), true)
		if want := dump(t, fresh, true); got != want {
			t.Fatalf("%s is not what its generator writes; write it again with -write-fixture\ngot\n%s\nwant\n%s",
				committed, got, want)
		}
	}
}

// Every fixture opens at the last schema with its records, has the schema of
// a file this binary creates, and an older one leaves the copy of its
// version beside it (phases.md, section State: a test per migration opens a
// fixture of each earlier version and compares it with a fresh file).
func TestFixturesReachTheLastSchema(t *testing.T) {
	found, err := filepath.Glob(filepath.Join(fixtures, "v*.sqlite"))
	if err != nil || len(found) == 0 {
		t.Fatalf("no fixture in %s: %v", fixtures, err)
	}
	created := filepath.Join(t.TempDir(), "data")
	st, err := Create(created)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	fresh := dump(t, Path(created), false)
	for _, fixture := range found {
		name := filepath.Base(fixture)
		t.Run(name, func(t *testing.T) {
			version, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "v"), ".sqlite"))
			if err != nil || fixtureRecords[version] == nil {
				t.Fatalf("%s: no version or no records for it in fixtureRecords", name)
			}
			dir := dataDirOf(t, name)
			// read from the copy: a read of the fixture itself leaves files beside it
			before := keptRows(t, Path(dir), version)
			for table, rows := range before {
				if rows == "" {
					t.Fatalf("%s holds no row of %s", name, table)
				}
			}
			st, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			got, err := st.Load()
			if cerr := st.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				t.Fatal(err)
			}
			sameRecords(t, got, fixtureRecords[version]())
			if after := keptRows(t, Path(dir), version); !reflect.DeepEqual(after, before) {
				t.Fatalf("rows after the open\n%v\nrows of the fixture\n%v", after, before)
			}
			if schema := dump(t, Path(dir), false); schema != fresh {
				t.Fatalf("schema after the open\n%s\nschema of a fresh file\n%s", schema, fresh)
			}
			_, err = os.Stat(Path(dir) + ".v" + strconv.Itoa(version))
			if older := version < SchemaVersion; older != (err == nil) {
				t.Fatalf("fixture of schema %d, binary of schema %d: copy %v", version, SchemaVersion, err)
			}
		})
	}
}
