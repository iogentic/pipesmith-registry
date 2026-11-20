package server

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

const repositoriesPath = "/service/rest/v1/repositories"

// upstreamSecret is the password of the upstream of the created proxies;
// no answer and no record may hold it (A11, ruling ST9).
const upstreamSecret = "example-up-secret"

// repositoryBody is a body of the contract for a create of the kind.
func repositoryBody(name, format, kind string, members ...string) string {
	b := map[string]any{"name": name, "online": true,
		"storage": map[string]any{"blobStoreName": "local-write", "strictContentTypeValidation": true}}
	switch kind {
	case "hosted":
		b["storage"].(map[string]any)["writePolicy"] = "ALLOW"
		b["cleanup"] = map[string]any{"policyNames": []any{}}
		b["component"] = map[string]any{"proprietaryComponents": false}
	case "proxy":
		b["proxy"] = map[string]any{"remoteUrl": "https://203.0.113.10/" + format + "/", "contentMaxAge": 1440, "metadataMaxAge": 1440}
		b["negativeCache"] = map[string]any{"enabled": true, "timeToLive": 1440}
		b["httpClient"] = map[string]any{"blocked": false, "autoBlock": true,
			"authentication": map[string]any{"type": "username", "username": "demo-up", "password": upstreamSecret}}
		b["routingRule"] = nil
	case "group":
		list := []any{}
		for _, m := range members {
			list = append(list, m)
		}
		b["group"] = map[string]any{"memberNames": list}
	}
	switch format {
	case "maven2":
		b["maven"] = map[string]any{"versionPolicy": "MIXED", "layoutPolicy": "PERMISSIVE", "contentDisposition": "INLINE"}
	case "docker":
		b["docker"] = map[string]any{"v1Enabled": false, "forceBasicAuth": true, "httpPort": nil, "httpsPort": nil}
	case "raw":
		b["raw"] = map[string]any{"contentDisposition": "ATTACHMENT"}
	}
	data, _ := json.Marshal(b)
	return string(data)
}

var segmentOfFormat = map[string]string{"maven2": "maven", "npm": "npm", "docker": "docker", "raw": "raw"}

// noUpstreamSecret fails for an answer or a record that holds the upstream
// secret.
func noUpstreamSecret(t *testing.T, label string, data []byte) {
	t.Helper()
	if strings.Contains(string(data), upstreamSecret) {
		t.Fatalf("%s holds the upstream secret: %s", label, data)
	}
}

