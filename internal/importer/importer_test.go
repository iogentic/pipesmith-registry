package importer_test

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/importer"
	"github.com/mirosso25/pipesmith-registry/internal/model"
	"github.com/mirosso25/pipesmith-registry/internal/repos"
	"github.com/mirosso25/pipesmith-registry/internal/state"
)

const (
	fixture        = "../../testdata/export/fixture"
	fixtureContent = "../../testdata/export/fixture-content"
)

// Invented passwords of the fixture users (tests/import/FixtureDb.java).
var passwords = map[string]string{
	"admin": "example-admin-1", "viewer": "example-viewer-1", "maven-lead": "example-maven-1",
	"locked.user": "example-locked-1",
}

// shiro1 is the hash the fixture stores: SHA-512, 1024 rounds, a salt
// derived from the user id.
func shiro1(user, password string) string {
	sum := sha256.Sum256([]byte("fixture salt " + user))
	salt := sum[:16]
	h := sha512.Sum512(append(append([]byte{}, salt...), password...))
	digest := h[:]
	for i := 1; i < 1024; i++ {
		next := sha512.Sum512(digest)
		digest = next[:]
	}
	return "$shiro1$SHA-512$1024$" + base64.StdEncoding.EncodeToString(salt) + "$" + base64.StdEncoding.EncodeToString(digest)
}

