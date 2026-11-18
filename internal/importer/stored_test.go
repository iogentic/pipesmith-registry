package importer_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/importer"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// Rows in the shape the old product stores them, as the export program
// writes them: it keeps the JSON of a column as stored and takes out the
// secret keys only. Every name, address and value here is invented.

// writeRows replaces the rows of one file of an export and makes the
// manifest match them: row count, count of removed fields and digest.
func writeRows(t *testing.T, dir, name string, rows ...string) {
	t.Helper()
	var data []byte
	removed := 0
	for _, r := range rows {
		var fields struct {
			Removed []string `json:"_removed"`
		}
		if err := json.Unmarshal([]byte(r), &fields); err != nil {
			t.Fatalf("%s: %v", r, err)
		}
		removed += len(fields.Removed)
		data = append(append(data, r...), '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	editManifest(t, dir, func(m map[string]any) {
		entry := fileEntry(m, name)
		entry["rows"], entry["removed"], entry["sha256"] = len(rows), removed, fmt.Sprintf("%x", sum)
	})
}

// drop as a value of edit removes the key.
var drop = &struct{}{}

// edit returns the JSON object text with the keys of changes set; a key
// whose value is drop is removed, a nil value is JSON null.
func edit(t *testing.T, text string, changes map[string]any) string {
	t.Helper()
	m := attrs(t, text)
	for key, value := range changes {
		if value == drop {
			delete(m, key)
			continue
		}
		m[key] = value
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The two JSON columns of an LDAP row as the old product stores them: the
// host as an object, the names of its stored entity, no group type. The
// export removed systemPassword and userPasswordAttribute.
const (
	staticConnection = `{"host":{"protocol":"ldap","hostName":"ldap.example.com","port":389},` +
		`"searchBase":"dc=example,dc=com","systemUsername":"cn=reader,dc=example,dc=com","authScheme":"SIMPLE",` +
		`"saslRealm":"","connectionTimeout":30,"connectionRetryDelay":300,"maxIncidentsCount":3,"useTrustStore":false}`
	staticMapping = `{"emailAddressAttribute":"mail","ldapFilter":"","userBaseDn":"ou=people","userSubtree":true,` +
		`"userObjectClass":"inetOrgPerson","userIdAttribute":"uid","userRealNameAttribute":"cn",` +
		`"userMemberOfAttribute":null,"ldapGroupsAsRoles":true,"groupBaseDn":"ou=groups","groupSubtree":false,` +
		`"groupObjectClass":"groupOfUniqueNames","groupIdAttribute":"cn","groupMemberAttribute":"uniqueMember",` +
		`"groupMemberFormat":"uid=${username},ou=people,dc=example,dc=com"}`
	dynamicConnection = `{"host":{"protocol":"ldaps","hostName":"directory.example.com","port":636},` +
		`"searchBase":"dc=example,dc=com","systemUsername":"cn=binder,ou=services,dc=example,dc=com",` +
		`"authScheme":"DIGEST-MD5","saslRealm":"example.com","connectionTimeout":15,"connectionRetryDelay":60,` +
		`"maxIncidentsCount":5,"useTrustStore":true}`
	dynamicMapping = `{"emailAddressAttribute":"mail","ldapFilter":"(employeeType=staff)","userBaseDn":"ou=staff",` +
		`"userSubtree":true,"userObjectClass":"person","userIdAttribute":"uid","userRealNameAttribute":"displayName",` +
		`"userMemberOfAttribute":"memberOf","ldapGroupsAsRoles":true,"groupBaseDn":null,"groupSubtree":false,` +
		`"groupObjectClass":null,"groupIdAttribute":null,"groupMemberAttribute":null,"groupMemberFormat":null}`
)

// restRow is the LDAP row of the 0.1.1 fixture, written with the names of
// the REST surface of the old product.
const restRow = `{"id":"6f1c2a52-0000-4000-8000-000000000001","name":"demo-directory","order":1,` +
	`"connection":{"protocol":"ldap","useTrustStore":false,"host":"ldap.example.com","port":389,` +
	`"searchBase":"dc=example,dc=com","authScheme":"SIMPLE","authRealm":"","authUsername":"cn=reader,dc=example,dc=com",` +
	`"connectionTimeoutSeconds":30,"connectionRetryDelaySeconds":300,"maxIncidentsCount":3},` +
	`"mapping":{"userBaseDn":"ou=people","userSubtree":true,"userObjectClass":"inetOrgPerson","userLdapFilter":"",` +
	`"userIdAttribute":"uid","userRealNameAttribute":"cn","userEmailAddressAttribute":"mail","ldapGroupsAsRoles":true,` +
	`"groupType":"static","groupBaseDn":"ou=groups","groupSubtree":false,"groupObjectClass":"groupOfUniqueNames",` +
	`"groupIdAttribute":"cn","groupMemberAttribute":"uniqueMember",` +
	`"groupMemberFormat":"uid=${username},ou=people,dc=example,dc=com","userMemberOfAttribute":""},` +
	`"_removed":["connection.authPassword","mapping.userPasswordAttribute"]}`

func ldapRow(id, name string, order int, connection, mapping string) string {
	return fmt.Sprintf(`{"id":%q,"name":%q,"order":%d,"connection":%s,"mapping":%s,`+
		`"_removed":["connection.systemPassword","mapping.userPasswordAttribute"]}`, id, name, order, connection, mapping)
}

// ldapExport is a copy of the identity fixture with rows as its LDAP rows.
func ldapExport(t *testing.T, rows ...string) string {
	t.Helper()
	dir := copyExport(t, fixture)
	writeRows(t, dir, "ldap_configuration.jsonl", rows...)
	return dir
}

func readLDAP(t *testing.T, dir string) *importer.Export {
	t.Helper()
	export, err := importer.ReadExport(dir)
	if err != nil {
		t.Fatal(err)
	}
	return export
}

func sameLDAP(t *testing.T, got, want []model.LDAPServer) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("LDAP servers differ\ngot  %s\nwant %s", g, w)
	}
}