// The twelve kinds of A5 are created, read, saved and deleted; a raw PUT
// into a created hosted repository lands in local-write and its group
// serves it; the upstream secret lies in its file only.
func TestRepositoryWrites(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	sec := w.opts.security.(*fakeSecurity)
	data := w.srv.rp.cfg.DataDir
	send := func(method, target, body string) *answer {
		t.Helper()
		r := do(t, w.addr(), method, target, []byte(body), "basic", asAdmin, "Content-Type", jsonType)
		noUpstreamSecret(t, method+" "+target, r.body)
		return r
	}
	for _, format := range []string{"maven2", "npm", "docker", "raw"} {
		seg := segmentOfFormat[format]
		for _, kind := range []string{"hosted", "proxy", "group"} {
			name := "demo-new-" + format + "-" + kind
			body := repositoryBody(name, format, kind, "demo-new-"+format+"-hosted")
			if r := send("POST", repositoriesPath+"/"+seg+"/"+kind, body); r.status != 201 || len(r.body) != 0 {
				t.Fatalf("create %s: %d %q", name, r.status, r.body)
			}
			got := send("GET", repositoriesPath+"/"+seg+"/"+kind+"/"+name, "")
			if got.status != 200 {
				t.Fatalf("read %s: %d %q", name, got.status, got.body)
			}
			if m := got.object(t); m["name"] != name || m["format"] != format || m["type"] != kind || m["online"] != true {
				t.Fatalf("read %s: %v", name, m)
			}
			if r := send("PUT", repositoriesPath+"/"+seg+"/"+kind+"/"+name, body); r.status != 204 {
				t.Fatalf("save %s: %d %q", name, r.status, r.body)
			}
		}
	}
	records, _ := json.Marshal(sec.Snapshot().Repositories)
	noUpstreamSecret(t, "the records", records)
	secretFile := filepath.Join(data, "secrets", "remote", "demo-new-raw-proxy")
	if b, err := os.ReadFile(secretFile); err != nil || string(b) != upstreamSecret {
		t.Fatalf("the secret file: %q %v", b, err)
	}
	if info, _ := os.Stat(secretFile); info.Mode().Perm() != 0o600 {
		t.Fatalf("the secret file has mode %v", info.Mode())
	}
	settings := send("GET", "/service/rest/v1/repositorySettings", "")
	noUpstreamSecret(t, "repositorySettings", settings.body)

	// R9: a save without an authentication object keeps the secret file and
	// the stored user; the type none removes both
	var proxyBody map[string]any
	_ = json.Unmarshal([]byte(repositoryBody("demo-new-raw-proxy", "raw", "proxy")), &proxyBody)
	delete(proxyBody["httpClient"].(map[string]any), "authentication")
	kept, _ := json.Marshal(proxyBody)
	if r := send("PUT", repositoriesPath+"/raw/proxy/demo-new-raw-proxy", string(kept)); r.status != 204 {
		t.Fatalf("save without authentication: %d %q", r.status, r.body)
	}
	if _, err := os.Stat(secretFile); err != nil {
		t.Fatal("a save without an authentication object removed the secret file")
	}
	if m := send("GET", repositoriesPath+"/raw/proxy/demo-new-raw-proxy", "").object(t); fmt.Sprint(m["httpClient"]) == "map[autoBlock:true blocked:false]" {
		t.Fatalf("the stored authentication went: %v", m["httpClient"])
	}
	proxyBody["httpClient"].(map[string]any)["authentication"] = map[string]any{"type": "none"}
	none, _ := json.Marshal(proxyBody)
	if r := send("PUT", repositoriesPath+"/raw/proxy/demo-new-raw-proxy", string(none)); r.status != 204 {
		t.Fatalf("save with type none: %d %q", r.status, r.body)
	}
	if _, err := os.Stat(secretFile); !os.IsNotExist(err) {
		t.Fatalf("the type none kept the secret file: %v", err)
	}

	// a write into a created hosted repository lands in local-write, and its
	// group serves it
	if r := do(t, w.addr(), "PUT", "/repository/demo-new-raw-hosted/docs/a.txt", []byte("made"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("PUT: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-new-raw-group/docs/a.txt", nil, "basic", asAdmin); r.status != 200 || string(r.body) != "made" {
		t.Fatalf("GET through the group: %d %q", r.status, r.body)
	}
	if row, _ := w.srv.rp.index.Lookup("demo-new-raw-hosted", "docs/a.txt"); row == nil || row.Store != "local-write" {
		t.Fatalf("%+v", row)
	}

	// A7: a member of a group stays; the delete removes rows and marks blobs
	if r := send("DELETE", repositoriesPath+"/demo-new-raw-hosted", ""); r.status != 409 || !strings.Contains(string(r.body), "demo-new-raw-group") {
		t.Fatalf("delete of a member: %d %q", r.status, r.body)
	}
	for _, name := range []string{"demo-new-raw-group", "demo-new-raw-hosted"} {
		if r := send("DELETE", repositoriesPath+"/"+name, ""); r.status != 204 {
			t.Fatalf("delete %s: %d %q", name, r.status, r.body)
		}
	}
	if rows, _ := w.srv.rp.index.RepoRows("demo-new-raw-hosted"); len(rows) != 0 {
		t.Fatalf("rows after the delete: %+v", rows)
	}
	if r := do(t, w.addr(), "GET", "/repository/demo-new-raw-hosted/docs/a.txt", nil, "basic", asAdmin); r.status != 404 {
		t.Fatalf("GET after the delete: %d", r.status)
	}
	deleted := do(t, w.addr(), "GET", "/api/blob-stores/local-write/deleted", nil, "basic", asAdmin)
	if !strings.Contains(string(deleted.body), `"blobCount":1`) {
		t.Fatalf("the marked blobs: %s", deleted.body)
	}
	if r := send("DELETE", repositoriesPath+"/demo-new-raw-proxy", ""); r.status != 204 {
		t.Fatalf("delete of the proxy: %d %q", r.status, r.body)
	}
	for _, name := range []string{"demo-new-raw-group", "demo-new-raw-hosted", "demo-new-raw-proxy"} {
		if strings.Contains(string(send("GET", repositoriesPath, "").body), `"`+name+`"`) {
			t.Fatalf("%s is listed after its delete", name)
		}
	}

	// ST6: rebuild index
	for _, name := range []string{"demo-new-maven2-hosted", "demo-new-npm-proxy"} {
		if r := send("POST", repositoriesPath+"/"+name+"/rebuild-index", ""); r.status != 204 {
			t.Fatalf("rebuild %s: %d %q", name, r.status, r.body)
		}
	}
}

// The refusals of the repository writes, each without a write.
func TestRepositoryWriteRefusals(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	sec := w.opts.security.(*fakeSecurity)
	sec.addBasic("demo-repo-admin:repo-pw", subject("demo-repo-admin", "", "nexus:repository-admin:*:*:*", "nexus:repository-view:*:*:*"))
	sec.addBasic("demo-raw-adder:adder-pw", subject("demo-raw-adder", "", "nexus:repository-admin:raw:*:add"))
	privateProxy := strings.Replace(repositoryBody("demo-p", "raw", "proxy"), "203.0.113.10", "127.0.0.1", 1)
	userInRemote := strings.Replace(repositoryBody("demo-p", "raw", "proxy"), "https://", "https://u:p@", 1)
	otherStore := strings.Replace(repositoryBody("demo-h", "raw", "hosted"), `"local-write"`, `"store-a"`, 1)
	dockerPort := strings.Replace(repositoryBody("demo-d", "docker", "hosted"), `"httpPort":null`, `"httpPort":14695`, 1)
	before, _ := sec.writes()
	for _, c := range []struct {
		who, method, target, body string
		status                    int
		holds                     string
	}{
		{asAdmin, "POST", repositoriesPath + "/pypi/hosted", `{"name":"demo-x"}`, 501, `"area":"repositories"`},
		{asAdmin, "POST", repositoriesPath + "/raw/virtual", `{"name":"demo-x"}`, 501, `"area":"repositories"`},
		{asAdmin, "POST", repositoriesPath + "/raw/hosted", repositoryBody("demo-raw", "raw", "hosted"), 409, "demo-raw"},
		{asAdmin, "POST", repositoriesPath + "/raw/hosted", otherStore, 400, "storage.blobStoreName"},
		{asAdmin, "POST", repositoriesPath + "/docker/hosted", dockerPort, 400, "docker.httpPort"},
		{asAdmin, "POST", repositoriesPath + "/raw/proxy", userInRemote, 400, "proxy.remoteUrl"},
		{asAdmin, "POST", repositoriesPath + "/raw/proxy", strings.Replace(repositoryBody("demo-p", "raw", "proxy"), `,"remoteUrl":"https://203.0.113.10/raw/"`, "", 1), 400, "proxy.remoteUrl"},
		{asAdmin, "POST", repositoriesPath + "/raw/group", repositoryBody("demo-g", "raw", "group", "demo-missing"), 400, "group.memberNames"},
		{asAdmin, "POST", repositoriesPath + "/raw/group", repositoryBody("demo-g", "raw", "group", "demo-maven"), 400, "group.memberNames"},
		{"demo-repo-admin:repo-pw", "POST", repositoriesPath + "/raw/proxy", privateProxy, 400, "127.0.0.1"},
		{"demo-raw-adder:adder-pw", "POST", repositoriesPath + "/raw/hosted", repositoryBody("demo-h", "raw", "hosted"), 403, "demo-h"},
		{asReader, "POST", repositoriesPath + "/raw/hosted", repositoryBody("demo-h", "raw", "hosted"), 403, ""},
		{asReader, "PUT", repositoriesPath + "/raw/hosted/demo-raw", repositoryBody("demo-raw", "raw", "hosted"), 403, ""},
		{asReader, "DELETE", repositoriesPath + "/demo-raw", "", 403, ""},
		{asNobody, "POST", repositoriesPath + "/demo-raw/rebuild-index", "", 403, ""},
		{asAdmin, "PUT", repositoriesPath + "/raw/hosted/demo-none", repositoryBody("demo-none", "raw", "hosted"), 404, ""},
		{asAdmin, "PUT", repositoriesPath + "/maven/hosted/demo-raw", repositoryBody("demo-raw", "raw", "hosted"), 404, ""},
		{asAdmin, "PUT", repositoriesPath + "/raw/hosted/demo-raw", repositoryBody("demo-raw", "raw", "hosted"), 409, "repositories.json"},
		{asAdmin, "DELETE", repositoriesPath + "/demo-raw", "", 409, "repositories.json"},
		{asAdmin, "DELETE", repositoriesPath + "/demo-none", "", 404, ""},
	} {
		r := do(t, w.addr(), c.method, c.target, []byte(c.body), "basic", c.who, "Content-Type", jsonType)
		if r.status != c.status || !strings.Contains(string(r.body), c.holds) {
			t.Errorf("%s %s %s: %d %q, want %d with %q", c.who, c.method, c.target, r.status, r.body, c.status, c.holds)
		}
	}
	if after, _ := sec.writes(); after != before {
		t.Fatalf("a refusal wrote: %d updates, want %d", after, before)
	}
	if _, err := os.Stat(filepath.Join(w.srv.rp.cfg.DataDir, "secrets", "remote", "demo-p")); !os.IsNotExist(err) {
		t.Fatalf("a refused proxy left its secret file: %v", err)
	}
	// with nexus:* the closed address passes (ST8), and the rights of every
	// generated privilege pass ST10
	if r := do(t, w.addr(), "POST", repositoriesPath+"/raw/proxy", []byte(privateProxy), "basic", asAdmin, "Content-Type", jsonType); r.status != 201 {
		t.Fatalf("a closed address with nexus:*: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "POST", repositoriesPath+"/raw/hosted", []byte(repositoryBody("demo-h", "raw", "hosted")), "basic", "demo-repo-admin:repo-pw", "Content-Type", jsonType); r.status != 201 {
		t.Fatalf("a create with the administration rights of every repository: %d %q", r.status, r.body)
	}
}

// The repository writes change the records with writes off as well (ruling
// ST4); a delete that would mark blobs of local-write needs the store.
func TestRepositoryWritesWithWritesOff(t *testing.T) {
	sec := newFakeSecurity()
	a := &secureApp{app: startWriting(t, sec, false), sec: sec}
	r := do(t, a.addr(), "POST", repositoriesPath+"/raw/hosted", []byte(repositoryBody("demo-h", "raw", "hosted")), "basic", asAdmin, "Content-Type", jsonType)
	if r.status != 201 {
		t.Fatalf("create: %d %q", r.status, r.body)
	}
	if r := a.send("GET", repositoriesPath+"/raw/hosted/demo-h", "basic", asAdmin); r.status != 200 {
		t.Fatalf("read: %d %q", r.status, r.body)
	}
	if r := do(t, a.addr(), "PUT", "/repository/demo-h/a.txt", []byte("x"), "basic", asAdmin); r.status == 201 {
		t.Fatal("a content write with writes off")
	}
	if r := a.send("DELETE", repositoriesPath+"/demo-h", "basic", asAdmin); r.status != 204 {
		t.Fatalf("delete: %d %q", r.status, r.body)
	}
	if r := a.send("POST", repositoriesPath+"/demo-maven/rebuild-index", "basic", asAdmin); r.status != 503 {
		t.Fatalf("rebuild with writes off: %d %q", r.status, r.body)
	}
}

// fakeLookup makes the resolver of the rules ST8 and ST11 answer addrs for
// every name while a test runs.
func fakeLookup(t *testing.T, addrs ...string) {
	t.Helper()
	before := lookupHost
	lookupHost = func(string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
	t.Cleanup(func() { lookupHost = before })
}

// Ruling ST11: the remote of a proxy that a subject without the full
// permission set created names a public address at the create, and resolves
// to loopback at dial time; the dial guard refuses it, and the remote sees
// no request. The same remote created with the full permission set is
// fetched from.
func TestAProxyOfARepositoryAdminDialsNoClosedAddress(t *testing.T) {
	fakeLookup(t, "203.0.113.10")
	u := startUpstream(t, upstreamSetup{upstream: true, writes: true})
	u.sec.addBasic("demo-repo-admin:repo-pw", subject("demo-repo-admin", "", "nexus:repository-admin:*:*:*", "nexus:repository-view:*:*:*"))
	u.remote.Script("/raw/docs/a.txt", testkit.OK("CANARY-CLOSED-SERVICE-58c2"))
	remote := u.remote.URLAt("localhost") + "/raw/"
	for _, c := range []struct {
		name, who string
		reached   bool
	}{{"demo-made-closed", "demo-repo-admin:repo-pw", false}, {"demo-made-open", asAdmin, true}} {
		b := strings.Replace(repositoryBody(c.name, "raw", "proxy"), "https://203.0.113.10/raw/", remote, 1)
		if r := do(t, u.addr(), "POST", repositoriesPath+"/raw/proxy", []byte(b), "basic", c.who, "Content-Type", jsonType); r.status != 201 {
			t.Fatalf("%s: create %d %q", c.name, r.status, r.body)
		}
		u.remote.Reset()
		got := u.get(c.name, "docs/a.txt")
		if reached := got.status == 200; reached != c.reached || u.remote.Count("/raw/docs/a.txt") != map[bool]int{true: 1, false: 0}[c.reached] {
			t.Fatalf("%s: %d %q, the remote saw %d requests", c.name, got.status, got.body, u.remote.Count("/raw/docs/a.txt"))
		}
		var held any
		for _, r := range u.sec.Snapshot().Repositories {
			if r.Name == c.name {
				held = r.Attributes["remote_public_only"]
			}
		}
		if (held == true) == c.reached {
			t.Fatalf("%s: remote_public_only is %v", c.name, held)
		}
	}
}

// Ruling ST12: a create gives no right the creator lacks. Without the view
// permission of the new repository the create is refused, and a group
// that adds a member whose content the creator may not read is refused;
// each is read back unchanged.
func TestNoRightsThroughANewRepository(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	sec := w.opts.security.(*fakeSecurity)
	sec.addBasic("demo-admin-only:pw", subject("demo-admin-only", "", "nexus:repository-admin:*:*:*"))
	sec.addBasic("demo-group-maker:pw", subject("demo-group-maker", "", "nexus:repository-admin:raw:*:*", "nexus:repository-view:raw:demo-g:*"))
	sec.addBasic("demo-raw-keeper:pw", subject("demo-raw-keeper", "", "nexus:repository-admin:raw:*:*", "nexus:repository-view:raw:*:*"))
	before, _ := sec.writes()
	for _, c := range []struct {
		who, target, body, holds string
	}{
		{"demo-admin-only:pw", repositoriesPath + "/raw/hosted", repositoryBody("demo-x", "raw", "hosted"), "nexus:repository-view:raw:demo-x:"},
		{"demo-group-maker:pw", repositoriesPath + "/raw/group", repositoryBody("demo-g", "raw", "group", "demo-raw"), "demo-raw"},
	} {
		r := do(t, w.addr(), "POST", c.target, []byte(c.body), "basic", c.who, "Content-Type", jsonType)
		if r.status != 403 || !strings.Contains(string(r.body), c.holds) {
			t.Fatalf("%s: %d %q, want 403 with %q", c.who, r.status, r.body, c.holds)
		}
	}
	if after, _ := sec.writes(); after != before {
		t.Fatalf("a refused create wrote: %d updates, want %d", after, before)
	}
	for _, target := range []string{repositoriesPath + "/raw/hosted/demo-x", repositoriesPath + "/raw/group/demo-g"} {
		if r := do(t, w.addr(), "GET", target, nil, "basic", asAdmin); r.status != 404 {
			t.Fatalf("%s after the refusal: %d", target, r.status)
		}
	}
	if r := do(t, w.addr(), "POST", repositoriesPath+"/raw/group", []byte(repositoryBody("demo-g", "raw", "group", "demo-raw")), "basic",
		"demo-raw-keeper:pw", "Content-Type", jsonType); r.status != 201 {
		t.Fatalf("a group of a creator that reads every raw repository: %d %q", r.status, r.body)
	}
}

// Ruling ST13: the limits and the placement of a repository are set on the
// host. A body with another value is refused at the key; a save keeps the
// stored values whatever the body leaves out.
func TestServerOwnedAttributesComeFromTheHost(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	sec := w.opts.security.(*fakeSecurity)
	with := func(body string, key string, value any) string {
		var m map[string]any
		_ = json.Unmarshal([]byte(body), &m)
		m[key] = value
		data, _ := json.Marshal(m)
		return string(data)
	}
	plain := repositoryBody("demo-h", "raw", "hosted")
	for _, c := range []struct {
		key   string
		value any
	}{
		{"max_body_bytes", 1}, {"free_space_floor_bytes", 0}, {"write_store", "store-a"}, {"remote_public_only", false},
	} {
		r := do(t, w.addr(), "POST", repositoriesPath+"/raw/hosted", []byte(with(plain, c.key, c.value)), "basic", asAdmin, "Content-Type", jsonType)
		if r.status != 400 || !strings.Contains(string(r.body), `"id":"`+c.key+`"`) {
			t.Fatalf("%s %v: %d %q", c.key, c.value, r.status, r.body)
		}
	}
	if r := do(t, w.addr(), "POST", repositoriesPath+"/raw/hosted", []byte(plain), "basic", asAdmin, "Content-Type", jsonType); r.status != 201 {
		t.Fatalf("create: %d %q", r.status, r.body)
	}
	// the host sets a limit in the record; a save neither changes nor drops it
	err := sec.Update(nil, func(s *model.Snapshot) error {
		for i := range s.Repositories {
			if s.Repositories[i].Name == "demo-h" {
				s.Repositories[i].Attributes["max_body_bytes"] = json.Number("64")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := do(t, w.addr(), "PUT", repositoriesPath+"/raw/hosted/demo-h", []byte(with(plain, "max_body_bytes", 65)), "basic", asAdmin, "Content-Type", jsonType); r.status != 400 {
		t.Fatalf("save with another limit: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "PUT", repositoriesPath+"/raw/hosted/demo-h", []byte(plain), "basic", asAdmin, "Content-Type", jsonType); r.status != 204 {
		t.Fatalf("save: %d %q", r.status, r.body)
	}
	for _, r := range sec.Snapshot().Repositories {
		if r.Name == "demo-h" && fmt.Sprint(r.Attributes["max_body_bytes"]) != "64" {
			t.Fatalf("the save dropped the limit of the host: %v", r.Attributes)
		}
	}
}

// Ruling ST14: names that differ only in letter case are the same name, for
// repositories and for blob stores, whatever the file system does.
func TestNamesWithoutCase(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	for _, c := range []struct{ target, body string }{
		{repositoriesPath + "/raw/hosted", repositoryBody("DEMO-RAW", "raw", "hosted")},
		{repositoriesPath + "/raw/hosted", repositoryBody("Demo-Releases", "raw", "hosted")},
		{blobStoresPath + "/file", `{"name":"Store-A"}`},
		{blobStoresPath + "/file", `{"name":"LOCAL-WRITE"}`},
	} {
		if r := do(t, w.addr(), "POST", c.target, []byte(c.body), "basic", asAdmin, "Content-Type", jsonType); r.status != 409 {
			t.Errorf("%s %s: %d %q", c.target, c.body, r.status, r.body)
		}
	}
	if r := do(t, w.addr(), "POST", repositoriesPath+"/raw/hosted", []byte(repositoryBody("demo-made", "raw", "hosted")), "basic", asAdmin,
		"Content-Type", jsonType); r.status != 201 {
		t.Fatalf("create: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "POST", repositoriesPath+"/raw/proxy", []byte(repositoryBody("Demo-Made", "raw", "proxy")), "basic", asAdmin,
		"Content-Type", jsonType); r.status != 409 {
		t.Fatalf("a proxy whose name differs from a created one in case only: %d %q", r.status, r.body)
	}
}

// Ruling ST17: a repository whose rows lie in a store of the old product
// is kept with 409; the host removes it.
func TestADeleteKeepsARepositoryWithOldRows(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")}, spec("demo-old", "docs/old.txt", []byte("old"), "text/plain"))
	sec := w.opts.security.(*fakeSecurity)
	if err := sec.Update(nil, func(s *model.Snapshot) error {
		s.Repositories = append(slices.Clone(s.Repositories), model.Repository{Name: "demo-old", Format: "raw", Type: "hosted", Online: true,
			Attributes: storage("ALLOW")})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := do(t, w.addr(), "DELETE", repositoriesPath+"/demo-old", nil, "basic", asAdmin)
	if r.status != 409 || !strings.Contains(string(r.body), "store-a") {
		t.Fatalf("%d %q", r.status, r.body)
	}
	if got := do(t, w.addr(), "GET", "/repository/demo-old/docs/old.txt", nil, "basic", asAdmin); got.status != 200 {
		t.Fatalf("the content after the refusal: %d", got.status)
	}
}

// Ruling ST18: a save of a repository that repositories.json defines is
// kept like its delete; a created one takes the save, and its name stays.
func TestASaveKeepsADefinedRepository(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	if r := do(t, w.addr(), "PUT", repositoriesPath+"/raw/hosted/demo-raw", []byte(repositoryBody("demo-raw", "raw", "hosted")), "basic", asAdmin,
		"Content-Type", jsonType); r.status != 409 || !strings.Contains(string(r.body), "repositories.json") {
		t.Fatalf("save of a defined repository: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "POST", repositoriesPath+"/raw/hosted", []byte(repositoryBody("demo-made", "raw", "hosted")), "basic", asAdmin,
		"Content-Type", jsonType); r.status != 201 {
		t.Fatalf("create: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "PUT", repositoriesPath+"/raw/hosted/demo-made", []byte(repositoryBody("demo-other", "raw", "hosted")), "basic", asAdmin,
		"Content-Type", jsonType); r.status != 400 || !strings.Contains(string(r.body), `"name"`) {
		t.Fatalf("a save with another name: %d %q", r.status, r.body)
	}
}

// Finding 15: rebuild-index of a proxy, a group and a hosted repository of
// another format than maven2 answers 204 and changes nothing: neither the
// index nor the write store.
func TestRebuildIndexOfAnotherRepositoryChangesNothing(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	for _, c := range [][3]string{{"demo-p", "raw", "proxy"}, {"demo-g", "raw", "group"}} {
		if r := do(t, w.addr(), "POST", repositoriesPath+"/"+c[1]+"/"+c[2], []byte(repositoryBody(c[0], c[1], c[2], "demo-raw")), "basic", asAdmin,
			"Content-Type", jsonType); r.status != 201 {
			t.Fatalf("create %s: %d %q", c[0], r.status, r.body)
		}
	}
	if r := do(t, w.addr(), "PUT", "/repository/demo-raw/docs/a.txt", []byte("a"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("PUT %d", r.status)
	}
	before, err := w.srv.rp.index.Generation()
	if err != nil {
		t.Fatal(err)
	}
	files := func() []string {
		var out []string
		_ = filepath.WalkDir(writeStoreRoot(w.srv.rp.cfg.DataDir), func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				out = append(out, path)
			}
			return nil
		})
		return out
	}
	held := files()
	for _, name := range []string{"demo-p", "demo-g", "demo-raw"} {
		if r := do(t, w.addr(), "POST", repositoriesPath+"/"+name+"/rebuild-index", nil, "basic", asAdmin); r.status != 204 {
			t.Fatalf("rebuild %s: %d %q", name, r.status, r.body)
		}
	}
	if after, _ := w.srv.rp.index.Generation(); after != before || !slices.Equal(files(), held) {
		t.Fatalf("the rebuild changed the index (%d to %d) or the write store", before, after)
	}
}

// Finding 15: the create takes the segments of the save and the read; the
// format name maven2 is no segment.
func TestTheCreateTakesTheSegmentsOfTheSave(t *testing.T) {
	w := startIngest(t, nil)
	if r := do(t, w.addr(), "POST", repositoriesPath+"/maven2/hosted", []byte(repositoryBody("demo-m", "maven2", "hosted")), "basic", asAdmin,
		"Content-Type", jsonType); r.status != 501 {
		t.Fatalf("the segment maven2: %d %q", r.status, r.body)
	}
}
