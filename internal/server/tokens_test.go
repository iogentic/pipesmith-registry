package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	statefile "github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Task 12 of the phase 3 plan: npm login, whoami and logout below a
// repository path, the token route of the Docker flow, the bearer challenge
// of N12 that names only the host of the request (hardening line 12) and an
// anonymous pull only under the three conditions of N12.
//
// Wire shapes, read on 2026-11-18:
//   - npm login of the current client (npm 11.21.2, docs.npmjs.com/cli/v11/commands/npm-login): the web login
//     POST /-/v1/login comes first and an answer of 400 to 499 or 500 lets the client fall back to the legacy login
//     (github.com/npm/npm-profile, lib/index.js, webAuth and loginCouch): PUT
//     /-/user/org.couchdb.user:<name> with a JSON body of _id, name, password, type "user", roles and date. The
//     registry answers 201 with ok, id and token, and 401 when the pair is wrong
//     (github.com/npm/registry, docs/user/authentication.md, sections Login and Legacy Status). The client reads
//     the field token (github.com/npm/cli, lib/utils/auth.js).
//   - npm whoami: GET /-/whoami with the token, the answer field username (docs.npmjs.com/cli/v11/commands/npm-whoami;
//     github.com/npm/cli, lib/utils/get-identity.js).
//   - npm logout: DELETE /-/user/token/<token>, the token with encodeURIComponent, no body read
//     (docs.npmjs.com/cli/v11/commands/npm-logout; github.com/npm/cli, lib/commands/logout.js).
//   - Docker: a 401 carries WWW-Authenticate: Bearer realm="<token route>",service="<name>", with scope for a
//     resource; the client asks GET <realm>?service=...&scope=... with Basic credentials and gets 200 with token,
//     access_token, expires_in and issued_at, a token never with less than 60 s to live; a failed sign in is 401;
//     partial or no access is no error of the token route (distribution.github.io/distribution/spec/auth/token/,
//     Token Authentication Specification; /spec/auth/scope/ for repository:<name>:pull). The error codes
//     UNAUTHORIZED and DENIED and the 401 of GET /v2/ are those of distribution.github.io/distribution/spec/api/
//     and of the OCI distribution spec (github.com/opencontainers/distribution-spec, spec.md).

// clockAt is a fake clock for the reload interval of the service.
type clockAt struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clockAt) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clockAt) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Invented users and passwords of the token tests.
const (
	npmUser    = "demo-npm-user"
	npmPass    = "demo-npm-pass-1"
	pullerUser = "demo-puller"
	pullerPass = "demo-puller-pass-1"
	bothUser   = "demo-both"
	bothPass   = "demo-both-pass-1"
	adminUser  = "demo-admin"
	adminPass  = "demo-admin-pass-1"
)

func viewPrivilege(name, format, repo string) model.Privilege {
	return model.Privilege{Name: name, Type: "repository-view",
		Properties: map[string]string{"format": format, "repository": repo, "actions": "read,browse"}}
}

func dockerAttributes(forceBasic bool) map[string]any {
	return map[string]any{"docker": map[string]any{"forceBasicAuth": forceBasic, "v1Enabled": false},
		"storage": map[string]any{"blobStoreName": "store-a"}}
}