// staticServer is the server of the static rows, the LDAP server of the
// identity fixture under another id, name and order.
func staticServer(id, name string, order int) model.LDAPServer {
	s := wantIdentity().LDAP[0]
	s.ID, s.Name, s.Order = id, name, order
	return s
}

func TestReadLDAPStoredShape(t *testing.T) {
	export := readLDAP(t, ldapExport(t,
		ldapRow("6f1c2a52-0000-4000-8000-0000000000a1", "staff-directory", 1, dynamicConnection, dynamicMapping),
		ldapRow("6f1c2a52-0000-4000-8000-0000000000a2", "people-directory", 2, staticConnection, staticMapping)))
	sameLDAP(t, export.Snapshot.LDAP, []model.LDAPServer{{
		ID: "6f1c2a52-0000-4000-8000-0000000000a1", Name: "staff-directory", Order: 1, Protocol: "ldaps",
		UseTrustStore: true, Host: "directory.example.com", Port: 636, SearchBase: "dc=example,dc=com",
		AuthScheme: "DIGEST-MD5", AuthRealm: "example.com", AuthUsername: "cn=binder,ou=services,dc=example,dc=com",
		ConnectionTimeoutSeconds: 15, ConnectionRetryDelaySeconds: 60, MaxIncidentsCount: 5, UserBaseDN: "ou=staff",
		UserSubtree: true, UserObjectClass: "person", UserLDAPFilter: "(employeeType=staff)", UserIDAttribute: "uid",
		UserRealNameAttribute: "displayName", UserEmailAddressAttribute: "mail", LDAPGroupsAsRoles: true,
		GroupType: "dynamic", UserMemberOfAttribute: "memberOf",
	}, staticServer("6f1c2a52-0000-4000-8000-0000000000a2", "people-directory", 2)})
	if !slices.Equal(export.UnmappedLDAPKeys, []string{}) {
		t.Fatalf("unmapped LDAP keys %v", export.UnmappedLDAPKeys)
	}
}

// A stored row has no group type: dynamic when the member of attribute is
// set and not blank, else static, as the public source of the old product
// decides it.
func TestLDAPGroupTypeOfAStoredRow(t *testing.T) {
	for _, c := range []struct {
		memberOf any
		want     string
	}{
		{nil, "static"}, {drop, "static"}, {"", "static"}, {" ", "static"},
		{"memberOf", "dynamic"}, {"isMemberOf", "dynamic"},
	} {
		mapping := edit(t, staticMapping, map[string]any{"userMemberOfAttribute": c.memberOf})
		export := readLDAP(t, ldapExport(t, ldapRow("6f1c2a52-0000-4000-8000-0000000000a3", "groups", 1,
			staticConnection, mapping)))
		if got := export.Snapshot.LDAP[0].GroupType; got != c.want {
			t.Errorf("member of attribute %v: group type %q, want %q", c.memberOf, got, c.want)
		}
	}
}

