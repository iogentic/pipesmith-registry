package rest

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/scan"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Invented test data: two stores, repositories of the four formats, a
// group and a proxy, and the identity records of an invented install.

const (
	older = int64(1_700_000_000_000)
	newer = int64(1_700_000_500_000)
)

func blob(repo, path, content string, created int64) testkit.BlobSpec {
	s := testkit.NewBlobSpec(repo, path)
	s.Content = []byte(content)
	s.CreatedMS = created
	return s
}

var storeA = []testkit.BlobSpec{
	blob("demo-maven", "com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar", "jar 1.0.0", older),
	blob("demo-maven", "com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar.sha1", "sha of jar", older),
	blob("demo-maven", "com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom", "<project/>", older),
	blob("demo-maven", "com/example/demo-lib/1.0.0/demo-lib-1.0.0-sources.jar", "sources 1.0.0", older),
	blob("demo-maven", "com/example/demo-lib/1.1.0/demo-lib-1.1.0.jar", "jar 1.1.0", older),
	blob("demo-maven", "com/example/demo-lib/maven-metadata.xml", "<metadata/>", older),
	blob("demo-maven", "org/sample/tool/2.0/tool-2.0.jar", "tool 2.0", older),
	blob("demo-npm", "widget", `{"name": "widget"}`, older),
	blob("demo-npm", "widget/-/widget-1.2.3.tgz", "widget tarball", older),
	blob("demo-npm", "@demo/gadget/-/gadget-0.1.0.tgz", "gadget tarball", older),
	blob("demo-docker", "v2/app/manifests/1.0", `{"schemaVersion": 2}`, older),
	blob("demo-docker", "v2/-/blobs/sha256:"+strings.Repeat("ab", 32), "layer", older),
	blob("demo-raw", "docs/readme.txt", "old readme", older),
	blob("demo-raw", "top.txt", "top", older),
}

var storeB = []testkit.BlobSpec{
	blob("demo-raw", "docs/readme.txt", "new readme", newer),
	blob("demo-proxy", "com/example/demo-lib/1.0.0/demo-lib-1.0.0.jar", "proxied jar", newer),
	blob("demo-proxy", "com/example/remote/3.0/remote-3.0.jar", "remote 3.0", newer),
}

// demoAttributes are stored attributes in the shapes the import writes:
// the database form (httpclient, cleanup.policyName) for demo-proxy and the
// REST form for the others.
func demoRepositories() []model.Repository {
	return []model.Repository{
		{Name: "demo-docker", Format: "docker", Type: "hosted", Online: true, Attributes: map[string]any{
			"storage": map[string]any{"blobStoreName": "store-a", "strictContentTypeValidation": true, "writePolicy": "ALLOW_ONCE"},
			"docker":  map[string]any{"v1Enabled": false, "forceBasicAuth": true, "httpPort": json.Number("14690")},
		}},
		{Name: "demo-group", Format: "maven2", Type: "group", Online: true, Attributes: map[string]any{
			"storage": map[string]any{"blobStoreName": "store-a", "strictContentTypeValidation": true},
			"group":   map[string]any{"memberNames": []any{"demo-maven", "demo-proxy"}},
		}},
		{Name: "demo-maven", Format: "maven2", Type: "hosted", Online: true, Attributes: map[string]any{
			"storage": map[string]any{"blobStoreName": "store-a", "strictContentTypeValidation": true, "writePolicy": "ALLOW_ONCE"},
			"maven":   map[string]any{"versionPolicy": "RELEASE", "layoutPolicy": "STRICT"},
			"cleanup": map[string]any{"policyNames": []any{"weekly"}},
		}},
		{Name: "demo-npm", Format: "npm", Type: "hosted", Online: false, Attributes: map[string]any{
			"storage": map[string]any{"blobStoreName": "store-a", "strictContentTypeValidation": true, "writePolicy": "ALLOW"},
		}},
		{Name: "demo-proxy", Format: "maven2", Type: "proxy", Online: true, Attributes: map[string]any{
			"storage": map[string]any{"blobStoreName": "store-b", "strictContentTypeValidation": true},
			"proxy":   map[string]any{"remoteUrl": "https://repo.example.com/maven2", "contentMaxAge": json.Number("-1")},
			"httpclient": map[string]any{"blocked": false, "autoBlock": true,
				"authentication": map[string]any{"type": "username", "username": "demo-pull", "password": "example-secret-1"}},
			"cleanup":       map[string]any{"policyName": []any{"monthly"}},
			"negativeCache": map[string]any{"enabled": true, "timeToLive": 1440.0},
		}},
		{Name: "demo-raw", Format: "raw", Type: "hosted", Online: true, Attributes: map[string]any{
			"storage": map[string]any{"blobStoreName": "store-a", "strictContentTypeValidation": false, "writePolicy": "ALLOW"},
			"raw":     map[string]any{"contentDisposition": "ATTACHMENT"},
		}},
	}
}