func attrs(t *testing.T, text string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func wantIdentity() *model.Snapshot {
	user := func(id, first, last, status, role string) model.User {
		return model.User{ID: id, FirstName: first, LastName: last, Email: id + "@example.com", Status: status,
			Source: "default", Roles: []string{role}, PasswordHash: shiro1(id, passwords[id])}
	}
	return &model.Snapshot{
		Users: []model.User{
			user("admin", "Ada", "Admin", "active", "nx-admin"),
			user("locked.user", "Lou", "Locked", "locked", "demo-viewer"),
			user("maven-lead", "Max", "Lead", "active", "demo-maven-lead"),
			user("viewer", "Vera", "Viewer", "active", "demo-viewer"),
		},
		RoleMappings: []model.RoleMapping{{UserID: "ldap.person", Source: "LDAP", Roles: []string{"demo-ops", "developers"}}},
		Roles: []model.Role{
			{ID: "demo-maven-lead", Source: "default", Name: "Demo maven lead", Description: "Writes to the Maven repositories",
				Privileges: []string{"demo-maven-write", "demo-csel-maven"}, Roles: []string{"demo-viewer"}},
			{ID: "demo-ops", Source: "default", Name: "Demo operations", Description: "",
				Privileges: []string{"demo-npm-admin", "demo-script-run"}, Roles: []string{"missing-role"}},
			{ID: "demo-viewer", Source: "default", Name: "Demo viewer", Description: "Reads and browses every repository",
				Privileges: []string{"nx-repository-view-*-*-read", "nx-repository-view-*-*-browse", "nx-search-read"},
				Roles:      []string{}},
			{ID: "developers", Source: "default", Name: "developers", Description: "Named like a directory group",
				Privileges: []string{"demo-wildcard-raw", "demo-settings-read"}, Roles: []string{"demo-viewer"}},
			{ID: "nx-admin", Source: "default", Name: "nx-admin", Description: "Administrator Role", ReadOnly: true,
				Privileges: []string{"nx-all"}, Roles: []string{}},
		},
		Privileges: []model.Privilege{
			{Name: "demo-csel-maven", Type: "repository-content-selector", Description: "Selected Maven content",
				Properties: map[string]string{"contentSelector": "demo-selector", "repository": "*-maven2", "actions": "read,browse"}},
			{Name: "demo-maven-write", Type: "repository-view", Description: "Add and edit in Maven repositories",
				Properties: map[string]string{"format": "maven2", "repository": "*", "actions": "add,edit"}},
			{Name: "demo-npm-admin", Type: "repository-admin", Description: "Administer demo-npm-proxy",
				Properties: map[string]string{"format": "npm", "repository": "demo-npm-proxy", "actions": "*"}},
			{Name: "demo-script-run", Type: "script", Description: "Run the demo script",
				Properties: map[string]string{"name": "demo-script", "actions": "browse,read,run"}},
			{Name: "demo-settings-read", Type: "application", Description: "Read the settings",
				Properties: map[string]string{"domain": "settings", "actions": "read"}},
			{Name: "demo-wildcard-raw", Type: "wildcard", Description: "Everything on demo-raw",
				Properties: map[string]string{"pattern": "nexus:repository-view:raw:demo-raw:*"}},
		},
		LDAP: []model.LDAPServer{{
			ID: "6f1c2a52-0000-4000-8000-000000000001", Name: "demo-directory", Order: 1, Protocol: "ldap",
			Host: "ldap.example.com", Port: 389, SearchBase: "dc=example,dc=com", AuthScheme: "SIMPLE",
			AuthUsername: "cn=reader,dc=example,dc=com", ConnectionTimeoutSeconds: 30, ConnectionRetryDelaySeconds: 300,
			MaxIncidentsCount: 3, UserBaseDN: "ou=people", UserSubtree: true, UserObjectClass: "inetOrgPerson",
			UserIDAttribute: "uid", UserRealNameAttribute: "cn", UserEmailAddressAttribute: "mail", LDAPGroupsAsRoles: true,
			GroupType: "static", GroupBaseDN: "ou=groups", GroupObjectClass: "groupOfUniqueNames", GroupIDAttribute: "cn",
			GroupMemberAttribute: "uniqueMember", GroupMemberFormat: "uid=${username},ou=people,dc=example,dc=com",
		}},
		Realms:       []string{"NexusAuthenticatingRealm", "NexusAuthorizingRealm", "LdapRealm", "DockerToken"},
		Anonymous:    model.Anonymous{Enabled: false, UserID: "anonymous", RealmName: "NexusAuthorizingRealm"},
		BlobStores:   []model.BlobStore{},
		Repositories: []model.Repository{},
	}
}

func wantContent(t *testing.T) *model.Snapshot {
	storage := `"storage":{"blobStoreName":"demo-files","strictContentTypeValidation":true`
	return &model.Snapshot{
		Users: []model.User{}, Roles: []model.Role{}, Privileges: []model.Privilege{}, RoleMappings: []model.RoleMapping{},
		LDAP: []model.LDAPServer{}, Realms: []string{},
		BlobStores: []model.BlobStore{
			{Name: "demo-cache", Type: "File", Path: "/srv/blobs/demo-cache",
				SoftQuota: &model.SoftQuota{Type: "spaceRemainingQuota", Limit: 5368709120}},
			{Name: "demo-files", Type: "File", Path: "/srv/blobs/demo-files"},
			{Name: "demo-group", Type: "Group"},
			{Name: "demo-s3", Type: "S3"},
		},
		Repositories: []model.Repository{
			{Name: "demo-docker", Format: "docker", Type: "hosted", Online: false, Attributes: attrs(t,
				`{`+storage+`,"writePolicy":"ALLOW"},"docker":{"httpPort":14511,"forceBasicAuth":true,"v1Enabled":false}}`)},
			{Name: "demo-maven-group", Format: "maven2", Type: "group", Online: true, Attributes: attrs(t,
				`{`+storage+`},"group":{"memberNames":["demo-maven-proxy","demo-maven-hosted"]}}`)},
			{Name: "demo-maven-hosted", Format: "maven2", Type: "hosted", Online: true, Attributes: attrs(t,
				`{`+storage+`,"writePolicy":"ALLOW_ONCE"},"maven":{"versionPolicy":"RELEASE","layoutPolicy":"STRICT"}}`)},
			{Name: "demo-maven-proxy", Format: "maven2", Type: "proxy", Online: true, Attributes: attrs(t,
				`{"storage":{"blobStoreName":"demo-cache","strictContentTypeValidation":true},`+
					`"maven":{"versionPolicy":"RELEASE","layoutPolicy":"PERMISSIVE"},`+
					`"proxy":{"remoteUrl":"https://repo.example.com/maven2/","contentMaxAge":-1,"metadataMaxAge":1440},`+
					`"httpclient":{"blocked":false,"autoBlock":true,"connection":{"retries":2,"timeout":60},`+
					`"authentication":{"type":"username","preemptive":false,"username":"demo-proxy-user"}},`+
					`"negativeCache":{"enabled":true,"timeToLive":1440}}`)},
			{Name: "demo-npm-proxy", Format: "npm", Type: "proxy", Online: true, Attributes: attrs(t,
				`{"storage":{"blobStoreName":"demo-cache","strictContentTypeValidation":true},`+
					`"proxy":{"remoteUrl":"https://registry.example.com/","contentMaxAge":1440,"metadataMaxAge":1440},`+
					`"httpclient":{"blocked":false,"autoBlock":true,"authentication":{"type":"bearerToken","preemptive":false}}}`)},
			{Name: "demo-raw", Format: "raw", Type: "hosted", Online: true, Attributes: attrs(t,
				`{"storage":{"blobStoreName":"demo-s3","strictContentTypeValidation":false,"writePolicy":"ALLOW"},`+
					`"raw":{"contentDisposition":"ATTACHMENT"}}`)},
		},
	}
}

func same(t *testing.T, got, want *model.Snapshot) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("snapshot differs\ngot  %s\nwant %s", g, w)
	}
	for i := range want.Users {
		if got.Users[i].PasswordHash != want.Users[i].PasswordHash {
			t.Fatalf("the hash of %s differs", want.Users[i].ID)
		}
	}
}