// An export written with the names of the REST surface, as the 0.1.1
// fixture is, still imports, and gives the server its stored shape gives.
func TestReadLDAPRESTNames(t *testing.T) {
	want := []model.LDAPServer{staticServer("6f1c2a52-0000-4000-8000-000000000001", "demo-directory", 1)}
	sameLDAP(t, readLDAP(t, ldapExport(t, restRow)).Snapshot.LDAP, want)
	sameLDAP(t, readLDAP(t, ldapExport(t, ldapRow("6f1c2a52-0000-4000-8000-000000000001", "demo-directory", 1,
		staticConnection, staticMapping))).Snapshot.LDAP, want)
	// a group type the row gives is kept, in lower case
	dynamic := strings.Replace(restRow, `"groupType":"static"`, `"groupType":"DYNAMIC"`, 1)
	if got := readLDAP(t, ldapExport(t, dynamic)).Snapshot.LDAP[0].GroupType; got != "dynamic" {
		t.Fatalf("group type %q", got)
	}
}

// replaceOnce replaces old in text and fails the test unless old is there
// exactly once.
func replaceOnce(t *testing.T, text, old, new string) string {
	t.Helper()
	if n := strings.Count(text, old); n != 1 {
		t.Fatalf("%q is %d times in the row, want once", old, n)
	}
	return strings.Replace(text, old, new, 1)
}

// The four integers of an LDAP row read a number with a zero fraction as the
// integer, in the stored names and in the names of the REST surface.
func TestLDAPIntegersWithAZeroFraction(t *testing.T) {
	connection := edit(t, staticConnection, map[string]any{
		"host":                 map[string]any{"protocol": "ldap", "hostName": "ldap.example.com", "port": json.Number("389.0")},
		"connectionTimeout":    json.Number("30.0"),
		"connectionRetryDelay": json.Number("3.0E2"),
		"maxIncidentsCount":    json.Number("3.000"),
	})
	sameLDAP(t, readLDAP(t, ldapExport(t, ldapRow("6f1c2a52-0000-4000-8000-0000000000a5", "fractions", 1,
		connection, staticMapping))).Snapshot.LDAP, []model.LDAPServer{staticServer("6f1c2a52-0000-4000-8000-0000000000a5", "fractions", 1)})
	rest := replaceOnce(t, restRow, `"port":389,`, `"port":3.89e2,`)
	rest = replaceOnce(t, rest, `"connectionTimeoutSeconds":30,`, `"connectionTimeoutSeconds":30.0,`)
	rest = replaceOnce(t, rest, `"connectionRetryDelaySeconds":300,`, `"connectionRetryDelaySeconds":300.00,`)
	rest = replaceOnce(t, rest, `"maxIncidentsCount":3}`, `"maxIncidentsCount":0.3E1}`)
	sameLDAP(t, readLDAP(t, ldapExport(t, rest)).Snapshot.LDAP,
		[]model.LDAPServer{staticServer("6f1c2a52-0000-4000-8000-000000000001", "demo-directory", 1)})
}

// A key of the host object no field takes is reported by its path.
func TestLDAPUnknownHostKeyIsReported(t *testing.T) {
	connection := edit(t, staticConnection, map[string]any{
		"host": map[string]any{"protocol": "ldap", "hostName": "ldap.example.com", "port": 389, "scheme": "x"}})
	export := readLDAP(t, ldapExport(t, ldapRow("6f1c2a52-0000-4000-8000-0000000000a4", "host-extra", 1,
		connection, staticMapping)))
	if !slices.Equal(export.UnmappedLDAPKeys, []string{"connection.host.scheme"}) {
		t.Fatalf("unmapped LDAP keys %v", export.UnmappedLDAPKeys)
	}
	sameLDAP(t, export.Snapshot.LDAP, []model.LDAPServer{staticServer("6f1c2a52-0000-4000-8000-0000000000a4", "host-extra", 1)})
}