// tokenSnapshot holds users whose passwords hash at the start of the test,
// the realms of npm and Docker tokens, and two Docker repositories that let
// a bearer token in.
func tokenSnapshot(t *testing.T) *model.Snapshot {
	t.Helper()
	hash := func(pass string) string {
		h, err := auth.HashPassword(pass)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	return &model.Snapshot{
		Users: []model.User{
			{ID: npmUser, Status: "active", Source: auth.SourceLocal, Roles: []string{"demo-npm-readers"}, PasswordHash: hash(npmPass)},
			{ID: pullerUser, Status: "active", Source: auth.SourceLocal, Roles: []string{"demo-docker-a"}, PasswordHash: hash(pullerPass)},
			{ID: bothUser, Status: "active", Source: auth.SourceLocal, Roles: []string{"demo-docker-a", "demo-docker-b"},
				PasswordHash: hash(bothPass)},
			{ID: adminUser, Status: "active", Source: auth.SourceLocal, Roles: []string{"nx-admin"}, PasswordHash: hash(adminPass)},
			{ID: "anonymous", Status: "active", Source: auth.SourceLocal, Roles: []string{"demo-anonymous"}},
		},
		Roles: []model.Role{
			{ID: "demo-npm-readers", Source: auth.SourceLocal, Name: "npm readers", Privileges: []string{"demo-npm-read"}},
			{ID: "demo-docker-a", Source: auth.SourceLocal, Name: "pull a", Privileges: []string{"demo-docker-read"}},
			{ID: "demo-docker-b", Source: auth.SourceLocal, Name: "pull b", Privileges: []string{"demo-docker-b-read"}},
			{ID: "demo-anonymous", Source: auth.SourceLocal, Name: "anonymous", Privileges: []string{"demo-anonymous-read"}},
		},
		Privileges: []model.Privilege{
			viewPrivilege("demo-npm-read", "npm", "demo-npm"),
			viewPrivilege("demo-docker-read", "docker", "demo-docker"),
			viewPrivilege("demo-docker-b-read", "docker", "demo-docker-b"),
			{Name: "demo-anonymous-read", Type: "wildcard", Properties: map[string]string{"pattern": "nexus:repository-view:*:*:browse,read"}},
		},
		Realms:    []string{auth.RealmLocal, auth.RealmNpm, auth.RealmDocker},
		Anonymous: model.Anonymous{Enabled: false, UserID: "anonymous", RealmName: "NexusAuthorizingRealm"},
		Repositories: []model.Repository{
			{Name: "demo-docker", Format: "docker", Type: "hosted", Online: true, Attributes: dockerAttributes(false)},
			{Name: "demo-docker-b", Format: "docker", Type: "hosted", Online: true, Attributes: dockerAttributes(false)},
			{Name: "demo-maven", Format: "maven2", Type: "hosted", Online: true, Attributes: map[string]any{}},
			{Name: "demo-npm", Format: "npm", Type: "hosted", Online: true, Attributes: map[string]any{}},
		},
	}
}

// tokenApp is a server with security on through auth.Service over a
// state.sqlite of snap, as serve builds it, with two Docker ports.
type tokenApp struct {
	*app
	store *statefile.Store
	sec   *auth.Service
	clock *clockAt
}

func newTokenApp(t *testing.T, snap *model.Snapshot, edits ...func(*appOptions)) *tokenApp {
	t.Helper()
	return newTokenAppWith(t, snap, nil, edits...)
}

// newTokenAppWith is newTokenApp whose service also asks the directories
// dirs, in their order.
func newTokenAppWith(t *testing.T, snap *model.Snapshot, dirs []auth.Directory, edits ...func(*appOptions)) *tokenApp {
	t.Helper()
	st, err := statefile.Create(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Write(statefile.Kinds, snap); err != nil {
		t.Fatal(err)
	}
	gen, err := st.Generation()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	clock := &clockAt{t: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
	sec, err := auth.New(auth.Options{Snapshot: loaded, Generation: gen, Store: st, Now: clock.now, Directories: dirs})
	if err != nil {
		t.Fatal(err)
	}
	blobs := []testkit.BlobSpec{
		spec("demo-maven", jarPath, []byte("jar bytes"), "application/java-archive"),
		spec("demo-npm", "widget/-/widget-1.2.3.tgz", []byte("tarball"), "application/x-tgz"),
	}
	blobs = append(blobs, blobsOf("demo-docker", dockerImage("app", "1.0", []byte("layer a")))...)
	blobs = append(blobs, blobsOf("demo-docker-b", dockerImage("app", "1.0", []byte("layer b")))...)
	base := t.TempDir()
	stores := []config.StoreConfig{blobStore(t, base, "store-a", blobs...)}
	opts := appOptions{defs: []*pyjson.Object{def("demo-maven", "maven2", "hosted"), def("demo-npm", "npm", "hosted"),
		dockerDef("demo-docker", 14690), dockerDef("demo-docker-b", 14691)},
		dockerRepos: []string{"demo-docker", "demo-docker-b"}, security: sec, hostNames: []string{"registry.example.com"}}
	for _, edit := range edits {
		edit(&opts)
	}
	return &tokenApp{app: startApp(t, base, prepare(t, base, stores, opts), opts), store: st, sec: sec, clock: clock}
}

// change rewrites the records of the state as another process does, then
// lets the reload interval of the service pass.
func (a *tokenApp) change(t *testing.T, edit func(*model.Snapshot)) {
	t.Helper()
	snap, err := a.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	edit(snap)
	if err := a.store.Write(statefile.Kinds, snap); err != nil {
		t.Fatal(err)
	}
	a.clock.add(auth.ReloadInterval)
}

func basicPair(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

var npmTokenText = regexp.MustCompile(`^NpmToken\.[A-Za-z0-9_-]{11}\.[A-Za-z0-9_-]{43}$`)

func loginBody(name, pass string) []byte {
	body, _ := json.Marshal(map[string]any{"_id": "org.couchdb.user:" + name, "name": name, "password": pass, "type": "user",
		"roles": []string{}, "date": "2026-11-18T09:00:00.000Z"})
	return body
}

func (a *tokenApp) npmLogin(t *testing.T, name, pass string) *answer {
	t.Helper()
	return do(t, a.addr(), "PUT", "/repository/demo-npm/-/user/org.couchdb.user:"+name, loginBody(name, pass),
		"Content-Type", "application/json")
}

func (a *tokenApp) npmToken(t *testing.T, name, pass string) string {
	t.Helper()
	r := a.npmLogin(t, name, pass)
	status(t, r, 201)
	var got struct {
		OK    bool   `json:"ok"`
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(r.body, &got); err != nil || !got.OK || got.ID != "org.couchdb.user:"+name || !npmTokenText.MatchString(got.Token) {
		t.Fatalf("login answer: %v", err)
	}
	if r.header.Get("Content-Type") != jsonType || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("login headers %v", r.header)
	}
	return got.Token
}

func TestNpmLoginWhoamiLogout(t *testing.T) {
	a := newTokenApp(t, tokenSnapshot(t))
	token := a.npmToken(t, npmUser, npmPass)
	withToken := []string{"Authorization", "Bearer " + token}

	who := do(t, a.addr(), "GET", "/repository/demo-npm/-/whoami", nil, withToken...)
	status(t, who, 200)
	if string(who.body) != `{"username":"`+npmUser+`"}` || who.header.Get("Content-Type") != jsonType {
		t.Fatalf("whoami %q %v", who.body, who.header)
	}
	// npm install with the token only
	tarball := do(t, a.addr(), "GET", "/repository/demo-npm/widget/-/widget-1.2.3.tgz", nil, withToken...)
	if tarball.status != 200 || string(tarball.body) != "tarball" {
		t.Fatalf("tarball with the token: %d %q", tarball.status, tarball.body)
	}
	status(t, do(t, a.addr(), "GET", "/repository/demo-maven/"+jarPath, nil, withToken...), 403)

	// logout revokes the token and the access line names no secret half
	_, secret, _ := strings.Cut(strings.TrimPrefix(token, "NpmToken."), ".")
	status(t, do(t, a.addr(), "DELETE", "/repository/demo-npm/-/user/token/"+token, nil), 401)
	out := do(t, a.addr(), "DELETE", "/repository/demo-npm/-/user/token/"+token, nil, withToken...)
	if out.status != 200 || string(out.body) != `{"ok":true}` {
		t.Fatalf("logout: %d %q", out.status, out.body)
	}
	if strings.Contains(a.access.String(), secret) || !strings.Contains(a.access.String(), `"DELETE /repository/demo-npm/-/user/token/`) {
		t.Fatalf("access log %q", a.access.String())
	}
	for _, target := range []string{"/repository/demo-npm/-/whoami", "/repository/demo-npm/widget/-/widget-1.2.3.tgz"} {
		status(t, do(t, a.addr(), "GET", target, nil, withToken...), 401)
	}
}

func TestNpmLoginRefusals(t *testing.T) {
	a := newTokenApp(t, tokenSnapshot(t))
	wrong := a.npmLogin(t, npmUser, "demo-wrong-pass-1")
	if wrong.status != 401 || string(wrong.body) != `{"ok":false,"error":"wrong user name or password"}` ||
		wrong.header.Get("WWW-Authenticate") != basicChallenge {
		t.Fatalf("wrong pair: %d %q %v", wrong.status, wrong.body, wrong.header)
	}
	unknown := a.npmLogin(t, "demo-nobody-here", "demo-wrong-pass-1")
	if unknown.status != 401 || !bytes.Equal(unknown.body, wrong.body) {
		t.Fatalf("unknown user: %d %q", unknown.status, unknown.body)
	}
	put := func(target string, body []byte) *answer {
		return do(t, a.addr(), "PUT", target, body, "Content-Type", "application/json")
	}
	login := "/repository/demo-npm/-/user/org.couchdb.user:" + npmUser
	status(t, put(login, loginBody("demo-someone-else", npmPass)), 400)
	status(t, put(login, []byte(`["not an object"]`)), 400)
	status(t, put(login, []byte(`{"name":"`+npmUser+`"}`)), 400)
	big := put(login, append(append([]byte(`{"name":"`+npmUser+`","password":"`), bytes.Repeat([]byte("x"), 64<<10)...), `"}`...))
	if big.status != 413 || !strings.Contains(string(big.body), "64 KiB") {
		t.Fatalf("a body past 64 KiB: %d %q", big.status, big.body)
	}
	// the web login of the client finds no route and falls back
	web := do(t, a.addr(), "POST", "/repository/demo-npm/-/v1/login", []byte(`{}`), "Content-Type", "application/json")
	if web.status < 400 || web.status > 499 {
		t.Fatalf("web login: %d", web.status)
	}
	// whoami asks for a subject; the anonymous one names itself while the switch is on
	status(t, do(t, a.addr(), "GET", "/repository/demo-npm/-/whoami", nil), 401)
	old := do(t, a.addr(), "GET", "/repository/demo-npm/-/whoami", nil, "Authorization", "Bearer NpmToken."+"4b9c2a6e-0000-4000"+"-8000-00000000c0de")
	if old.status != 401 || !strings.Contains(string(old.body), "issues tokens new") {
		t.Fatalf("old token: %d %q", old.status, old.body)
	}
	a.change(t, func(s *model.Snapshot) { s.Anonymous.Enabled = true })
	anonymous := do(t, a.addr(), "GET", "/repository/demo-npm/-/whoami", nil)
	if anonymous.status != 200 || string(anonymous.body) != `{"username":"anonymous"}` {
		t.Fatalf("whoami of the anonymous subject: %d %q", anonymous.status, anonymous.body)
	}
	// the npm token realm leaves the order: a good pair gets no token
	a.change(t, func(s *model.Snapshot) { s.Realms = []string{auth.RealmLocal, auth.RealmDocker} })
	off := a.npmLogin(t, npmUser, npmPass)
	if off.status != 403 || !strings.Contains(string(off.body), auth.RealmNpm) {
		t.Fatalf("npm realm off: %d %q", off.status, off.body)
	}
}

// Below a repository of another format the paths of these routes answer as
// if the routes did not exist: the same answers as a neighbouring path.
func TestTokenRoutesBelowAnotherFormat(t *testing.T) {
	a := newTokenApp(t, tokenSnapshot(t))
	admin := []string{"Authorization", basicPair(adminUser, adminPass)}
	for _, c := range []struct{ method, path, neighbour string }{
		{"GET", "/repository/demo-maven/-/whoami", "/repository/demo-maven/-/whoamj"},
		{"PUT", "/repository/demo-maven/-/user/org.couchdb.user:x", "/repository/demo-maven/-/user/org.couchdb.usex:x"},
		{"DELETE", "/repository/demo-maven/-/user/token/x", "/repository/demo-maven/-/user/tokex/x"},
		{"GET", "/repository/demo-maven/v2/token", "/repository/demo-maven/v2/tokex"},
		{"GET", "/repository/demo-npm/v2/token", "/repository/demo-npm/v2/tokex"},
		{"GET", "/repository/demo-docker/-/whoami", "/repository/demo-docker/-/whoamj"},
		{"GET", "/repository/missing/-/whoami", "/repository/missing/-/whoamj"},
	} {
		for _, who := range [][]string{nil, admin} {
			got := do(t, a.addr(), c.method, c.path, nil, who...)
			want := do(t, a.addr(), c.method, c.neighbour, nil, who...)
			if got.status != want.status || got.header.Get("WWW-Authenticate") != want.header.Get("WWW-Authenticate") ||
				got.header.Get("Content-Type") != want.header.Get("Content-Type") || !bytes.Equal(got.body, want.body) {
				t.Errorf("%s %s as %v: %d %q, the neighbour %d %q", c.method, c.path, who, got.status, got.body, want.status, want.body)
			}
		}
	}
}

// challenge is the WWW-Authenticate header of a 401 on a Docker route.
func challenge(t *testing.T, r *response) string {
	t.Helper()
	if r.status != 401 || errorCode(t, r) != "UNAUTHORIZED" {
		t.Fatalf("not the 401 of the registry: %s", r)
	}
	return r.header.Get("WWW-Authenticate")
}

func TestDockerChallenge(t *testing.T) {
	a := newTokenApp(t, tokenSnapshot(t))
	port := a.docker["demo-docker"].Listener.Addr().String()
	_, portNumber, _ := strings.Cut(port, ":")
	main := a.addr()
	for _, host := range []string{"localhost:" + portNumber, "127.0.0.1:" + portNumber, "LOCALHOST:" + portNumber, "registry.example.com"} {
		got := challenge(t, a.on("demo-docker", "GET", "/v2/", "Host", host, "X-Forwarded-Host", "evil.example.com"))
		if want := `Bearer realm="http://` + host + `/v2/token",service="Pipesmith Registry"`; got != want {
			t.Errorf("Host %s: %q, want %q", host, got, want)
		}
	}
	// a Host that does not pass the guard gets no challenge
	if r := a.on("demo-docker", "GET", "/v2/", "Host", "evil.example.com"); r.status != 403 || r.header.Get("WWW-Authenticate") != "" {
		t.Fatalf("foreign Host: %s", r)
	}
	// a resource of the registry carries its scope
	got := challenge(t, a.on("demo-docker", "GET", "/v2/app/manifests/1.0", "Host", "localhost:"+portNumber))
	if want := `Bearer realm="http://localhost:` + portNumber + `/v2/token",service="Pipesmith Registry",scope="repository:app:pull"`; got != want {
		t.Errorf("manifest: %q, want %q", got, want)
	}
	// the main port names the token route below the repository
	got = challenge(t, raw(t, main, "GET", "/repository/demo-docker/v2/"))
	if want := `Bearer realm="http://` + main + `/repository/demo-docker/v2/token",service="Pipesmith Registry"`; got != want {
		t.Errorf("main port: %q, want %q", got, want)
	}
	// a repository that forces Basic sign in, and a stored order without the
	// realm of Docker tokens, ask for Basic
	a.change(t, func(s *model.Snapshot) { s.Repositories[1].Attributes = dockerAttributes(true) })
	if got := challenge(t, a.on("demo-docker-b", "GET", "/v2/")); got != basicChallenge {
		t.Errorf("forced Basic: %q", got)
	}
	a.change(t, func(s *model.Snapshot) { s.Realms = []string{auth.RealmLocal, auth.RealmNpm} })
	if got := challenge(t, a.on("demo-docker", "GET", "/v2/")); got != basicChallenge {
		t.Errorf("no Docker token realm: %q", got)
	}
	// a browser keeps the challenge of the session (N13)
	if got := a.on("demo-docker", "GET", "/v2/", "Cookie", "other=1").header.Get("WWW-Authenticate"); got != sessionChallenge {
		t.Errorf("browser: %q", got)
	}
}

type dockerTokenAnswer struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	IssuedAt    string `json:"issued_at"`
}

// dockerTokenOf reads a 200 of the token route, issued at the time now of
// the service.
func (a *tokenApp) dockerTokenOf(t *testing.T, r *response) dockerTokenAnswer {
	t.Helper()
	if r.status != 200 || r.header.Get("Content-Type") != jsonType || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("token route: %s", r)
	}
	var got dockerTokenAnswer
	dec := json.NewDecoder(bytes.NewReader(r.body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil || got.Token == "" || got.Token != got.AccessToken || got.ExpiresIn != 300 {
		t.Fatalf("token answer: %v", err)
	}
	if issued, err := time.Parse(time.RFC3339, got.IssuedAt); err != nil || !issued.Equal(a.clock.now()) {
		t.Fatalf("issued_at %q at %v: %v", got.IssuedAt, a.clock.now(), err)
	}
	return got
}

func TestDockerTokenRoute(t *testing.T) {
	a := newTokenApp(t, tokenSnapshot(t))
	flow := "/v2/token?service=Pipesmith+Registry&scope=repository:app:pull&account=" + pullerUser
	tok := a.dockerTokenOf(t, a.on("demo-docker", "GET", flow, "Authorization", basicPair(pullerUser, pullerPass)))
	withToken := []string{"Authorization", "Bearer " + tok.Token}
	expect(t, a.on("demo-docker", "GET", "/v2/", withToken...), 200)
	manifest := a.on("demo-docker", "GET", "/v2/app/manifests/1.0", withToken...)
	if manifest.status != 200 || manifest.header.Get("Docker-Content-Digest") == "" {
		t.Fatalf("manifest with the token: %s", manifest)
	}
	// the main port has the same route below the repository
	main := a.dockerTokenOf(t, raw(t, a.addr(), "GET", "/repository/demo-docker/v2/token", "Authorization", basicPair(pullerUser, pullerPass)))
	expect(t, raw(t, a.addr(), "GET", "/repository/demo-docker/v2/app/manifests/1.0", "Authorization", "Bearer "+main.Token), 200)
	// a wrong pair, another scheme, no credentials while anonymous access is off
	for _, header := range [][]string{{"Authorization", basicPair(pullerUser, "demo-wrong-pass-1")}, withToken, nil} {
		r := a.on("demo-docker", "GET", flow, header...)
		if r.status != 401 || errorCode(t, r) != "UNAUTHORIZED" || r.header.Get("WWW-Authenticate") != basicChallenge {
			t.Errorf("refused %v: %s", header, r)
		}
		if strings.Contains(string(r.body), tok.Token) {
			t.Error("a refusal names a token")
		}
	}
	// the token ends after 300 s
	a.clock.add(auth.DockerTokenTime)
	expect(t, a.on("demo-docker", "GET", "/v2/app/manifests/1.0", withToken...), 401)
	if strings.Contains(a.access.String(), tok.Token) {
		t.Error("the access log names a token")
	}
}

// An anonymous pull needs the realm of Docker tokens, anonymous access and
// the repository attribute, all three (decision N12).
func TestAnonymousPullNeedsThreeConditions(t *testing.T) {
	snap := tokenSnapshot(t)
	snap.Anonymous.Enabled = true
	a := newTokenApp(t, snap)
	pull := func(repo string) *response { return a.on(repo, "GET", "/v2/app/manifests/1.0") }
	anonymousToken := func(repo string) *response { return a.on(repo, "GET", "/v2/token?scope=repository:app:pull") }

	expect(t, pull("demo-docker"), 200)
	tok := a.dockerTokenOf(t, anonymousToken("demo-docker"))
	expect(t, a.on("demo-docker", "GET", "/v2/app/manifests/1.0", "Authorization", "Bearer "+tok.Token), 200)

	for name, change := range map[string]func(*model.Snapshot){
		"forced Basic":          func(s *model.Snapshot) { s.Repositories[0].Attributes = dockerAttributes(true) },
		"no Docker token realm": func(s *model.Snapshot) { s.Realms = []string{auth.RealmLocal, auth.RealmNpm} },
		"anonymous access off":  func(s *model.Snapshot) { s.Anonymous.Enabled = false },
		"no attribute":          func(s *model.Snapshot) { s.Repositories[0].Attributes = map[string]any{} },
	} {
		t.Run(name, func(t *testing.T) {
			a.change(t, func(s *model.Snapshot) {
				*s = *tokenSnapshot(t)
				s.Anonymous.Enabled = true
				change(s)
			})
			if r := pull("demo-docker"); r.status != 401 || errorCode(t, r) != "UNAUTHORIZED" {
				t.Errorf("anonymous pull: %s", r)
			}
			if r := anonymousToken("demo-docker"); r.status != 401 {
				t.Errorf("anonymous token: %s", r)
			}
			if r := a.on("demo-docker", "GET", "/v2/app/manifests/1.0", "Authorization", "Bearer "+tok.Token); r.status != 401 {
				t.Errorf("anonymous token of before: %s", r)
			}
			// a signed in user still pulls with Basic
			expect(t, a.on("demo-docker", "GET", "/v2/app/manifests/1.0", "Authorization", basicPair(pullerUser, pullerPass)), 200)
		})
	}
	// the other repository kept its three conditions all along
	a.change(t, func(s *model.Snapshot) {
		*s = *tokenSnapshot(t)
		s.Anonymous.Enabled = true
		s.Repositories[0].Attributes = dockerAttributes(true)
	})
	expect(t, pull("demo-docker-b"), 200)
	expect(t, pull("demo-docker"), 401)
}

// Review S6: what follows /-/user/token/ is masked in the access line of
// every answer and method, the 403 of the host guard included, and also when
// a byte of the path is percent escaped.
func TestNpmTokenMaskedInEveryAccessLine(t *testing.T) {
	a := newTokenApp(t, tokenSnapshot(t))
	token := a.npmToken(t, npmUser, npmPass)
	_, secret, _ := strings.Cut(strings.TrimPrefix(token, "NpmToken."), ".")
	cases := []struct {
		method, target string
		headers        []string
	}{
		{"DELETE", "/repository/demo-npm/-/user/token/" + token, []string{"Host", "evil.example.com"}},
		{"GET", "/repository/demo-npm/-/user/token/" + token, nil},
		{"PUT", "/repository/demo-npm/-/user/token/" + token, nil},
		{"DELETE", "/repository/demo-npm/-/user/tok%65n/" + token, nil},
		{"DELETE", "/repository/demo-npm/%2D/user/token/" + token, nil},
		{"DELETE", "/repository/demo-npm/-/user/token%2F" + token, nil},
		{"DELETE", "/repository/demo-npm/-/user/token/" + token + "?x=1", nil},
		{"DELETE", "/repository/demo-maven/-/user/token/" + token, nil},
	}
	for _, c := range cases {
		do(t, a.addr(), c.method, c.target, nil, c.headers...)
	}
	log := a.access.String()
	if strings.Contains(log, secret) || strings.Count(log, "/-/user/token/- HTTP/1.1") != len(cases) {
		t.Fatalf("access log %q", log)
	}
}