func demoSnapshot() *model.Snapshot {
	return &model.Snapshot{
		Users: []model.User{
			{ID: "demo-admin", FirstName: "Demo", LastName: "Admin", Email: "demo-admin@example.com", Status: "active", Source: "default",
				Roles: []string{"demo-administrators"}, PasswordHash: "$shiro1$SHA-512$1024$c2FsdA==$aGFzaA=="},
			{ID: "demo-reader", FirstName: "Demo", LastName: "Reader", Email: "demo-reader@example.com", Status: "active", Source: "default",
				Roles: []string{"demo-readers"}, PasswordHash: "0123456789abcdef0123456789abcdef01234567"},
			{ID: "demo-locked", FirstName: "Demo", LastName: "Locked", Email: "demo-locked@example.com", Status: "locked", Source: "default",
				Roles: nil},
		},
		Roles: []model.Role{
			{ID: "demo-administrators", Source: "default", Name: "Administrators", Description: "Every right", ReadOnly: false,
				Privileges: []string{"demo-all"}, Roles: []string{}},
			{ID: "demo-readers", Source: "default", Name: "Readers", Description: "Read the Maven repositories",
				Privileges: []string{"demo-maven-read", "demo-search"}, Roles: nil},
			{ID: "platform-team", Source: "default", Name: "platform-team", Description: "Mapped directory group",
				Privileges: nil, Roles: []string{"demo-readers"}},
		},
		Privileges: []model.Privilege{
			{Name: "demo-all", Type: "wildcard", Description: "Everything", ReadOnly: true, Properties: map[string]string{"pattern": "nexus:*"}},
			{Name: "demo-maven-read", Type: "repository-view", Description: "Browse and read Maven", Properties: map[string]string{
				"format": "maven2", "repository": "*", "actions": "browse,read"}},
			{Name: "demo-search", Type: "application", Description: "Search", Properties: map[string]string{"domain": "search", "actions": "read"}},
			{Name: "demo-users-all", Type: "application", Properties: map[string]string{"domain": "users", "actions": "*"}},
			{Name: "demo-script", Type: "script", Properties: map[string]string{"name": "cleanup", "actions": `["run","read"]`}},
		},
		RoleMappings: []model.RoleMapping{{UserID: "dir.person", Source: "LDAP", Roles: []string{"platform-team"}}},
		LDAP: []model.LDAPServer{
			{ID: "conn-2", Name: "lab-directory", Order: 2, Protocol: "ldap", Host: "ldap-lab.example.org", Port: 389,
				SearchBase: "dc=lab,dc=example,dc=org", AuthScheme: "NONE", GroupType: "static"},
			{ID: "conn-1", Name: "corp-directory", Order: 1, Protocol: "ldaps", Host: "ldap.example.com", Port: 636,
				SearchBase: "dc=example,dc=com", AuthScheme: "SIMPLE", AuthUsername: "uid=bind,dc=example,dc=com", GroupType: "static",
				LDAPGroupsAsRoles: true},
		},
		Realms:    []string{"NexusAuthenticatingRealm", "LdapRealm", "DockerToken"},
		Anonymous: model.Anonymous{Enabled: false, UserID: "anonymous", RealmName: "NexusAuthenticatingRealm"},
		BlobStores: []model.BlobStore{
			{Name: "old-cloud", Type: "S3", Path: ""},
			{Name: "store-a", Type: "File", Path: "/srv/old-host/store-a", SoftQuota: &model.SoftQuota{Type: "spaceUsedQuota", Limit: 10}},
			{Name: "store-b", Type: "File", Path: "/srv/old-host/store-b"},
		},
		Repositories: demoRepositories(),
	}
}

