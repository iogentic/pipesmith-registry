package rest

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestImpliesOfTheTests(t *testing.T) {
	cases := []struct {
		granted    string
		permission string
		want       bool
	}{
		{"nexus:*", "nexus:repository-view:maven2:demo-maven:browse", true},
		{"nexus:repository-view:maven2:*:browse,read", "nexus:repository-view:maven2:demo-maven:read", true},
		{"nexus:repository-view:maven2:*:browse,read", "nexus:repository-view:npm:demo-npm:read", false},
		{"nexus:search:read", "nexus:search:read", true},
		{"nexus:search:read", "nexus:search", false},
		{"nexus:repository-view:*:*:browse", "nexus:repository-view:-:missing:browse", true},
		{"nexus:repository-view:maven2:*:browse", "nexus:repository-view:-:missing:browse", false},
	}
	for _, c := range cases {
		if got := implies([]string{c.granted}, c.permission); got != c.want {
			t.Errorf("%s implies %s: %v", c.granted, c.permission, got)
		}
	}
}

func TestPermissionStrings(t *testing.T) {
	if got := ViewPermission("maven2", "demo-maven", "browse"); got != "nexus:repository-view:maven2:demo-maven:browse" {
		t.Fatal(got)
	}
	if got := AdminPermission("npm", "demo-npm", "read"); got != "nexus:repository-admin:npm:demo-npm:read" {
		t.Fatal(got)
	}
	if UnknownFormat != "-" {
		t.Fatal(UnknownFormat)
	}
}

func TestNotImplemented(t *testing.T) {
	v := body(t, NotImplemented("tasks", "the task list"), nil, 501)
	m := v.(map[string]any)
	if m["area"] != "tasks" || !strings.Contains(m["message"].(string), "the task list") || len(m) != 2 {
		t.Fatalf("%#v", m)
	}
}

// Repositories

func TestRepositoryList(t *testing.T) {
	f := newFixture(t)
	a, err := Repositories(f.admin())
	items := list(t, body(t, a, err, 200))
	want := []string{"demo-docker", "demo-group", "demo-maven", "demo-npm", "demo-proxy", "demo-raw", "found-only"}
	if got := names(items, "name"); !reflect.DeepEqual(got, want) {
		t.Fatalf("names %v, want %v", got, want)
	}
	for _, item := range items {
		if got := keys(item); !reflect.DeepEqual(got, []string{"attributes", "format", "name", "type", "url"}) {
			t.Fatalf("keys %v", got)
		}
		if item["url"] != "http://localhost:14650/repository/"+item["name"].(string) {
			t.Errorf("url %v", item["url"])
		}
	}
	proxy := items[4]
	if proxy["format"] != "maven2" || proxy["type"] != "proxy" {
		t.Fatalf("%#v", proxy)
	}
	if got := proxy["attributes"].(map[string]any)["proxy"].(map[string]any)["remoteUrl"]; got != "https://repo.example.com/maven2" {
		t.Fatalf("remoteUrl %v", got)
	}
	if len(items[2]["attributes"].(map[string]any)) != 0 {
		t.Fatalf("hosted attributes %v", items[2]["attributes"])
	}
}