func blobRow(id, name, kind, attributes string) string {
	return fmt.Sprintf(`{"id":%q,"name":%q,"type":%q,"attributes":%s,"_removed":[]}`, id, name, kind, attributes)
}

// blobExport is a copy of the content fixture with rows as its blob stores.
func blobExport(t *testing.T, rows ...string) string {
	t.Helper()
	dir := copyExport(t, fixtureContent)
	writeRows(t, dir, "blob_store_configuration.jsonl", rows...)
	return dir
}

// The old product writes an empty blobStoreQuotaConfig for a store without
// a soft quota and reads it as none, as it reads a missing key.
func TestBlobStoreWithoutSoftQuota(t *testing.T) {
	export, err := importer.ReadExport(blobExport(t,
		blobRow("0b1e0000-0000-4000-8000-0000000000c1", "empty-quota", "File",
			`{"file":{"path":"/srv/blobs/empty-quota"},"blobStoreQuotaConfig":{}}`),
		blobRow("0b1e0000-0000-4000-8000-0000000000c2", "null-quota", "File",
			`{"file":{"path":"/srv/blobs/null-quota"},"blobStoreQuotaConfig":null}`),
		blobRow("0b1e0000-0000-4000-8000-0000000000c3", "no-quota", "File", `{"file":{"path":"/srv/blobs/no-quota"}}`),
		blobRow("0b1e0000-0000-4000-8000-0000000000c4", "group-store", "Group",
			`{"group":{"members":["empty-quota"],"fillPolicy":"writeToFirst"},"blobStoreQuotaConfig":{}}`),
		blobRow("0b1e0000-0000-4000-8000-0000000000c5", "with-quota", "File",
			`{"file":{"path":"/srv/blobs/with-quota"},"blobStoreQuotaConfig":{"quotaType":"spaceUsedQuota","quotaLimitBytes":1073741824}}`)))
	if err != nil {
		t.Fatal(err)
	}
	want := []model.BlobStore{
		{Name: "empty-quota", Type: "File", Path: "/srv/blobs/empty-quota"},
		{Name: "group-store", Type: "Group"},
		{Name: "no-quota", Type: "File", Path: "/srv/blobs/no-quota"},
		{Name: "null-quota", Type: "File", Path: "/srv/blobs/null-quota"},
		{Name: "with-quota", Type: "File", Path: "/srv/blobs/with-quota",
			SoftQuota: &model.SoftQuota{Type: "spaceUsedQuota", Limit: 1073741824}},
	}
	if !reflect.DeepEqual(export.Snapshot.BlobStores, want) {
		g, _ := json.MarshalIndent(export.Snapshot.BlobStores, "", " ")
		t.Fatalf("blob stores\n%s", g)
	}
}

// The old product stores some integers with a zero fraction, as it stores
// the Docker port (14511.0). The limit of a soft quota with a zero fraction
// is the integer, in any form JSON allows.
func TestBlobStoreSoftQuotaWithAZeroFraction(t *testing.T) {
	quota := func(name, limit string) string {
		return blobRow("0b1e0000-0000-4000-8000-0000000000e"+name[len(name)-1:], name, "File", `{"file":{"path":"/srv/blobs/`+name+`"},`+
			`"blobStoreQuotaConfig":{"quotaType":"spaceRemainingQuota","quotaLimitBytes":`+limit+`}}`)
	}
	export, err := importer.ReadExport(blobExport(t, quota("quota-1", "1073741824.0"), quota("quota-2", "5.36870912E9"),
		quota("quota-3", "1.073741824e+9"), quota("quota-4", "2147483648.000"), quota("quota-5", "1073741824")))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, b := range export.Snapshot.BlobStores {
		got[b.Name] = b.SoftQuota
	}
	limit := func(n int64) *model.SoftQuota { return &model.SoftQuota{Type: "spaceRemainingQuota", Limit: n} }
	want := map[string]any{"quota-1": limit(1073741824), "quota-2": limit(5368709120), "quota-3": limit(1073741824),
		"quota-4": limit(2147483648), "quota-5": limit(1073741824)}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		t.Fatalf("soft quotas %s", g)
	}
}

