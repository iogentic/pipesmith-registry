package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/auth"
	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/model"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/rest"
	"github.com/mirosso25/pipesmith-registry/internal/testkit"
	"github.com/mirosso25/pipesmith-registry/internal/ui"
)

const usersPath = "/service/rest/v1/security/users"

// fakeSecurity is model.Security with invented subjects: Basic pairs, two
// cookie sessions with their request tokens, a throttled name. It resolves
// a caller in the order of auth.Service, which TestFakeResolvesLikeTheService
// holds it to.
type fakeSecurity struct {
	mu        sync.Mutex
	snap      *model.Snapshot
	anonymous bool
	// anonymousPermissions are the rights of the anonymous user while
	// anonymous access is on
	anonymousPermissions []string
	basic                map[string]*model.Subject // "user:password"
	sessions             map[string]*model.Subject // cookie value
	calls                []string                  // methods the session handler saw
}

func subject(id string, session string, permissions ...string) *model.Subject {
	return &model.Subject{User: model.User{ID: id, Source: "default", Status: "active"}, Realm: "NexusAuthenticatingRealm",
		Permissions: permissions, Session: session}
}

func newFakeSecurity() *fakeSecurity {
	admin := []string{"nexus:*"}
	reader := []string{"nexus:repository-view:maven2:*:browse,read", "nexus:search:read"}
	return &fakeSecurity{
		snap: secureSnapshot(),
		basic: map[string]*model.Subject{
			"demo-admin:admin-pw":   subject("demo-admin", "", admin...),
			"demo-reader:reader-pw": subject("demo-reader", "", reader...),
			"demo-nobody:nobody-pw": subject("demo-nobody", "", "nexus:none:read"),
		},
		sessions: map[string]*model.Subject{
			"s1": subject("demo-admin", "s1", admin...),
			"s2": subject("demo-reader", "s2", reader...),
		},
	}
}

// Subject: a live session cookie, else Basic credentials; a cookie of no
// live session without an Authorization header next to it is refused, as
// auth.Service refuses it.
func (f *fakeSecurity) Subject(r *http.Request) (*model.Subject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, cookieErr := r.Cookie("fake-session")
	if cookieErr == nil {
		if s := f.sessions[c.Value]; s != nil {
			return s, nil
		}
	}
	if r.Header.Get("Authorization") == "" {
		if cookieErr == nil {
			return nil, model.ErrUnauthorized
		}
		return nil, nil
	}
	user, password, basic := r.BasicAuth()
	if !basic {
		return nil, model.ErrUnauthorized
	}
	if user == "demo-throttled" {
		return nil, &model.Error{Status: http.StatusTooManyRequests, Message: model.ErrThrottled.Message, RetryAfter: 7}
	}
	if s := f.basic[user+":"+password]; s != nil {
		return s, nil
	}
	return nil, model.ErrUnauthorized
}

func (f *fakeSecurity) Permits(s *model.Subject, permission string) bool {
	if s == nil {
		return f.anonymous && wildcard(f.anonymousPermissions, permission)
	}
	return wildcard(s.Permissions, permission)
}

func (f *fakeSecurity) AnonymousEnabled() bool { return f.anonymous }

func (f *fakeSecurity) CSRF(r *http.Request, s *model.Subject) bool {
	return s.Session == "" || r.Header.Get("X-Pipesmith-Csrf") == "csrf-"+s.Session
}

func (f *fakeSecurity) SessionHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(299)
		io.WriteString(w, `{"fake":"session"}`)
	})
}

func (f *fakeSecurity) Snapshot() *model.Snapshot { return f.snap }

func (f *fakeSecurity) addBasic(pair string, s *model.Subject) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.basic[pair] = s
}