func TestRepositoryListShowsOnlyBrowsable(t *testing.T) {
	f := newFixture(t)
	a, err := Repositories(f.env("nexus:repository-view:maven2:*:browse"))
	got := names(list(t, body(t, a, err, 200)), "name")
	if !reflect.DeepEqual(got, []string{"demo-group", "demo-maven", "demo-proxy"}) {
		t.Fatalf("%v", got)
	}
	a, err = Repositories(f.env("nexus:search:read"))
	if got := list(t, body(t, a, err, 200)); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestRepositoryByName(t *testing.T) {
	f := newFixture(t)
	a, err := Repository(f.admin(), "demo-npm")
	m := body(t, a, err, 200).(map[string]any)
	if m["name"] != "demo-npm" || m["format"] != "npm" || m["type"] != "hosted" || m["url"] != "http://localhost:14650/repository/demo-npm" {
		t.Fatalf("%#v", m)
	}
	a, err = Repository(f.admin(), "missing")
	message(t, body(t, a, err, 404))
}

func TestRepositoryDetail(t *testing.T) {
	f := newFixture(t)
	a, err := RepositoryDetail(f.admin(), "maven", "hosted", "demo-maven")
	m := body(t, a, err, 200).(map[string]any)
	want := map[string]any{
		"name": "demo-maven", "format": "maven2", "type": "hosted", "online": true, "url": "http://localhost:14650/repository/demo-maven",
		"storage": map[string]any{"blobStoreName": "store-a", "strictContentTypeValidation": true, "writePolicy": "ALLOW_ONCE"},
		"maven":   map[string]any{"versionPolicy": "RELEASE", "layoutPolicy": "STRICT"},
		"cleanup": map[string]any{"policyNames": []any{"weekly"}},
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got  %#v\nwant %#v", m, want)
	}
	a, err = RepositoryDetail(f.admin(), "npm", "hosted", "demo-npm")
	if m := body(t, a, err, 200).(map[string]any); m["online"] != false {
		t.Fatalf("online %v", m["online"])
	}
	for _, c := range [][3]string{{"maven", "proxy", "demo-maven"}, {"npm", "hosted", "demo-maven"}, {"maven2", "hosted", "demo-maven"},
		{"maven", "hosted", "missing"}} {
		a, err := RepositoryDetail(f.admin(), c[0], c[1], c[2])
		message(t, body(t, a, err, 404))
	}
}

func TestRepositoryDetailNormalizesTheDatabaseFormAndDropsSecrets(t *testing.T) {
	f := newFixture(t)
	a, err := RepositoryDetail(f.admin(), "maven", "proxy", "demo-proxy")
	m := body(t, a, err, 200).(map[string]any)
	if _, ok := m["httpclient"]; ok {
		t.Fatal("httpclient kept")
	}
	auth := m["httpClient"].(map[string]any)["authentication"].(map[string]any)
	if auth["username"] != "demo-pull" || auth["type"] != "username" {
		t.Fatalf("%#v", auth)
	}
	if _, ok := auth["password"]; ok {
		t.Fatal("password in the answer")
	}
	if got := m["cleanup"].(map[string]any); !reflect.DeepEqual(got, map[string]any{"policyNames": []any{"monthly"}}) {
		t.Fatalf("cleanup %#v", got)
	}
	if m["proxy"].(map[string]any)["contentMaxAge"] != json.Number("-1") {
		t.Fatalf("%#v", m["proxy"])
	}
	data, _ := a.JSON()
	if strings.Contains(string(data), "example-secret-1") {
		t.Fatal("the stored password is in the answer")
	}
	// the stored record is not changed by the answer
	if _, ok := f.snap.Repositories[4].Attributes["httpclient"]; !ok {
		t.Fatal("the snapshot was changed")
	}
}

func TestGroupDetailNamesItsMembers(t *testing.T) {
	f := newFixture(t)
	a, err := RepositoryDetail(f.admin(), "maven", "group", "demo-group")
	m := body(t, a, err, 200).(map[string]any)
	if got := m["group"].(map[string]any)["memberNames"]; !reflect.DeepEqual(got, []any{"demo-maven", "demo-proxy"}) {
		t.Fatalf("%#v", got)
	}
}

func TestRepositorySettings(t *testing.T) {
	f := newFixture(t)
	a, err := RepositorySettings(f.admin())
	items := list(t, body(t, a, err, 200))
	if got := names(items, "name"); !reflect.DeepEqual(got, []string{"demo-docker", "demo-group", "demo-maven", "demo-npm", "demo-proxy",
		"demo-raw", "found-only"}) {
		t.Fatalf("%v", got)
	}
	if items[0]["docker"].(map[string]any)["httpPort"] != json.Number("14690") {
		t.Fatalf("%#v", items[0])
	}
	a, err = RepositorySettings(f.env("nexus:repository-admin:maven2:demo-maven:read"))
	if got := names(list(t, body(t, a, err, 200)), "name"); !reflect.DeepEqual(got, []string{"demo-maven"}) {
		t.Fatalf("%v", got)
	}
	a, err = RepositorySettings(f.env("nexus:repository-view:*:*:*"))
	message(t, body(t, a, err, 403))
}

// Blob stores

func TestBlobStores(t *testing.T) {
	f := newFixture(t)
	a, err := BlobStores(f.admin())
	items := list(t, body(t, a, err, 200))
	if got := names(items, "name"); !reflect.DeepEqual(got, []string{"gone", "old-cloud", "store-a", "store-b"}) {
		t.Fatalf("%v", got)
	}
	stats, err := f.ix.StoreStats()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string][2]int64{}
	for _, s := range stats {
		counts[s.Store] = [2]int64{s.Files, s.Bytes}
	}
	for _, item := range items {
		if got := keys(item); !reflect.DeepEqual(got, []string{"availableSpaceInBytes", "blobCount", "name", "softQuota", "totalSizeInBytes",
			"type", "unavailable"}) {
			t.Fatalf("keys %v", got)
		}
		name := item["name"].(string)
		want := counts[name]
		if item["blobCount"] != json.Number(itoa(want[0])) || item["totalSizeInBytes"] != json.Number(itoa(want[1])) {
			t.Errorf("%s: %v %v, want %v", name, item["blobCount"], item["totalSizeInBytes"], want)
		}
		available := name == "store-a" || name == "store-b"
		if item["unavailable"] != !available {
			t.Errorf("%s: unavailable %v", name, item["unavailable"])
		}
		free, _ := item["availableSpaceInBytes"].(json.Number).Int64()
		if available != (free > 0) {
			t.Errorf("%s: free %d", name, free)
		}
	}
	if counts["store-a"][0] != 14 || counts["store-b"][0] != 3 {
		t.Fatalf("index counts %v", counts)
	}
	if items[1]["type"] != "S3" || items[2]["type"] != "File" || items[0]["type"] != "File" {
		t.Fatalf("types %v %v %v", items[0]["type"], items[1]["type"], items[2]["type"])
	}
	quota := items[2]["softQuota"].(map[string]any)
	if quota["type"] != "spaceUsedQuota" || quota["limit"] != json.Number("10") || items[3]["softQuota"] != nil {
		t.Fatalf("%#v %#v", quota, items[3]["softQuota"])
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestFileBlobStore(t *testing.T) {
	f := newFixture(t)
	a, err := FileBlobStore(f.admin(), "store-a")
	m := body(t, a, err, 200).(map[string]any)
	if m["path"] != "/srv/old-host/store-a" || m["softQuota"].(map[string]any)["limit"] != json.Number("10") {
		t.Fatalf("%#v", m)
	}
	a, err = FileBlobStore(f.admin(), "gone")
	if m := body(t, a, err, 200).(map[string]any); m["path"] != f.cfg.Stores[2].Root || m["softQuota"] != nil {
		t.Fatalf("%#v", m)
	}
	for _, name := range []string{"old-cloud", "missing"} {
		a, err := FileBlobStore(f.admin(), name)
		message(t, body(t, a, err, 404))
	}
}

func TestQuotaStatus(t *testing.T) {
	f := newFixture(t)
	a, err := QuotaStatus(f.admin(), "store-a")
	m := body(t, a, err, 200).(map[string]any)
	if m["isViolation"] != true || m["blobStoreName"] != "store-a" || m["message"] == "" {
		t.Fatalf("%#v", m)
	}
	a, err = QuotaStatus(f.admin(), "store-b")
	if m := body(t, a, err, 200).(map[string]any); m["isViolation"] != false {
		t.Fatalf("%#v", m)
	}
	a, err = QuotaStatus(f.admin(), "missing")
	message(t, body(t, a, err, 404))
}

// Security

func TestPrivileges(t *testing.T) {
	f := newFixture(t)
	a, err := Privileges(f.admin())
	items := list(t, body(t, a, err, 200))
	byName := map[string]map[string]any{}
	for _, item := range items {
		byName[item["name"].(string)] = item
	}
	want := map[string]map[string]any{
		"demo-all": {"type": "wildcard", "name": "demo-all", "description": "Everything", "readOnly": true, "pattern": "nexus:*"},
		"demo-maven-read": {"type": "repository-view", "name": "demo-maven-read", "description": "Browse and read Maven", "readOnly": false,
			"format": "maven2", "repository": "*", "actions": []any{"BROWSE", "READ"}},
		"demo-search": {"type": "application", "name": "demo-search", "description": "Search", "readOnly": false, "domain": "search",
			"actions": []any{"READ"}},
		"demo-users-all": {"type": "application", "name": "demo-users-all", "description": "", "readOnly": false, "domain": "users",
			"actions": []any{"ALL"}},
		"demo-script": {"type": "script", "name": "demo-script", "description": "", "readOnly": false, "scriptName": "cleanup",
			"actions": []any{"RUN", "READ"}},
	}
	if !reflect.DeepEqual(byName, want) {
		t.Fatalf("got  %#v\nwant %#v", byName, want)
	}
	a, err = Privilege(f.admin(), "demo-search")
	if m := body(t, a, err, 200).(map[string]any); !reflect.DeepEqual(m, want["demo-search"]) {
		t.Fatalf("%#v", m)
	}
	a, err = Privilege(f.admin(), "missing")
	message(t, body(t, a, err, 404))
}

func TestRoles(t *testing.T) {
	f := newFixture(t)
	a, err := Roles(f.admin(), query())
	items := list(t, body(t, a, err, 200))
	if got := names(items, "id"); !reflect.DeepEqual(got, []string{"demo-administrators", "demo-readers", "platform-team"}) {
		t.Fatalf("%v", got)
	}
	readers := items[1]
	want := map[string]any{"id": "demo-readers", "source": "default", "name": "Readers", "description": "Read the Maven repositories",
		"readOnly": false, "privileges": []any{"demo-maven-read", "demo-search"}, "roles": []any{}}
	if !reflect.DeepEqual(readers, want) {
		t.Fatalf("%#v", readers)
	}
	a, err = Roles(f.admin(), query("source", "default"))
	if len(list(t, body(t, a, err, 200))) != 3 {
		t.Fatal("source default")
	}
	a, err = Roles(f.admin(), query("source", "LDAP"))
	if m := body(t, a, err, 501).(map[string]any); m["area"] != "security" {
		t.Fatalf("%#v", m)
	}
	a, err = Roles(f.admin(), query("source", "Crowd"))
	message(t, body(t, a, err, 404))
	a, err = Role(f.admin(), "platform-team")
	if m := body(t, a, err, 200).(map[string]any); !reflect.DeepEqual(m["roles"], []any{"demo-readers"}) || !reflect.DeepEqual(m["privileges"], []any{}) {
		t.Fatalf("%#v", m)
	}
	a, err = Role(f.admin(), "missing")
	message(t, body(t, a, err, 404))
}

func TestUserSources(t *testing.T) {
	f := newFixture(t)
	a, err := UserSources(f.admin())
	want := []any{map[string]any{"id": "default", "name": "Local"}, map[string]any{"id": "LDAP", "name": "LDAP"}}
	if got := body(t, a, err, 200); !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v", got)
	}
	f.snap.LDAP = nil
	a, err = UserSources(f.admin())
	if got := body(t, a, err, 200); !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("%#v", got)
	}
}