// A soft quota with only one of type and limit, or with a limit that is no
// integer, is refused before anything is written.
func TestBlobStoreHalfASoftQuotaIsRefused(t *testing.T) {
	for _, quota := range []string{`{"quotaType":"spaceUsedQuota"}`, `{"quotaLimitBytes":1073741824}`,
		`{"quotaType":"spaceUsedQuota","quotaLimitBytes":"1073741824"}`, `{"quotaType":"spaceUsedQuota","quotaLimitBytes":1.5}`,
		`{"quotaType":"spaceUsedQuota","quotaLimitBytes":1073741824.5}`, `{"quotaType":"spaceUsedQuota","quotaLimitBytes":5.3687091205E9}`,
		`{"quotaType":"spaceUsedQuota","quotaLimitBytes":1e400}`} {
		dir := blobExport(t, blobRow("0b1e0000-0000-4000-8000-0000000000c6", "half-quota", "File",
			`{"file":{"path":"/srv/blobs/half-quota"},"blobStoreQuotaConfig":`+quota+`}`))
		cfg := dataDir(t)
		_, err := importer.Import(dir, cfg)
		want := `blob_store_configuration.jsonl: the soft quota of blob store "half-quota" has no type or no integer limit`
		if err == nil || err.Error() != want {
			t.Fatalf("%s: got %v, want %q", quota, err, want)
		}
		if _, err := os.Stat(cfg.DataDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: a refused import touched the data directory: %v", quota, err)
		}
	}
}

func repoRow(id, name, recipe, attributes string) string {
	return fmt.Sprintf(`{"id":%q,"name":%q,"recipe_name":%q,"online":true,"routing_rule_id":null,"attributes":%s,`+
		`"_removed":[]}`, id, name, recipe, attributes)
}

func dockerRow(id, name, port string) string {
	return repoRow(id, name, "docker-hosted", `{"storage":{"blobStoreName":"demo-files",`+
		`"strictContentTypeValidation":true,"writePolicy":"ALLOW"},"docker":{"httpPort":`+port+
		`,"forceBasicAuth":true,"v1Enabled":false}}`)
}

// repoExport is a copy of the content fixture with rows as its repositories.
func repoExport(t *testing.T, rows ...string) string {
	t.Helper()
	dir := copyExport(t, fixtureContent)
	writeRows(t, dir, "repository.jsonl", rows...)
	return dir
}

// The old product stores a Docker port as a number with a decimal point
// (14511.0). A number whose fraction is zero is the port, in any form JSON
// allows.
func TestDockerPortWithAZeroFraction(t *testing.T) {
	dir := repoExport(t,
		dockerRow("0e900000-0000-4000-8000-0000000000d1", "docker-a", "14521.0"),
		dockerRow("0e900000-0000-4000-8000-0000000000d2", "docker-b", "14522"),
		dockerRow("0e900000-0000-4000-8000-0000000000d3", "docker-c", "14523.000"),
		dockerRow("0e900000-0000-4000-8000-0000000000d4", "docker-d", "1.4524E4"),
		dockerRow("0e900000-0000-4000-8000-0000000000d5", "docker-e", "null"),
		repoRow("0e900000-0000-4000-8000-0000000000d6", "docker-f", "docker-hosted", `{"docker":{"forceBasicAuth":true}}`))
	cfg := dataDir(t)
	if _, err := importer.Import(dir, cfg); err != nil {
		t.Fatal(err)
	}
	defs, err := repos.LoadDefinitions(cfg.RepositoriesPath())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, d := range defs {
		got[d.Name] = nil
		if d.HTTPPort != nil {
			got[d.Name] = *d.HTTPPort
		}
	}
	want := map[string]any{"docker-a": 14521, "docker-b": 14522, "docker-c": 14523, "docker-d": 14524, "docker-e": nil,
		"docker-f": nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("http_port of repositories.json %v, want %v", got, want)
	}
}