func (f *fakeSecurity) sessionCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// wildcard is the rule of the contract, enough for the tests.
func wildcard(granted []string, permission string) bool {
	want := strings.Split(permission, ":")
	for _, g := range granted {
		ok := true
		for i, part := range strings.Split(g, ":") {
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

func secureSnapshot() *model.Snapshot {
	storage := func(store string) map[string]any {
		return map[string]any{"storage": map[string]any{"blobStoreName": store, "strictContentTypeValidation": true}}
	}
	return &model.Snapshot{
		Users: []model.User{{ID: "demo-admin", FirstName: "Demo", LastName: "Admin", Email: "demo-admin@example.com", Status: "active",
			Source: "default", Roles: []string{"demo-administrators"}, PasswordHash: "$shiro1$SHA-512$1024$c2FsdA==$aGFzaA=="}},
		Roles:      []model.Role{{ID: "demo-administrators", Source: "default", Name: "Administrators", Privileges: []string{"demo-all"}}},
		Privileges: []model.Privilege{{Name: "demo-all", Type: "wildcard", Properties: map[string]string{"pattern": "nexus:*"}}},
		LDAP:       []model.LDAPServer{{ID: "conn-1", Name: "corp", Order: 1, Protocol: "ldap", Host: "ldap.example.com", Port: 389}},
		Realms:     []string{"NexusAuthenticatingRealm"},
		Anonymous:  model.Anonymous{Enabled: false, UserID: "anonymous", RealmName: "NexusAuthenticatingRealm"},
		BlobStores: []model.BlobStore{{Name: "store-a", Type: "File", Path: "/srv/old-host/store-a"}},
		Repositories: []model.Repository{
			{Name: "demo-docker", Format: "docker", Type: "hosted", Online: true, Attributes: storage("store-a")},
			{Name: "demo-group", Format: "maven2", Type: "group", Online: true, Attributes: storage("store-a")},
			{Name: "demo-maven", Format: "maven2", Type: "hosted", Online: true, Attributes: storage("store-a")},
			{Name: "demo-npm", Format: "npm", Type: "hosted", Online: true, Attributes: storage("store-a")},
			{Name: "demo-raw", Format: "raw", Type: "hosted", Online: true, Attributes: storage("store-a")},
		},
	}
}

// secureApp is a server with security on over invented stores.
type secureApp struct {
	*app
	sec *fakeSecurity
}

func newSecureApp(t *testing.T, sec *fakeSecurity) *secureApp {
	t.Helper()
	var security model.Security
	if sec != nil {
		security = sec
	}
	return &secureApp{app: startSecured(t, security), sec: sec}
}

// startSecured starts the server over the invented stores with any
// Security, nil for security off.
func startSecured(t *testing.T, security model.Security) *app {
	t.Helper()
	blobs := []testkit.BlobSpec{
		spec("demo-maven", jarPath, []byte("jar bytes"), "application/java-archive"),
		spec("demo-npm", "widget/-/widget-1.2.3.tgz", []byte("tarball"), "application/x-tgz"),
		spec("demo-raw", "docs/readme.txt", []byte("readme"), "text/plain"),
	}
	blobs = append(blobs, blobsOf("demo-docker", dockerImage("app", "1.0", []byte("layer")))...)
	base := t.TempDir()
	stores := []config.StoreConfig{blobStore(t, base, "store-a", blobs...)}
	opts := appOptions{defs: []*pyjson.Object{def("demo-maven", "maven2", "hosted"), def("demo-npm", "npm", "hosted"),
		def("demo-raw", "raw", "hosted"), def("demo-group", "maven2", "group", "demo-maven"), dockerDef("demo-docker", 14690)},
		dockerRepos: []string{"demo-docker"}, security: security}
	return startApp(t, base, prepare(t, base, stores, opts), opts)
}

type answer struct {
	status int
	header http.Header
	body   []byte
}

func (a *answer) json(t *testing.T) any {
	t.Helper()
	var v any
	dec := json.NewDecoder(bytes.NewReader(a.body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%v: %d %q", err, a.status, a.body)
	}
	return v
}

func (a *answer) object(t *testing.T) map[string]any {
	t.Helper()
	m, ok := a.json(t).(map[string]any)
	if !ok {
		t.Fatalf("not an object: %d %q", a.status, a.body)
	}
	return m
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// do sends one request; headers are pairs, "basic" takes "user:password".
func do(t *testing.T, addr, method, target string, body []byte, headers ...string) *answer {
	t.Helper()
	r, err := http.NewRequest(method, "http://"+addr+target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			r.Host = headers[i+1]
			continue
		}
		if headers[i] == "basic" {
			user, password, _ := strings.Cut(headers[i+1], ":")
			r.SetBasicAuth(user, password)
			continue
		}
		r.Header.Set(headers[i], headers[i+1])
	}
	resp, err := noRedirect.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return &answer{status: resp.StatusCode, header: resp.Header, body: data}
}

func (a *secureApp) send(method, target string, headers ...string) *answer {
	a.t.Helper()
	return do(a.t, a.addr(), method, target, nil, headers...)
}

const (
	asAdmin  = "demo-admin:admin-pw"
	asReader = "demo-reader:reader-pw"
	asNobody = "demo-nobody:nobody-pw"
)

func status(t *testing.T, a *answer, want int) {
	t.Helper()
	if a.status != want {
		t.Fatalf("status %d, want %d: %v %q", a.status, want, a.header, a.body)
	}
}

// Security off

func TestSecurityOffServesStepTwo(t *testing.T) {
	a := newSecureApp(t, nil)
	page := a.send("GET", "/")
	status(t, page, 200)
	if !bytes.Equal(page.body, pageHTML) {
		t.Fatal("/ is not the page of step 2")
	}
	for _, target := range []string{"/css/base.css", "/js/app.js", "/api/session", "/service/rest/v1/security/roles",
		"/service/rest/v1/search", "/api/tokens"} {
		r := a.send("GET", target)
		if r.status != 404 || string(r.body) != `{"detail":"Not Found"}` {
			t.Errorf("%s: %d %q", target, r.status, r.body)
		}
	}
	asset := a.send("GET", "/api/asset/demo-maven?path="+jarPath).object(t)
	if _, ok := asset["asset_id"]; ok {
		t.Fatal("asset_id without security")
	}
	status(t, a.send("GET", "/repository/demo-maven/"+jarPath), 200)
	repos := a.send("GET", "/api/repositories").object(t)["repositories"].([]any)
	if len(repos) != 5 {
		t.Fatalf("%d repositories", len(repos))
	}
}

// Interface files

func TestInterfaceAtRoot(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	page := a.send("GET", "/")
	status(t, page, 200)
	if !bytes.Contains(page.body, []byte(`<script type="module" src="js/app.js"></script>`)) {
		t.Fatalf("/ is not the interface: %q", page.body[:min(200, len(page.body))])
	}
	if page.header.Get("Content-Security-Policy") != ui.Policy || page.header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("%v", page.header)
	}
	for _, target := range []string{"/js/app.js", "/css/tokens.css", "/assets/favicon-32.png", "/js/pages/security/users.js"} {
		r := a.send("GET", target)
		if r.status != 200 || r.header.Get("X-Content-Type-Options") != "nosniff" || r.header.Get("Referrer-Policy") != "same-origin" {
			t.Errorf("%s: %d %v", target, r.status, r.header)
		}
	}
	status(t, a.send("HEAD", "/"), 200)
	status(t, a.send("GET", "/index.html"), 404)
	status(t, a.send("GET", "/js/missing.js"), 404)
	status(t, a.send("POST", "/js/app.js"), 405)
	if strings.Contains(a.access.String(), "GET /js/app.js HTTP/1.1\" 200") == false {
		t.Fatalf("no access line for a file: %s", a.access.String())
	}
}

// Public routes

func TestPublicRoutesWithoutSession(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	status(t, a.send("GET", "/api/health"), 200)
	status(t, a.send("GET", "/service/rest/v1/status"), 200)
	status(t, a.send("GET", "/service/rest/v1/status/writable"), 503)
	short := a.send("GET", "/api/status")
	status(t, short, 200)
	if got := short.object(t); len(got) != 2 || got["name"] != "Pipesmith Registry" || got["version"] == nil {
		t.Fatalf("%#v", got)
	}
	if got := a.send("GET", "/api/status", "basic", "demo-admin:wrong").object(t); len(got) != 2 {
		t.Fatalf("wrong credentials: %#v", got)
	}
	full := a.send("GET", "/api/status", "basic", asNobody).object(t)
	for _, key := range []string{"name", "version", "stores", "totals", "scan", "data_dir"} {
		if _, ok := full[key]; !ok {
			t.Errorf("no %s in %v", key, full)
		}
	}
}

func TestSessionRoutesGoToSecurity(t *testing.T) {
	sec := newFakeSecurity()
	a := newSecureApp(t, sec)
	for _, method := range []string{"GET", "POST", "DELETE"} {
		r := a.send(method, "/api/session")
		if r.status != 299 || string(r.body) != `{"fake":"session"}` {
			t.Fatalf("%s: %d %q", method, r.status, r.body)
		}
	}
	if got := sec.sessionCalls(); !reflect.DeepEqual(got, []string{"GET", "POST", "DELETE"}) {
		t.Fatalf("%v", got)
	}
}

// readRoutes are the reads of the screens with the one permission each
// needs; "" is a route for any subject that filters what it shows.
var readRoutes = []struct {
	target, permission string
	nobody             int // the status of a subject without rights
}{
	{"/service/rest/v1/repositories", "", 200},
	{"/service/rest/v1/repositories/demo-maven", "nexus:repository-view:maven2:demo-maven:browse", 403},
	{"/service/rest/v1/repositories/maven/hosted/demo-maven", "nexus:repository-admin:maven2:demo-maven:read", 403},
	{"/service/rest/v1/repositorySettings", "nexus:repository-admin:maven2:demo-maven:read", 403},
	{"/service/rest/v1/search?name=demo-lib", "nexus:search:read", 403},
	{"/service/rest/v1/assets/" + rest.AssetID("demo-maven", jarPath), "nexus:repository-view:maven2:demo-maven:browse", 403},
	{"/service/rest/v1/blobstores", "nexus:blobstores:read", 403},
	{"/service/rest/v1/blobstores/file/store-a", "nexus:blobstores:read", 403},
	{"/service/rest/v1/blobstores/store-a/quota-status", "nexus:blobstores:read", 403},
	{"/service/rest/v1/security/privileges", "nexus:privileges:read", 403},
	{"/service/rest/v1/security/privileges/demo-all", "nexus:privileges:read", 403},
	{"/service/rest/v1/security/roles", "nexus:roles:read", 403},
	{"/service/rest/v1/security/roles/demo-administrators", "nexus:roles:read", 403},
	{"/service/rest/v1/security/user-sources", "nexus:users:read", 403},
	{"/service/rest/v1/security/user-sources", "nexus:roles:read", 403},
	{usersPath, "nexus:users:read", 403},
	{usersPath + "?source=default&userId=demo", "nexus:users:read", 403},
	{"/service/rest/v1/security/anonymous", "nexus:settings:read", 403},
	{"/service/rest/v1/security/realms/available", "nexus:settings:read", 403},
	{"/service/rest/v1/security/realms/active", "nexus:settings:read", 403},
	{"/service/rest/v1/security/ldap", "nexus:ldap:read", 403},
	{"/service/rest/v1/security/ldap/corp", "nexus:ldap:read", 403},
	{"/service/rest/v1/status/check", "nexus:atlas:read", 403},
	{"/service/rest/v1/read-only", "nexus:settings:read", 403},
	{"/api/repositories", "", 200},
	{"/api/browse/demo-maven", "nexus:repository-view:maven2:demo-maven:browse", 403},
	{"/api/asset/demo-maven?path=" + jarPath, "nexus:repository-view:maven2:demo-maven:browse", 403},
}

func TestInterfaceRoutesNeedASubject(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	for _, route := range readRoutes {
		r := a.send("GET", route.target)
		if r.status != 401 || r.object(t)["message"] != "authentication required" {
			t.Errorf("%s: %d %q", route.target, r.status, r.body)
			continue
		}
		challenge := r.header.Get("WWW-Authenticate")
		if strings.HasPrefix(route.target, "/api/") {
			if strings.HasPrefix(challenge, "Basic") || challenge == "" {
				t.Errorf("%s: challenge %q opens the dialog of a browser", route.target, challenge)
			}
		} else if challenge != `Basic realm="Pipesmith Registry"` {
			t.Errorf("%s: challenge %q", route.target, challenge)
		}
		// a request with a cookie, even an unknown one, never gets Basic
		r = a.send("GET", route.target, "Cookie", "fake-session=expired")
		if r.status != 401 || strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Basic") {
			t.Errorf("%s with a cookie: %d %v", route.target, r.status, r.header)
		}
	}
}

func TestCredentialsThatFail(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	r := a.send("GET", "/service/rest/v1/security/roles", "basic", "demo-admin:wrong")
	status(t, r, 401)
	r = a.send("GET", "/service/rest/v1/security/roles", "basic", "demo-throttled:any")
	status(t, r, 429)
	if r.header.Get("Retry-After") != "7" || len(r.body) != 0 {
		t.Fatalf("%v %q", r.header, r.body)
	}
}

func TestEachReadNamesItsPermission(t *testing.T) {
	sec := newFakeSecurity()
	a := newSecureApp(t, sec)
	for i, route := range readRoutes {
		r := a.send("GET", route.target, "basic", asNobody)
		if r.status != route.nobody {
			t.Errorf("%s without rights: %d %q", route.target, r.status, r.body)
		}
		if r.status == 403 && r.object(t)["message"] == "" {
			t.Errorf("%s: 403 without a message", route.target)
		}
		if route.permission == "" {
			continue
		}
		user := "holder-" + strconv.Itoa(i)
		sec.addBasic(user+":pw", subject(user, "", route.permission))
		r = a.send("GET", route.target, "basic", user+":pw")
		if r.status != 200 {
			t.Errorf("%s with %s: %d %q", route.target, route.permission, r.status, r.body)
		}
		if r.header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: Content-Type %q", route.target, r.header.Get("Content-Type"))
		}
	}
}

func TestRepositoryRules(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	status(t, a.send("GET", "/api/browse/demo-maven", "basic", asReader), 200)
	status(t, a.send("GET", "/api/browse/demo-npm", "basic", asReader), 403)
	status(t, a.send("GET", "/api/asset/demo-npm?path=widget/-/widget-1.2.3.tgz", "basic", asReader), 403)
	// a name that does not exist: only a wildcard grant learns that it is free
	status(t, a.send("GET", "/api/browse/missing", "basic", asReader), 403)
	status(t, a.send("GET", "/api/browse/missing", "basic", asAdmin), 404)
	status(t, a.send("GET", "/service/rest/v1/repositories/missing", "basic", asReader), 403)
	status(t, a.send("GET", "/service/rest/v1/repositories/missing", "basic", asAdmin), 404)
	status(t, a.send("GET", "/service/rest/v1/repositories/maven/hosted/demo-maven", "basic", asReader), 403)
	status(t, a.send("GET", "/service/rest/v1/repositories/npm/hosted/demo-maven", "basic", asAdmin), 404)
	// a group is checked by its own permission
	status(t, a.send("GET", "/api/browse/demo-group", "basic", asReader), 200)
}

func TestListsShowWhatTheSubjectMayBrowse(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	rows := a.send("GET", "/api/repositories", "basic", asReader).object(t)["repositories"].([]any)
	var got []string
	for _, row := range rows {
		got = append(got, row.(map[string]any)["name"].(string))
	}
	if !reflect.DeepEqual(got, []string{"demo-group", "demo-maven"}) {
		t.Fatalf("%v", got)
	}
	list := a.send("GET", "/service/rest/v1/repositories", "basic", asReader).json(t).([]any)
	got = nil
	for _, item := range list {
		m := item.(map[string]any)
		got = append(got, m["name"].(string))
		if m["url"] != "http://"+a.addr()+"/repository/"+m["name"].(string) {
			t.Errorf("url %v", m["url"])
		}
	}
	if !reflect.DeepEqual(got, []string{"demo-group", "demo-maven"}) {
		t.Fatalf("%v", got)
	}
	all := a.send("GET", "/api/repositories", "basic", asAdmin).object(t)["repositories"].([]any)
	if len(all) != 5 {
		t.Fatalf("%d", len(all))
	}
}

func TestAssetIDLeadsToTheAsset(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	detail := a.send("GET", "/api/asset/demo-maven?path="+jarPath, "basic", asReader).object(t)
	id, _ := detail["asset_id"].(string)
	if id != rest.AssetID("demo-maven", jarPath) {
		t.Fatalf("asset_id %v", detail["asset_id"])
	}
	asset := a.send("GET", "/service/rest/v1/assets/"+url.PathEscape(id), "basic", asReader).object(t)
	if asset["path"] != jarPath || asset["repository"] != "demo-maven" || asset["blobStoreName"] != "store-a" {
		t.Fatalf("%#v", asset)
	}
	status(t, a.send("GET", "/service/rest/v1/assets/"+rest.AssetID("demo-npm", "widget/-/widget-1.2.3.tgz"), "basic", asReader), 403)
	status(t, a.send("GET", "/service/rest/v1/assets/not-an-id!", "basic", asAdmin), 422)
}

func TestReadsThroughTheServer(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	users := a.send("GET", usersPath, "basic", asAdmin)
	status(t, users, 200)
	if bytes.Contains(users.body, []byte("shiro")) || bytes.Contains(users.body, []byte("hash")) {
		t.Fatalf("%q", users.body)
	}
	if users.header.Get("Cache-Control") != "no-store" || users.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%v", users.header)
	}
	page := a.send("GET", "/service/rest/v1/search?name=demo-lib", "basic", asReader).object(t)
	items := page["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["repository"] != "demo-maven" {
		t.Fatalf("%#v", page)
	}
	ro := a.send("GET", "/service/rest/v1/read-only", "basic", asAdmin).object(t)
	if ro["frozen"] != true {
		t.Fatalf("%#v", ro)
	}
	ldap := a.send("GET", "/service/rest/v1/security/roles?source=LDAP", "basic", asAdmin)
	status(t, ldap, 501)
	if ldap.object(t)["area"] != "security" {
		t.Fatalf("%q", ldap.body)
	}
	status(t, a.send("GET", "/service/rest/v1/search?colour=blue", "basic", asAdmin), 400)
	status(t, a.send("GET", "/service/rest/v1/search?q=%zz", "basic", asAdmin), 400)
	status(t, a.send("GET", "/service/rest/v1/security/roles/missing", "basic", asAdmin), 404)
	status(t, a.send("HEAD", "/service/rest/v1/security/roles", "basic", asAdmin), 405)
}