func TestUsers(t *testing.T) {
	f := newFixture(t)
	a, err := Users(f.admin(), query())
	items := list(t, body(t, a, err, 200))
	if got := names(items, "userId"); !reflect.DeepEqual(got, []string{"demo-admin", "demo-locked", "demo-reader", "dir.person"}) {
		t.Fatalf("%v", got)
	}
	want := map[string]any{"userId": "demo-reader", "firstName": "Demo", "lastName": "Reader", "emailAddress": "demo-reader@example.com",
		"source": "default", "status": "active", "readOnly": false, "roles": []any{"demo-readers"}, "externalRoles": []any{}}
	if !reflect.DeepEqual(items[2], want) {
		t.Fatalf("%#v", items[2])
	}
	if items[1]["status"] != "locked" || !reflect.DeepEqual(items[1]["roles"], []any{}) {
		t.Fatalf("%#v", items[1])
	}
	external := items[3]
	if external["source"] != "LDAP" || external["readOnly"] != true || !reflect.DeepEqual(external["roles"], []any{"platform-team"}) {
		t.Fatalf("%#v", external)
	}
	data, _ := a.JSON()
	for _, hash := range []string{"$shiro1$", "0123456789abcdef0123456789abcdef01234567", "password"} {
		if strings.Contains(string(data), hash) {
			t.Fatalf("%q in the user answer", hash)
		}
	}
	a, err = Users(f.admin(), query("source", "default", "userId", "READ"))
	if got := names(list(t, body(t, a, err, 200)), "userId"); !reflect.DeepEqual(got, []string{"demo-reader"}) {
		t.Fatalf("%v", got)
	}
	a, err = Users(f.admin(), query("source", "LDAP"))
	if got := names(list(t, body(t, a, err, 200)), "userId"); !reflect.DeepEqual(got, []string{"dir.person"}) {
		t.Fatalf("%v", got)
	}
	a, err = Users(f.admin(), query("source", "Crowd"))
	if got := list(t, body(t, a, err, 200)); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestAnonymousAndRealms(t *testing.T) {
	f := newFixture(t)
	a, err := Anonymous(f.admin())
	if got := body(t, a, err, 200); !reflect.DeepEqual(got, map[string]any{"enabled": false, "userId": "anonymous",
		"realmName": "NexusAuthenticatingRealm"}) {
		t.Fatalf("%#v", got)
	}
	a, err = RealmsActive(f.admin())
	if got := body(t, a, err, 200); !reflect.DeepEqual(got, []any{"NexusAuthenticatingRealm", "LdapRealm", "DockerToken"}) {
		t.Fatalf("%#v", got)
	}
	f.snap.Realms = nil
	a, err = RealmsActive(f.admin())
	if got := body(t, a, err, 200); !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("%#v", got)
	}
	a, err = RealmsAvailable(f.admin())
	items := list(t, body(t, a, err, 200))
	if got := names(items, "id"); !reflect.DeepEqual(got, []string{"NexusAuthenticatingRealm", "LdapRealm"}) {
		t.Fatalf("%v", got)
	}
	for _, item := range items {
		if item["name"] == "" {
			t.Fatalf("%#v", item)
		}
	}
}