func dataDir(t *testing.T) *config.Config {
	t.Helper()
	return config.New(filepath.Join(t.TempDir(), "data"))
}

func loadState(t *testing.T, cfg *config.Config) *model.Snapshot {
	t.Helper()
	st, err := state.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestReadIdentityExport(t *testing.T) {
	export, err := importer.ReadExport(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if export.Set != "identity" || !slices.Equal(export.Kinds, []state.Kind{state.KindUsers, state.KindRoles,
		state.KindPrivileges, state.KindLDAP, state.KindRealms, state.KindAnonymous}) {
		t.Fatalf("set %q kinds %v", export.Set, export.Kinds)
	}
	same(t, export.Snapshot, wantIdentity())
	if export.PasswordHashes != 4 {
		t.Fatalf("%d users with a hash, want 4", export.PasswordHashes)
	}
	if !slices.Equal(export.UnmappedLDAPKeys, []string{}) {
		t.Fatalf("unmapped LDAP keys %v", export.UnmappedLDAPKeys)
	}
}

func TestReadContentExport(t *testing.T) {
	export, err := importer.ReadExport(fixtureContent)
	if err != nil {
		t.Fatal(err)
	}
	if export.Set != "content" || !slices.Equal(export.Kinds, []state.Kind{state.KindBlobStores, state.KindRepositories}) {
		t.Fatalf("set %q kinds %v", export.Set, export.Kinds)
	}
	same(t, export.Snapshot, wantContent(t))
	if !slices.Equal(export.NeedsCredentials, []string{"demo-maven-proxy", "demo-npm-proxy"}) {
		t.Fatalf("needs credentials %v", export.NeedsCredentials)
	}
}

func TestImportWritesStateAndRepositoriesJSON(t *testing.T) {
	cfg := dataDir(t)
	result, err := importer.Import(fixtureContent, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.RepositoriesPath != cfg.RepositoriesPath() {
		t.Fatalf("repositories.json at %q", result.RepositoriesPath)
	}
	same(t, loadState(t, cfg), wantContent(t))
	defs, err := repos.LoadDefinitions(cfg.RepositoriesPath())
	if err != nil {
		t.Fatal(err)
	}
	text := func(s string) *string { return &s }
	port := 14511
	want := []repos.Repo{
		{Name: "demo-docker", Format: "docker", Type: "hosted", Defined: true, WritePolicy: text("ALLOW"), HTTPPort: &port},
		{Name: "demo-maven-group", Format: "maven2", Type: "group", Defined: true,
			Members: []string{"demo-maven-proxy", "demo-maven-hosted"}},
		{Name: "demo-maven-hosted", Format: "maven2", Type: "hosted", Defined: true, VersionPolicy: text("RELEASE"),
			LayoutPolicy: text("STRICT"), WritePolicy: text("ALLOW_ONCE")},
		{Name: "demo-maven-proxy", Format: "maven2", Type: "proxy", Defined: true, VersionPolicy: text("RELEASE"),
			LayoutPolicy: text("PERMISSIVE"), RemoteURL: text("https://repo.example.com/maven2/")},
		{Name: "demo-npm-proxy", Format: "npm", Type: "proxy", Defined: true, RemoteURL: text("https://registry.example.com/")},
		{Name: "demo-raw", Format: "raw", Type: "hosted", Defined: true, WritePolicy: text("ALLOW")},
	}
	if !reflect.DeepEqual(defs, want) {
		t.Fatalf("repositories.json\n%+v\nwant\n%+v", defs, want)
	}
	info, err := os.Stat(state.Path(cfg.DataDir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v", info, err)
	}
}

func TestIdentityImportLeavesRepositories(t *testing.T) {
	cfg := dataDir(t)
	if _, err := importer.Import(fixtureContent, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.RepositoriesPath())
	if err != nil {
		t.Fatal(err)
	}
	result, err := importer.Import(fixture, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.RepositoriesPath != "" {
		t.Fatalf("an identity import wrote %s", result.RepositoriesPath)
	}
	after, err := os.ReadFile(cfg.RepositoriesPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("repositories.json changed: %v", err)
	}
	want := wantIdentity()
	content := wantContent(t)
	want.BlobStores, want.Repositories = content.BlobStores, content.Repositories
	same(t, loadState(t, cfg), want)
}

// copyExport copies an export into a fresh directory that a test may break.
func copyExport(t *testing.T, from string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "export")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func editManifest(t *testing.T, dir string, edit func(m map[string]any)) {
	t.Helper()
	path := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileEntry(m map[string]any, name string) map[string]any {
	for _, f := range m["files"].([]any) {
		if entry := f.(map[string]any); entry["file"] == name {
			return entry
		}
	}
	panic("no entry " + name)
}

func rewrite(t *testing.T, dir, name string, edit func([]byte) []byte) {
	t.Helper()
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edit(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fixDigest makes the manifest match an edited file, so that the check
// behind the digest check is reached.
func fixDigest(t *testing.T, dir, name string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	editManifest(t, dir, func(m map[string]any) { fileEntry(m, name)["sha256"] = fmt.Sprintf("%x", sum) })
}

func TestManifestMismatchesAreRefusedBeforeAnyWrite(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"changed byte": func(t *testing.T, dir string) {
			rewrite(t, dir, "role.jsonl", func(b []byte) []byte { return bytes.Replace(b, []byte("Demo"), []byte("Dema"), 1) })
		},
		"wrong count": func(t *testing.T, dir string) {
			editManifest(t, dir, func(m map[string]any) { fileEntry(m, "role.jsonl")["rows"] = 4 })
		},
		"missing file": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "privilege.jsonl")); err != nil {
				t.Fatal(err)
			}
		},
		"file left out of the manifest": func(t *testing.T, dir string) {
			editManifest(t, dir, func(m map[string]any) { m["files"] = m["files"].([]any)[1:] })
		},
		"unknown set": func(t *testing.T, dir string) {
			editManifest(t, dir, func(m map[string]any) { m["set"] = "everything" })
		},
		"unknown format": func(t *testing.T, dir string) {
			editManifest(t, dir, func(m map[string]any) { m["format"] = "other" })
		},
		"newer version": func(t *testing.T, dir string) {
			editManifest(t, dir, func(m map[string]any) { m["version"] = 2 })
		},
		"other columns": func(t *testing.T, dir string) {
			editManifest(t, dir, func(m map[string]any) { fileEntry(m, "role.jsonl")["columns"] = []any{"id"} })
		},
		"a hash in an export without hashes": func(t *testing.T, dir string) {
			editManifest(t, dir, func(m map[string]any) { m["passwordHashes"] = false })
		},
		"wrong removed count": func(t *testing.T, dir string) {
			editManifest(t, dir, func(m map[string]any) { fileEntry(m, "ldap_configuration.jsonl")["removed"] = 0 })
		},
		"row with an extra key": func(t *testing.T, dir string) {
			rewrite(t, dir, "role.jsonl", func(b []byte) []byte {
				return bytes.Replace(b, []byte(`{"id":"demo-ops",`), []byte(`{"extra":1,"id":"demo-ops",`), 1)
			})
			fixDigest(t, dir, "role.jsonl")
		},
		"symbolic link": func(t *testing.T, dir string) {
			path := filepath.Join(dir, "role.jsonl")
			if err := os.Rename(path, path+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+".real", path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			dir := copyExport(t, fixture)
			breakIt(t, dir)
			cfg := dataDir(t)
			_, err := importer.Import(dir, cfg)
			if err == nil {
				t.Fatal("a broken export was imported")
			}
			t.Logf("refused: %v", err)
			for _, user := range []string{"admin", "viewer"} {
				if strings.Contains(err.Error(), shiro1(user, passwords[user])) || strings.Contains(err.Error(), "$shiro1$") {
					t.Fatal("the refusal carries a hash")
				}
			}
			if _, err := os.Stat(cfg.DataDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refused import touched the data directory: %v", err)
			}
		})
	}
	// the unbroken copy imports, so each case above failed for its own reason
	cfg := dataDir(t)
	if _, err := importer.Import(copyExport(t, fixture), cfg); err != nil {
		t.Fatal(err)
	}
}

const settingsSnapshot = `{"items": [
 {"name": "real-hosted", "format": "maven2", "type": "hosted", "url": "https://old.example.com/repository/real-hosted",
  "online": true, "storage": {"blobStoreName": "default", "strictContentTypeValidation": true, "writePolicy": "ALLOW_ONCE"},
  "maven": {"versionPolicy": "RELEASE", "layoutPolicy": "STRICT"}},
 {"name": "real-proxy", "format": "npm", "type": "proxy", "online": false,
  "proxy": {"remoteUrl": "https://registry.example.com/"},
  "httpClient": {"authentication": {"type": "username", "username": "demo-user", "password": "settings-secret-1"},
                 "connection": {"retries": 1}}},
 {"name": "real-group", "format": "maven2", "type": "group", "group": {"memberNames": ["real-proxy", "real-hosted"]}},
 {"name": "real-hosted", "format": "raw", "type": "hosted"},
 {"format": "raw"},
 "not an object"
]}`

func TestImportSettingsSnapshot(t *testing.T) {
	cfg := dataDir(t)
	source := filepath.Join(t.TempDir(), "repositorySettings.json")
	if err := os.WriteFile(source, []byte(settingsSnapshot), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := importer.ImportSettings(source, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Kinds, []state.Kind{state.KindRepositories}) || result.RepositoriesPath != cfg.RepositoriesPath() {
		t.Fatalf("kinds %v, repositories.json %q", result.Kinds, result.RepositoriesPath)
	}
	if !slices.Equal(result.NeedsCredentials, []string{"real-proxy"}) {
		t.Fatalf("needs credentials %v", result.NeedsCredentials)
	}
	got := loadState(t, cfg)
	want := []model.Repository{
		{Name: "real-group", Format: "maven2", Type: "group", Online: true,
			Attributes: attrs(t, `{"group": {"memberNames": ["real-proxy", "real-hosted"]}}`)},
		{Name: "real-hosted", Format: "maven2", Type: "hosted", Online: true, Attributes: attrs(t,
			`{"storage": {"blobStoreName": "default", "strictContentTypeValidation": true, "writePolicy": "ALLOW_ONCE"},
			  "maven": {"versionPolicy": "RELEASE", "layoutPolicy": "STRICT"}}`)},
		{Name: "real-proxy", Format: "npm", Type: "proxy", Online: false, Attributes: attrs(t,
			`{"proxy": {"remoteUrl": "https://registry.example.com/"},
			  "httpClient": {"authentication": {"type": "username", "username": "demo-user"}, "connection": {"retries": 1}}}`)},
	}
	if !reflect.DeepEqual(got.Repositories, want) {
		g, _ := json.Marshal(got.Repositories)
		t.Fatalf("repositories %s", g)
	}
	if len(got.Users) != 0 {
		t.Fatalf("a settings import wrote users: %v", got.Users)
	}
	// repositories.json is what import-settings writes from the same file
	reference := filepath.Join(t.TempDir(), "repositories.json")
	if _, err := repos.ImportRepositorySettings(source, reference); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(reference)
	b, _ := os.ReadFile(cfg.RepositoriesPath())
	if len(a) == 0 || !bytes.Equal(a, b) {
		t.Fatalf("repositories.json differs from import-settings\n%s\n%s", b, a)
	}
	data, err := os.ReadFile(state.Path(cfg.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("settings-secret-1")) || bytes.Contains(data, []byte("old.example.com")) {
		t.Fatal("state.sqlite holds the proxy password or the url of the old server")
	}
}

// An identity export laid over a settings import keeps the settings
// repositories, and a settings import over an identity export keeps the users.
func TestImportsReplaceOnlyTheirKinds(t *testing.T) {
	source := filepath.Join(t.TempDir(), "repositorySettings.json")
	if err := os.WriteFile(source, []byte(settingsSnapshot), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, order := range [][2]string{{"settings", "identity"}, {"identity", "settings"}} {
		cfg := dataDir(t)
		for _, step := range order {
			var err error
			if step == "settings" {
				_, err = importer.ImportSettings(source, cfg)
			} else {
				_, err = importer.Import(fixture, cfg)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		got := loadState(t, cfg)
		if len(got.Users) != 4 || len(got.Repositories) != 3 || len(got.Roles) != 5 {
			t.Fatalf("%v: %d users, %d repositories, %d roles", order, len(got.Users), len(got.Repositories), len(got.Roles))
		}
	}
}

func TestImportSettingsRefusesAFileWithoutRepositories(t *testing.T) {
	cfg := dataDir(t)
	source := filepath.Join(t.TempDir(), "settings.json")
	for _, text := range []string{`{"items": 3}`, `not json`, `"text"`} {
		if err := os.WriteFile(source, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := importer.ImportSettings(source, cfg); err == nil {
			t.Fatalf("%q was imported", text)
		}
	}
	if _, err := os.Stat(cfg.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused settings import touched the data directory: %v", err)
	}
}