// refusals are the requests of the contract this server does not serve
// yet, each with its area.
var refusals = []struct{ method, target, area string }{
	{"DELETE", "/service/rest/v1/assets/" + rest.AssetID("demo-maven", jarPath), "content"},
	{"GET", "/service/rest/v1/formats/maven2/upload-specs", "content"},
	{"POST", "/service/rest/v1/components?repository=demo-maven", "content"},
	{"POST", "/service/rest/v1/repositories/maven/hosted", "repositories"},
	{"PUT", "/service/rest/v1/repositories/maven/hosted/demo-maven", "repositories"},
	{"DELETE", "/service/rest/v1/repositories/demo-maven", "repositories"},
	{"POST", "/service/rest/v1/repositories/demo-maven/rebuild-index", "repositories"},
	{"POST", "/service/rest/v1/repositories/demo-maven/invalidate-cache", "repositories"},
	{"GET", "/api/cleanup-policies", "cleanup-policies"},
	{"GET", "/api/cleanup-policies/weekly", "cleanup-policies"},
	{"POST", "/api/cleanup-policies", "cleanup-policies"},
	{"PUT", "/api/cleanup-policies/weekly", "cleanup-policies"},
	{"DELETE", "/api/cleanup-policies/weekly", "cleanup-policies"},
	{"POST", "/api/cleanup-policies/preview", "cleanup-policies"},
	{"GET", "/service/rest/v1/routing-rules", "routing-rules"},
	{"GET", "/service/rest/v1/routing-rules/block-all", "routing-rules"},
	{"POST", "/service/rest/v1/routing-rules", "routing-rules"},
	{"PUT", "/service/rest/v1/routing-rules/block-all", "routing-rules"},
	{"DELETE", "/service/rest/v1/routing-rules/block-all", "routing-rules"},
	{"POST", "/api/routing-rules/test", "routing-rules"},
	{"POST", "/api/routing-rules/preview", "routing-rules"},
	{"POST", "/service/rest/v1/blobstores/file", "blob-stores"},
	{"PUT", "/service/rest/v1/blobstores/file/store-a", "blob-stores"},
	{"DELETE", "/service/rest/v1/blobstores/store-a", "blob-stores"},
	{"GET", "/api/blob-stores/store-a/deleted", "blob-stores"},
	{"POST", "/service/rest/v1/security/roles", "security"},
	{"PUT", "/service/rest/v1/security/roles/demo-administrators", "security"},
	{"DELETE", "/service/rest/v1/security/roles/demo-administrators", "security"},
	{"POST", usersPath, "security"},
	{"PUT", usersPath + "/demo-admin", "security"},
	{"DELETE", usersPath + "/demo-admin?realm=NexusAuthenticatingRealm", "security"},
	{"PUT", usersPath + "/demo-admin/change-password", "security"},
	{"PUT", "/service/rest/v1/security/anonymous", "security"},
	{"PUT", "/service/rest/v1/security/realms/active", "security"},
	{"POST", "/service/rest/v1/security/ldap", "ldap"},
	{"PUT", "/service/rest/v1/security/ldap/corp", "ldap"},
	{"DELETE", "/service/rest/v1/security/ldap/corp", "ldap"},
	{"POST", "/service/rest/v1/security/ldap/change-order", "ldap"},
	{"POST", "/api/ldap/clear-cache", "ldap"},
	{"POST", "/api/ldap/verify-connection", "ldap"},
	{"POST", "/api/ldap/verify-user-mapping", "ldap"},
	{"POST", "/api/ldap/verify-login", "ldap"},
	{"GET", "/api/tokens", "tokens"},
	{"GET", "/api/tokens?userId=demo-admin", "tokens"},
	{"POST", "/api/tokens", "tokens"},
	{"DELETE", "/api/tokens/t-1", "tokens"},
	{"PUT", "/api/account", "account"},
	{"POST", "/api/account/password", "account"},
	{"GET", "/api/logging/loggers", "logging"},
	{"PUT", "/api/logging/loggers/pipesmith_registry.server", "logging"},
	{"DELETE", "/api/logging/loggers/pipesmith_registry.server", "logging"},
	{"POST", "/api/logging/reset", "logging"},
	{"GET", "/api/logs", "logs"},
	{"GET", "/api/logs/tasks/compact-1.log?bytes=25600", "logs"},
	{"GET", "/service/rest/beta/system/information", "system"},
	{"GET", "/service/rest/v1/capabilities", "capabilities"},
	{"PUT", "/service/rest/v1/capabilities/c-1", "capabilities"},
	{"DELETE", "/service/rest/v1/capabilities/c-1", "capabilities"},
	{"GET", "/api/capabilities/support", "capabilities"},
	{"GET", "/service/rest/v1/system/node", "system"},
	{"POST", "/service/rest/v1/read-only/freeze", "system"},
	{"POST", "/service/rest/v1/read-only/release", "system"},
	{"POST", "/service/rest/v1/read-only/force-release", "system"},
	{"GET", "/service/rest/v1/tasks", "tasks"},
	{"GET", "/service/rest/v1/tasks/t-1", "tasks"},
	{"POST", "/service/rest/v1/tasks/t-1/run", "tasks"},
	{"POST", "/service/rest/v1/tasks/t-1/stop", "tasks"},
	{"GET", "/api/tasks/t-1", "tasks"},
	{"POST", "/api/tasks", "tasks"},
	{"PUT", "/api/tasks/t-1", "tasks"},
	{"DELETE", "/api/tasks/t-1", "tasks"},
	{"GET", "/api/task-types", "tasks"},
	{"GET", "/api/metrics/requests?from=2026-11-01&to=2026-11-12", "metrics"},
	{"GET", "/api/metrics/downloads?from=2026-11-01&to=2026-11-12", "metrics"},
}