func TestLDAPServers(t *testing.T) {
	f := newFixture(t)
	a, err := LDAPServers(f.admin())
	items := list(t, body(t, a, err, 200))
	if got := names(items, "name"); !reflect.DeepEqual(got, []string{"corp-directory", "lab-directory"}) {
		t.Fatalf("%v", got)
	}
	first := items[0]
	if first["host"] != "ldap.example.com" || first["port"] != json.Number("636") || first["authScheme"] != "SIMPLE" || first["id"] != "conn-1" {
		t.Fatalf("%#v", first)
	}
	if _, ok := first["authPassword"]; ok {
		t.Fatal("authPassword in the answer")
	}
	if len(first) != 31 {
		t.Fatalf("%d fields: %v", len(first), keys(first))
	}
	a, err = LDAPServer(f.admin(), "lab-directory")
	if m := body(t, a, err, 200).(map[string]any); m["id"] != "conn-2" {
		t.Fatalf("%#v", m)
	}
	a, err = LDAPServer(f.admin(), "missing")
	message(t, body(t, a, err, 404))
}

// System

func TestStatusChecks(t *testing.T) {
	f := newFixture(t)
	a, err := StatusChecks(f.admin())
	m := body(t, a, err, 200).(map[string]any)
	if got := keys(m); !reflect.DeepEqual(got, []string{"Blob Stores", "Index"}) {
		t.Fatalf("%v", got)
	}
	for name, v := range m {
		check := v.(map[string]any)
		if got := keys(check); !reflect.DeepEqual(got, []string{"details", "duration", "error", "healthy", "message", "time", "timestamp"}) {
			t.Fatalf("%s: %v", name, got)
		}
	}
	if m["Index"].(map[string]any)["healthy"] != true {
		t.Fatalf("%#v", m["Index"])
	}
	stores := m["Blob Stores"].(map[string]any)
	if stores["healthy"] != false || !strings.Contains(stores["message"].(string), "gone") {
		t.Fatalf("%#v", stores)
	}
	f.envs = f.envs[:2]
	a, err = StatusChecks(f.admin())
	if m := body(t, a, err, 200).(map[string]any); m["Blob Stores"].(map[string]any)["healthy"] != true {
		t.Fatalf("%#v", m)
	}
}

func TestReadOnly(t *testing.T) {
	m := body(t, ReadOnly(), nil, 200).(map[string]any)
	if m["frozen"] != true || m["systemInitiated"] != true || m["summaryReason"] == "" || len(m) != 3 {
		t.Fatalf("%#v", m)
	}
}