// fixture is a data directory with the two stores scanned, the read path
// repositories and an Env for the administrator.
type fixture struct {
	t     *testing.T
	cfg   *config.Config
	ix    *index.Index
	cat   *Catalog
	repos *repos.Repositories
	snap  *model.Snapshot
	envs  []Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	cfg := config.New(filepath.Join(base, "data"))
	for name, blobs := range map[string][]testkit.BlobSpec{"store-a": storeA, "store-b": storeB} {
		root, err := testkit.MakeBlobStore(filepath.Join(base, name), blobs)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Stores = append(cfg.Stores, config.StoreConfig{Name: name, Kind: stores.KindBlobStore, Root: root})
	}
	cfg.Stores = append(cfg.Stores, config.StoreConfig{Name: "gone", Kind: stores.KindDirectory, Root: filepath.Join(base, "missing")})
	ix, err := index.Open(cfg.IndexPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	f := &fixture{t: t, cfg: cfg, ix: ix, snap: demoSnapshot()}
	for _, sc := range cfg.Stores[:2] {
		s, err := stores.Make(sc, cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := scan.Run(s, ix, nil); err != nil {
			t.Fatal(err)
		}
		f.envs = append(f.envs, Store{Name: sc.Name, Kind: sc.Kind, Root: sc.Root, Available: !stores.LooksUnmounted(s)})
	}
	f.envs = append(f.envs, Store{Name: "gone", Kind: stores.KindDirectory, Root: cfg.Stores[2].Root, Available: false})
	var defined []repos.Repo
	for _, r := range f.snap.Repositories {
		repo := repos.Repo{Name: r.Name, Format: r.Format, Type: r.Type, Defined: true}
		if r.Name == "demo-group" {
			repo.Members = []string{"demo-maven", "demo-proxy"}
		}
		defined = append(defined, repo)
	}
	names, err := ix.RepoNames()
	if err != nil {
		t.Fatal(err)
	}
	// a repository found in a store without a definition
	f.repos = repos.New(append(defined, repos.Repo{Name: "found-only", Format: "unknown", Type: "hosted"}), names, nil)
	if f.cat, err = OpenCatalog(cfg.IndexPath()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.cat.Close() })
	return f
}

// env is the Env of a subject holding the granted permissions.
func (f *fixture) env(granted ...string) *Env {
	return &Env{
		Context:      context.Background(),
		Snapshot:     f.snap,
		Repositories: f.repos,
		Index:        f.ix,
		Catalog:      f.cat,
		Stores:       f.envs,
		Permits:      func(p string) bool { return implies(granted, p) },
		Base:         "http://localhost:14650",
	}
}

func (f *fixture) admin() *Env { return f.env("nexus:*") }

// implies is the wildcard rule of the old product as the contract states
// it, enough for the tests: parts split by ":", alternatives by ",", "*"
// matches any value, missing trailing parts count as "*".
func implies(granted []string, permission string) bool {
	want := strings.Split(permission, ":")
	for _, g := range granted {
		parts := strings.Split(g, ":")
		ok := true
		for i, part := range parts {
			alternatives := strings.Split(part, ",")
			if contains(alternatives, "*") {
				continue
			}
			if i >= len(want) || !contains(alternatives, want[i]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// body is the answer as JSON values, as a client reads it.
func body(t *testing.T, a Answer, err error, status int) any {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	data, jerr := a.JSON()
	if jerr != nil {
		t.Fatal(jerr)
	}
	if a.Status != status {
		t.Fatalf("status %d, want %d: %s", a.Status, status, data)
	}
	var v any
	if len(data) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return v
}

func list(t *testing.T, v any) []map[string]any {
	t.Helper()
	items, ok := v.([]any)
	if !ok {
		t.Fatalf("not a list: %#v", v)
	}
	out := make([]map[string]any, len(items))
	for i, item := range items {
		if out[i], ok = item.(map[string]any); !ok {
			t.Fatalf("item %d is not an object: %#v", i, item)
		}
	}
	return out
}

func names(items []map[string]any, key string) []string {
	out := []string{}
	for _, item := range items {
		s, _ := item[key].(string)
		out = append(out, s)
	}
	return out
}

func keys(m map[string]any) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func query(pairs ...string) url.Values {
	q := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		q.Add(pairs[i], pairs[i+1])
	}
	return q
}

func message(t *testing.T, v any) string {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %#v", v)
	}
	s, _ := m["message"].(string)
	if s == "" {
		t.Fatalf("no message: %#v", v)
	}
	return s
}