func TestContractRefusals(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	for _, c := range refusals {
		r := a.send(c.method, c.target)
		if r.status != 401 {
			t.Errorf("%s %s without a subject: %d %q", c.method, c.target, r.status, r.body)
		}
		r = a.send(c.method, c.target, "basic", asAdmin)
		if r.status != 501 {
			t.Errorf("%s %s: %d %q", c.method, c.target, r.status, r.body)
			continue
		}
		m := r.object(t)
		if m["area"] != c.area || m["message"] == "" || len(m) != 2 {
			t.Errorf("%s %s: %#v, want area %s", c.method, c.target, m, c.area)
		}
		if c.method == "GET" {
			continue
		}
		r = a.send(c.method, c.target, "Cookie", "fake-session=s1")
		if r.status != 403 || r.object(t)["message"] != "missing or wrong request token" {
			t.Errorf("%s %s without the request token: %d %q", c.method, c.target, r.status, r.body)
		}
		r = a.send(c.method, c.target, "Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1")
		if r.status != 501 {
			t.Errorf("%s %s with the request token: %d %q", c.method, c.target, r.status, r.body)
		}
	}
	// docs/ui/API.md at 1e6d884 names 104 distinct requests (method and path,
	// testdata/ui-contract-requests.txt); 31 are served, GET /api/areas
	// among them, the other 73 are this list, GET /api/tokens twice
	if len(refusals) != 74 {
		t.Fatalf("%d refusals", len(refusals))
	}
}