// A Docker port with a real fraction, outside 1 to 65535 or that is no
// number stops the import with the repository named; nothing is written.
func TestDockerPortRefusals(t *testing.T) {
	for _, c := range []struct{ port, want string }{
		{"14511.5", "docker.httpPort has a fraction"},
		{"1451.15e1", "docker.httpPort has a fraction"},
		{"70000.0", "docker.httpPort is outside 1 to 65535"},
		{"0", "docker.httpPort is outside 1 to 65535"},
		{"-14511.0", "docker.httpPort is outside 1 to 65535"},
		{"1e30", "docker.httpPort is outside 1 to 65535"},
		{`"14511"`, "docker.httpPort is no number"},
		{"true", "docker.httpPort is no number"},
		{"[14511]", "docker.httpPort is no number"},
	} {
		dir := repoExport(t, dockerRow("0e900000-0000-4000-8000-0000000000d7", "docker-refused", c.port))
		want := `repository.jsonl: repository "docker-refused": ` + c.want
		if _, err := importer.ReadExport(dir); err == nil || err.Error() != want {
			t.Errorf("%s: read: got %v, want %q", c.port, err, want)
			continue
		}
		cfg := dataDir(t)
		if _, err := importer.Import(dir, cfg); err == nil || err.Error() != want {
			t.Errorf("%s: import: got %v, want %q", c.port, err, want)
		}
		if _, err := os.Stat(cfg.DataDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: a refused import touched the data directory: %v", c.port, err)
		}
	}
}

// A row that gives one field in both forms, or a value of the wrong type,
// is refused before anything is written. The refusal names the keys of the
// row, never a value.
func TestLDAPRowRefusals(t *testing.T) {
	cases := []struct {
		name, connection, mapping, want string
	}{
		{"bind user in both forms",
			edit(t, staticConnection, map[string]any{"authUsername": "cn=other,dc=example,dc=com"}), staticMapping,
			"connection.authUsername and connection.systemUsername both set authUsername"},
		{"port in the host and beside it", edit(t, staticConnection, map[string]any{"port": 1389}), staticMapping,
			"connection.host.port and connection.port both set port"},
		{"timeout in both forms", edit(t, staticConnection, map[string]any{"connectionTimeoutSeconds": 45}), staticMapping,
			"connection.connectionTimeout and connection.connectionTimeoutSeconds both set connectionTimeoutSeconds"},
		{"filter in both forms", staticConnection, edit(t, staticMapping, map[string]any{"userLdapFilter": "(uid=other)"}),
			"mapping.ldapFilter and mapping.userLdapFilter both set userLdapFilter"},
		{"one key in both columns", staticConnection,
			edit(t, staticMapping, map[string]any{"searchBase": "dc=other,dc=example,dc=com"}),
			"connection.searchBase and mapping.searchBase both set searchBase"},
		{"port of the host as text", edit(t, staticConnection, map[string]any{
			"host": map[string]any{"protocol": "ldap", "hostName": "ldap.example.com", "port": "389"}}), staticMapping,
			"connection.host.port is not of type int"},
		{"host neither text nor object", edit(t, staticConnection, map[string]any{"host": 7}), staticMapping,
			"connection.host is not of type string"},
		{"port of the host with a fraction", edit(t, staticConnection, map[string]any{
			"host": map[string]any{"protocol": "ldap", "hostName": "ldap.example.com", "port": json.Number("389.5")}}), staticMapping,
			"connection.host.port is not of type int"},
		{"timeout with a fraction", edit(t, staticConnection, map[string]any{"connectionTimeout": json.Number("30.5")}),
			staticMapping, "connection.connectionTimeout is not of type int"},
		{"retry delay with a fraction", edit(t, staticConnection, map[string]any{"connectionRetryDelay": json.Number("3.005E2")}),
			staticMapping, "connection.connectionRetryDelay is not of type int"},
		{"incidents outside the integers", edit(t, staticConnection, map[string]any{"maxIncidentsCount": json.Number("1e400")}),
			staticMapping, "connection.maxIncidentsCount is not of type int"},
		{"timeout of the REST names with a fraction", edit(t, staticConnection, map[string]any{
			"connectionTimeout": drop, "connectionTimeoutSeconds": json.Number("1.55E1")}), staticMapping,
			"connection.connectionTimeoutSeconds is not of type int"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := ldapExport(t, ldapRow("6f1c2a52-0000-4000-8000-0000000000b1", "refused", 1, c.connection, c.mapping))
			cfg := dataDir(t)
			_, err := importer.Import(dir, cfg)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v\nwant an error with %q", err, c.want)
			}
			for _, value := range []string{"cn=reader", "cn=other", "ldap.example.com", "dc=other", "uid=other", "1389", "45"} {
				if strings.Contains(err.Error(), value) {
					t.Fatalf("the refusal carries the value %q: %v", value, err)
				}
			}
			if _, err := os.Stat(cfg.DataDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refused import touched the data directory: %v", err)
			}
		})
	}
}