func TestScanNeedsEveryRight(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	body := []byte(`{"store": "store-a"}`)
	host := "localhost:" + a.addr()[strings.LastIndex(a.addr(), ":")+1:]
	scan := func(headers ...string) *answer {
		return do(t, a.addr(), "POST", "/api/scan", body, append([]string{"X-Artifact-Repository", "1", "Host", host}, headers...)...)
	}
	status(t, scan(), 401)
	status(t, scan("basic", asReader), 403)
	status(t, scan("Cookie", "fake-session=s1"), 403)
	r := scan("Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1")
	if r.status != 202 && r.status != 409 {
		t.Fatalf("%d %q", r.status, r.body)
	}
}

// The client read path: Basic when anonymous access is off, open when on.

func TestClientPathNeedsCredentialsWhenAnonymousIsOff(t *testing.T) {
	sec := newFakeSecurity()
	a := newSecureApp(t, sec)
	file := "/repository/demo-maven/" + jarPath
	for _, target := range []string{file, "/repository/demo-maven/com/example/", "/service/rest/repository/browse/demo-maven/"} {
		r := a.send("GET", target)
		if r.status != 401 || r.header.Get("WWW-Authenticate") != `Basic realm="Pipesmith Registry"` {
			t.Errorf("%s: %d %v", target, r.status, r.header)
		}
	}
	r := a.send("GET", file, "basic", asReader)
	if r.status != 200 || string(r.body) != "jar bytes" {
		t.Fatalf("%d %q", r.status, r.body)
	}
	status(t, a.send("GET", file, "basic", "demo-admin:wrong"), 401)
	status(t, a.send("GET", file, "basic", "demo-throttled:x"), 429)
	status(t, a.send("GET", file, "Cookie", "fake-session=s2"), 200)
	if c := a.send("GET", file, "Cookie", "fake-session=expired").header.Get("WWW-Authenticate"); strings.HasPrefix(c, "Basic") {
		t.Fatalf("a request with a cookie got %q", c)
	}
	status(t, a.send("PUT", file), 401)
	status(t, a.send("PUT", file, "basic", asReader), 405)
	// the Docker port
	docker := a.on("demo-docker", "GET", "/v2/")
	if docker.status != 401 || docker.header.Get("WWW-Authenticate") != `Basic realm="Pipesmith Registry"` ||
		!strings.Contains(string(docker.body), "UNAUTHORIZED") || docker.header.Get("Docker-Distribution-Api-Version") != "registry/2.0" {
		t.Fatalf("%s", docker)
	}
	docker = a.on("demo-docker", "GET", "/v2/", "Authorization", "Basic ZGVtby1yZWFkZXI6cmVhZGVyLXB3")
	status(t, &answer{status: docker.status}, 200)
	main := a.send("GET", "/repository/demo-docker/v2/")
	if main.status != 401 || !strings.Contains(string(main.body), "UNAUTHORIZED") {
		t.Fatalf("%d %q", main.status, main.body)
	}
}

// anonymousRights are the client rights of the role nx-anonymous of the old
// product (internal/authz/builtin.go).
var anonymousRights = []string{"nexus:repository-view:*:*:browse,read", "nexus:search:read"}

func TestClientPathIsOpenWhenAnonymousIsOn(t *testing.T) {
	sec := newFakeSecurity()
	sec.anonymous = true
	sec.anonymousPermissions = anonymousRights
	a := newSecureApp(t, sec)
	r := a.send("GET", "/repository/demo-maven/"+jarPath)
	if r.status != 200 || string(r.body) != "jar bytes" {
		t.Fatalf("%d %q", r.status, r.body)
	}
	status(t, a.send("GET", "/service/rest/repository/browse/demo-maven/"), 200)
	status(t, a.send("PUT", "/repository/demo-maven/x.jar"), 405)
	status(t, &answer{status: a.on("demo-docker", "GET", "/v2/").status}, 200)
	// anonymous access opens the client path, not the interface
	status(t, a.send("GET", "/api/repositories"), 401)
}

// The client read path needs the view permission of the repository: read
// below /repository/ and on the Docker ports, browse below the browse route
// (review 6, finding 3).

// denied is the one text of every refusal by a rule that depends on a
// repository (review 6, finding 7).
const denied = "requested access to the resource is denied"

func basicHeader(pair string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(pair))
}

func TestClientPathNeedsTheViewPermission(t *testing.T) {
	sec := newFakeSecurity()
	const (
		asBrowser = "demo-browser:browser-pw"
		asFetcher = "demo-fetcher:fetcher-pw"
		asPuller  = "demo-puller:puller-pw"
	)
	sec.addBasic(asBrowser, subject("demo-browser", "", "nexus:repository-view:maven2:demo-maven:browse"))
	sec.addBasic(asFetcher, subject("demo-fetcher", "", "nexus:repository-view:maven2:demo-maven:read"))
	sec.addBasic(asPuller, subject("demo-puller", "", "nexus:repository-view:docker:demo-docker:read"))
	a := newSecureApp(t, sec)
	file := "/repository/demo-maven/" + jarPath
	folder := "/repository/demo-maven/com/example/"
	browse := "/service/rest/repository/browse/demo-maven/"
	cases := []struct {
		who, method, target string
		want                int
	}{
		{asNobody, "GET", file, 403},
		{asNobody, "HEAD", file, 403},
		{asNobody, "GET", folder, 403},
		{asNobody, "GET", "/repository/demo-maven", 403},
		{asNobody, "GET", browse, 403},
		{asNobody, "PUT", file, 403},
		{asReader, "GET", file, 200},
		{asReader, "GET", folder, 200},
		{asReader, "GET", browse, 200},
		{asReader, "PUT", file, 405},
		{asReader, "GET", "/repository/demo-group/" + jarPath, 200},
		{asReader, "GET", "/repository/demo-npm/widget/-/widget-1.2.3.tgz", 403},
		{asReader, "GET", "/repository/demo-raw/docs/readme.txt", 403},
		{asReader, "GET", "/service/rest/repository/browse/demo-raw/", 403},
		{asFetcher, "GET", file, 200},
		{asFetcher, "GET", browse, 403},
		{asFetcher, "GET", "/repository/demo-group/" + jarPath, 403},
		{asBrowser, "GET", browse, 200},
		{asBrowser, "GET", file, 403},
		{asAdmin, "GET", file, 200},
		{asAdmin, "GET", "/repository/missing/x.jar", 404},
	}
	for _, c := range cases {
		r := a.send(c.method, c.target, "basic", c.who)
		if r.status != c.want {
			t.Errorf("%s %s as %s: %d %q, want %d", c.method, c.target, c.who, r.status, r.body, c.want)
			continue
		}
		if c.want == 403 && c.method != "HEAD" && string(r.body) != denied+"\n" {
			t.Errorf("%s %s as %s: %q", c.method, c.target, c.who, r.body)
		}
	}
	// the Docker base of a Docker port needs sign in only, every other path
	// the read permission of its repository
	manifest := "/v2/app/manifests/1.0"
	for _, c := range []struct {
		who, target string
		want        int
	}{
		{asNobody, "/v2/", 200},
		{asNobody, "/v2", 200},
		{asNobody, manifest, 403},
		{asReader, manifest, 403},
		{asPuller, manifest, 200},
		{asAdmin, manifest, 200},
	} {
		r := a.on("demo-docker", "GET", c.target, "Authorization", basicHeader(c.who))
		if r.status != c.want {
			t.Errorf("Docker port %s as %s: %s, want %d", c.target, c.who, r, c.want)
			continue
		}
		if c.want == 403 && (errorCode(t, r) != "DENIED" || r.header.Get("Docker-Distribution-Api-Version") != "registry/2.0" ||
			!strings.Contains(string(r.body), denied)) {
			t.Errorf("Docker port %s as %s: %s", c.target, c.who, r)
		}
	}
	main := a.send("GET", "/repository/demo-docker/v2/", "basic", asNobody)
	if main.status != 403 || !strings.Contains(string(main.body), `"DENIED"`) {
		t.Errorf("main port: %d %q", main.status, main.body)
	}
	status(t, a.send("GET", "/repository/demo-docker/v2/app/manifests/1.0", "basic", asPuller), 200)
}

// With anonymous access on, a caller without credentials holds the rights
// of the anonymous user; without the right it is asked for credentials.
func TestAnonymousHoldsTheRightsOfTheAnonymousUser(t *testing.T) {
	sec := newFakeSecurity()
	sec.anonymous = true
	sec.anonymousPermissions = []string{"nexus:repository-view:maven2:demo-maven:read"}
	a := newSecureApp(t, sec)
	r := a.send("GET", "/repository/demo-maven/"+jarPath)
	if r.status != 200 || string(r.body) != "jar bytes" {
		t.Fatalf("%d %q", r.status, r.body)
	}
	for _, target := range []string{"/repository/demo-raw/docs/readme.txt", "/repository/missing/docs/readme.txt",
		"/service/rest/repository/browse/demo-maven/"} {
		r := a.send("GET", target)
		if r.status != 401 || r.header.Get("WWW-Authenticate") != `Basic realm="Pipesmith Registry"` {
			t.Errorf("%s: %d %v %q", target, r.status, r.header, r.body)
		}
	}
	status(t, &answer{status: a.on("demo-docker", "GET", "/v2/").status}, 200)
	docker := a.on("demo-docker", "GET", "/v2/app/manifests/1.0")
	if docker.status != 401 || errorCode(t, docker) != "UNAUTHORIZED" {
		t.Fatalf("%s", docker)
	}
	// a subject is held to its own rights, not to those of the anonymous user
	status(t, a.send("GET", "/repository/demo-maven/"+jarPath, "basic", asNobody), 403)
}

// A rule that depends on a repository refuses a missing repository and a
// forbidden one with the same answer, which names neither the format nor
// the permission (review 6, finding 7).
func TestRepositoryRefusalsLookAlike(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	pairs := []struct{ forbidden, missing string }{
		{"/service/rest/v1/repositories/demo-npm", "/service/rest/v1/repositories/missing"},
		{"/service/rest/v1/repositories/npm/hosted/demo-npm", "/service/rest/v1/repositories/npm/hosted/missing"},
		{"/service/rest/v1/assets/" + rest.AssetID("demo-npm", "widget/-/widget-1.2.3.tgz"),
			"/service/rest/v1/assets/" + rest.AssetID("missing", "widget/-/widget-1.2.3.tgz")},
		{"/api/browse/demo-npm", "/api/browse/missing"},
		{"/api/asset/demo-npm?path=widget/-/widget-1.2.3.tgz", "/api/asset/missing?path=widget/-/widget-1.2.3.tgz"},
		{"/repository/demo-npm/widget/-/widget-1.2.3.tgz", "/repository/missing/widget/-/widget-1.2.3.tgz"},
		{"/service/rest/repository/browse/demo-npm/", "/service/rest/repository/browse/missing/"},
		{"/repository/demo-docker/v2/app/manifests/1.0", "/repository/missing/v2/app/manifests/1.0"},
	}
	for _, who := range []string{asNobody, asReader} {
		for _, p := range pairs {
			f := a.send("GET", p.forbidden, "basic", who)
			m := a.send("GET", p.missing, "basic", who)
			if f.status != 403 || m.status != 403 {
				t.Errorf("as %s: %s %d, %s %d", who, p.forbidden, f.status, p.missing, m.status)
				continue
			}
			if !bytes.Equal(f.body, m.body) || f.header.Get("Content-Type") != m.header.Get("Content-Type") {
				t.Errorf("as %s: %s %q, %s %q", who, p.forbidden, f.body, p.missing, m.body)
			}
			if !bytes.Contains(f.body, []byte(denied)) {
				t.Errorf("as %s: %s %q", who, p.forbidden, f.body)
			}
			for _, word := range []string{"nexus:", "npm", "docker", "maven", "missing", "permission"} {
				if bytes.Contains(f.body, []byte(word)) {
					t.Errorf("as %s: %s names %q: %q", who, p.forbidden, word, f.body)
				}
			}
		}
	}
	// the rule of a screen still names its permission: it tells nothing of a
	// repository
	if got := a.send("GET", "/service/rest/v1/blobstores", "basic", asNobody).object(t)["message"]; got !=
		"this needs the permission nexus:blobstores:read" {
		t.Errorf("%q", got)
	}
}

// With anonymous access on, a cookie that names no live session counts as
// no credentials on the client path when no Authorization header comes with
// it; the interface still learns that its session ended (review 6,
// finding 9). The fake and the real service answer alike.
func TestStaleCookieOnTheOpenClientPath(t *testing.T) {
	fake := newFakeSecurity()
	fake.anonymous = true
	fake.anonymousPermissions = anonymousRights
	for _, impl := range []struct {
		name   string
		sec    model.Security
		cookie string
	}{
		{"fake", fake, "fake-session=expired"},
		{"service", anonymousService(t), auth.CookieName + "=expired"},
	} {
		t.Run(impl.name, func(t *testing.T) {
			a := startSecured(t, impl.sec)
			r := do(t, a.addr(), "GET", "/repository/demo-maven/"+jarPath, nil, "Cookie", impl.cookie)
			if r.status != 200 || string(r.body) != "jar bytes" {
				t.Fatalf("file: %d %q", r.status, r.body)
			}
			status(t, do(t, a.addr(), "GET", "/service/rest/repository/browse/demo-maven/", nil, "Cookie", impl.cookie), 200)
			status(t, &answer{status: a.on("demo-docker", "GET", "/v2/app/manifests/1.0", "Cookie", impl.cookie).status}, 200)
			// credentials next to the cookie count
			wrong := do(t, a.addr(), "GET", "/repository/demo-maven/"+jarPath, nil, "Cookie", impl.cookie, "basic", "demo-admin:wrong")
			status(t, wrong, 401)
			bearer := do(t, a.addr(), "GET", "/repository/demo-maven/"+jarPath, nil, "Cookie", impl.cookie, "Authorization", "Bearer t-1")
			status(t, bearer, 401)
			// the interface still gets the 401 of an ended session
			ended := do(t, a.addr(), "GET", "/api/repositories", nil, "Cookie", impl.cookie)
			if ended.status != 401 || strings.HasPrefix(ended.header.Get("WWW-Authenticate"), "Basic") {
				t.Fatalf("interface: %d %v", ended.status, ended.header)
			}
		})
	}
}

// anonymousService is auth.Service over the invented snapshot with
// anonymous access on and the rights of nx-anonymous for the anonymous user.
func anonymousService(t *testing.T) *auth.Service {
	t.Helper()
	snap := secureSnapshot()
	snap.Anonymous.Enabled = true
	snap.Users = append(snap.Users, model.User{ID: "anonymous", Status: "active", Source: auth.SourceLocal, Roles: []string{"demo-anonymous"}})
	snap.Roles = append(snap.Roles, model.Role{ID: "demo-anonymous", Source: auth.SourceLocal, Name: "Anonymous",
		Privileges: []string{"demo-anonymous-read"}})
	snap.Privileges = append(snap.Privileges, model.Privilege{Name: "demo-anonymous-read", Type: "wildcard",
		Properties: map[string]string{"pattern": anonymousRights[0]}})
	sec, err := auth.New(auth.Options{Snapshot: snap})
	if err != nil {
		t.Fatal(err)
	}
	return sec
}

// The fake resolves a caller without a live session as auth.Service does:
// the same subject or none, the same status of the refusal.
func TestFakeResolvesLikeTheService(t *testing.T) {
	service := anonymousService(t)
	fake := newFakeSecurity()
	type outcome struct {
		subject bool
		status  int
	}
	resolve := func(sec model.Security, r *http.Request) outcome {
		s, err := sec.Subject(r)
		var e *model.Error
		if err != nil && !errors.As(err, &e) {
			t.Fatalf("%v", err)
		}
		o := outcome{subject: s != nil}
		if e != nil {
			o.status = e.Status
		}
		return o
	}
	for _, c := range []struct {
		name   string
		cookie bool
		auth   string
	}{
		{"nothing", false, ""},
		{"stale cookie", true, ""},
		{"stale cookie and wrong Basic", true, basicHeader("demo-admin:wrong")},
		{"wrong Basic", false, basicHeader("demo-admin:wrong")},
		{"Basic without a colon", false, "Basic " + base64.StdEncoding.EncodeToString([]byte("demo-admin"))},
		{"Bearer", false, "Bearer t-1"},
		{"stale cookie and Bearer", true, "Bearer t-1"},
	} {
		got := map[string]outcome{}
		for name, sec := range map[string]model.Security{"fake": fake, "service": service} {
			r := httptest.NewRequest("GET", "/repository/demo-maven/"+jarPath, nil)
			if c.cookie {
				cookie := "fake-session"
				if name == "service" {
					cookie = auth.CookieName
				}
				r.AddCookie(&http.Cookie{Name: cookie, Value: "expired"})
			}
			if c.auth != "" {
				r.Header.Set("Authorization", c.auth)
			}
			got[name] = resolve(sec, r)
		}
		if got["fake"] != got["service"] {
			t.Errorf("%s: fake %+v, service %+v", c.name, got["fake"], got["service"])
		}
	}
}

// Every route of the table with security on carries a rule.
func TestEverySecuredRouteHasARule(t *testing.T) {
	seen := map[string]bool{}
	for _, rt := range securedRoutes {
		if rt.rule.kind == ruleNone {
			t.Errorf("%s %v has no rule", rt.pattern, rt.methods)
		}
		key := rt.pattern.String() + " " + strings.Join(rt.methods, ",")
		if seen[key] {
			t.Errorf("%s twice", key)
		}
		seen[key] = true
	}
	var kinds []string
	for _, rt := range routes {
		if rt.rule.kind == ruleNone {
			kinds = append(kinds, rt.pattern.String())
		}
	}
	sort.Strings(kinds)
	if len(kinds) > 0 {
		t.Fatalf("routes of step 2 without a rule: %v", kinds)
	}
}
