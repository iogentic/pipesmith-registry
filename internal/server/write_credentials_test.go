package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Every credential of the security stream on a write with writes on (leg 4h
// of the phase 4 execution plan), through auth.Service over a state.sqlite as
// serve builds it: Basic, a session with its request token, a user token as
// a bearer token and as a password, an npm token on a publish, a dist tag
// and an unpublish, and a Docker bearer token on a push. Each write asks the
// rights the user holds at the moment of the request; a token never gives
// more, and no write passes without credentials, whatever the anonymous user
// holds.

// Invented users of the write tests: writerUser holds every action on
// demo-raw, demo-npm and demo-docker; pusherUser holds add, edit, read and
// browse of demo-docker only; the user of pullerUser reads demo-docker.
const (
	writerUser = "demo-writer"
	writerPass = "demo-writer-pass-1"
	pusherUser = "demo-pusher"
	pusherPass = "demo-pusher-pass-1"
)

// writeTokenSnapshot is tokenSnapshot with the realm of user tokens, the
// write users and a raw repository, and the write attributes of the old
// product on every repository.
func writeTokenSnapshot(t *testing.T) *model.Snapshot {
	t.Helper()
	snap := tokenSnapshot(t)
	hash := func(pass string) string {
		h, err := auth.HashPassword(pass)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	snap.Users = append(snap.Users,
		model.User{ID: writerUser, Status: "active", Source: auth.SourceLocal, Roles: []string{"demo-writers"}, PasswordHash: hash(writerPass)},
		model.User{ID: pusherUser, Status: "active", Source: auth.SourceLocal, Roles: []string{"demo-pushers"}, PasswordHash: hash(pusherPass)})
	snap.Roles = append(snap.Roles,
		model.Role{ID: "demo-writers", Source: auth.SourceLocal, Name: "writers", Privileges: []string{"demo-write"}},
		model.Role{ID: "demo-pushers", Source: auth.SourceLocal, Name: "pushers", Privileges: []string{"demo-push"}})
	snap.Privileges = append(snap.Privileges,
		model.Privilege{Name: "demo-write", Type: "wildcard",
			Properties: map[string]string{"pattern": "nexus:repository-view:*:demo-raw,demo-npm,demo-docker:*"}},
		model.Privilege{Name: "demo-push", Type: "wildcard",
			Properties: map[string]string{"pattern": "nexus:repository-view:docker:demo-docker:add,edit,read,browse"}})
	snap.Realms = append(snap.Realms, auth.RealmUserToken)
	open := map[string]any{"writePolicy": "ALLOW", "blobStoreName": "store-a"}
	for i := range snap.Repositories {
		r := &snap.Repositories[i]
		attributes := map[string]any{}
		for key, value := range r.Attributes {
			attributes[key] = value
		}
		attributes["storage"] = open
		r.Attributes = attributes
	}
	snap.Repositories = append(snap.Repositories, model.Repository{Name: "demo-raw", Format: "raw", Type: "hosted", Online: true,
		Attributes: map[string]any{"storage": open}})
	return snap
}

// newWriteTokenApp is newTokenApp over writeTokenSnapshot with writes on.
func newWriteTokenApp(t *testing.T) *tokenApp {
	t.Helper()
	return newTokenApp(t, writeTokenSnapshot(t), func(o *appOptions) {
		o.writes = true
		o.defs = append(o.defs, def("demo-raw", "raw", "hosted"))
	})
}

// port is the address of the Docker port of a repository.
func (a *tokenApp) port(repo string) string { return a.docker[repo].Listener.Addr().String() }

// signIn signs a user in through the interface and gives its session cookie
// and request token.
func (a *tokenApp) signIn(t *testing.T, user, pass string) (cookie, csrf string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	r := do(t, a.addr(), "POST", "/api/session", body, "Content-Type", "application/json")
	var answer struct {
		CSRFToken string `json:"csrfToken"`
	}
	if r.status != 200 || json.Unmarshal(r.body, &answer) != nil || answer.CSRFToken == "" {
		t.Fatalf("sign in: %d %q", r.status, r.body)
	}
	for _, c := range (&http.Response{Header: r.header}).Cookies() {
		if c.Name == auth.CookieName {
			return c.Name + "=" + c.Value, answer.CSRFToken
		}
	}
	t.Fatalf("sign in without a session cookie: %v", r.header)
	return "", ""
}

// userToken issues a user token of a local user, as `token issue` does.
func (a *tokenApp) userToken(t *testing.T, user string) string {
	t.Helper()
	text, row, err := auth.NewToken(auth.KindUser, user, auth.SourceLocal, a.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.AddToken(row, auth.MaxTokensPerKind); err != nil {
		t.Fatal(err)
	}
	return text
}

// dockerToken is the bearer token the token route of a Docker port gives
// for these headers.
func (a *tokenApp) dockerToken(t *testing.T, repo string, headers ...string) string {
	t.Helper()
	return a.dockerTokenOf(t, a.on(repo, "GET", "/v2/token?service=Pipesmith+Registry&scope=repository:app:pull,push", headers...)).Token
}

// pushBlob is a monolithic upload of one blob to the Docker port of repo.
func (a *tokenApp) pushBlob(t *testing.T, repo string, data []byte, headers ...string) *answer {
	t.Helper()
	return do(t, a.port(repo), "POST", "/v2/app/blobs/uploads/?digest="+url.QueryEscape(digestOf(data)), data,
		append([]string{"Content-Type", "application/octet-stream"}, headers...)...)
}

func TestEveryCredentialWrites(t *testing.T) {
	a := newWriteTokenApp(t)
	put := func(path string, body string, headers ...string) *answer {
		return do(t, a.addr(), "PUT", "/repository/demo-raw/"+path, []byte(body), headers...)
	}
	// Basic, through the cache and the limits of the pair
	status(t, put("basic.txt", "basic", "basic", writerUser+":"+writerPass), 201)
	status(t, put("basic-reader.txt", "basic", "basic", pullerUser+":"+pullerPass), 403)

	// a session of the interface writes with its request token only
	cookie, csrf := a.signIn(t, writerUser, writerPass)
	status(t, put("session.txt", "session", "Cookie", cookie, auth.CSRFHeader, csrf), 201)
	if r := put("session-no-token.txt", "session", "Cookie", cookie); r.status != 403 || !strings.Contains(string(r.body), csrfRefusal) {
		t.Fatalf("a session without its request token: %d %q", r.status, r.body)
	}

	// a user token as a bearer token and as the password of its user
	token := a.userToken(t, writerUser)
	status(t, put("user-token.txt", "bearer", "Authorization", "Bearer "+token), 201)
	status(t, put("user-token-basic.txt", "basic", "basic", writerUser+":"+token), 201)
	status(t, put("user-token-reader.txt", "bearer", "Authorization", "Bearer "+a.userToken(t, pullerUser)), 403)

	// an npm token publishes, tags and unpublishes
	npmToken := []string{"Authorization", "Bearer " + a.npmToken(t, writerUser, writerPass)}
	publish := do(t, a.addr(), "PUT", "/repository/demo-npm/demo-cred", npmPublishBody("demo-cred", "1.0.0", npmTarballOf(t, "cred")),
		append([]string{"Content-Type", jsonType}, npmToken...)...)
	status(t, publish, 201)
	tag := do(t, a.addr(), "PUT", "/repository/demo-npm/-/package/demo-cred/dist-tags/stable", []byte(`"1.0.0"`),
		append([]string{"Content-Type", jsonType}, npmToken...)...)
	if tag.status < 200 || tag.status > 299 {
		t.Fatalf("dist tag with an npm token: %d %q", tag.status, tag.body)
	}
	var document struct {
		Rev string `json:"_rev"`
	}
	if r := do(t, a.addr(), "GET", "/repository/demo-npm/demo-cred?write=true", nil, npmToken...); r.status != 200 ||
		json.Unmarshal(r.body, &document) != nil || document.Rev == "" {
		t.Fatalf("document: %d %q", r.status, r.body)
	}
	status(t, do(t, a.addr(), "DELETE", "/repository/demo-npm/demo-cred/-rev/"+document.Rev, nil, npmToken...), 200)
	readerNpm := []string{"Authorization", "Bearer " + a.npmToken(t, npmUser, npmPass)}
	status(t, do(t, a.addr(), "PUT", "/repository/demo-npm/demo-cred-2", npmPublishBody("demo-cred-2", "1.0.0", npmTarballOf(t, "cred 2")),
		append([]string{"Content-Type", jsonType}, readerNpm...)...), 403)

	// a Docker bearer token pushes with the rights of its user at the request
	pusher := "Bearer " + a.dockerToken(t, "demo-docker", "Authorization", basicPair(pusherUser, pusherPass))
	status(t, a.pushBlob(t, "demo-docker", []byte("pushed layer"), "Authorization", pusher), 201)
	denied := a.pushBlob(t, "demo-docker-b", []byte("pushed layer"), "Authorization", pusher)
	if denied.status != 403 || !strings.Contains(string(denied.body), "DENIED") {
		t.Fatalf("a token of demo-pusher on demo-docker-b: %d %q", denied.status, denied.body)
	}
	puller := "Bearer " + a.dockerToken(t, "demo-docker", "Authorization", basicPair(pullerUser, pullerPass))
	status(t, a.pushBlob(t, "demo-docker", []byte("pulled only"), "Authorization", puller), 403)
	a.change(t, func(s *model.Snapshot) {
		for i := range s.Users {
			if s.Users[i].ID == pusherUser {
				s.Users[i].Roles = nil
			}
		}
	})
	status(t, a.pushBlob(t, "demo-docker", []byte("after the role went"), "Authorization", pusher), 403)
}

// The anonymous user may hold every action on every repository: a write
// without credentials, and a write with the Docker token of the anonymous
// subject, still gets 401 with the challenge of its format, and writes
// nothing. A read without credentials passes.
func TestAnonymousNeverWrites(t *testing.T) {
	a := newWriteTokenApp(t)
	a.change(t, func(s *model.Snapshot) {
		s.Anonymous.Enabled = true
		for i := range s.Privileges {
			if s.Privileges[i].Name == "demo-anonymous-read" {
				s.Privileges[i].Properties = map[string]string{"pattern": "nexus:repository-view:*:*:*"}
			}
		}
	})
	status(t, do(t, a.addr(), "GET", "/repository/demo-npm/widget/-/widget-1.2.3.tgz", nil), 200)
	raw := do(t, a.addr(), "PUT", "/repository/demo-raw/anonymous.txt", []byte("anonymous"))
	if raw.status != 401 || raw.header.Get("WWW-Authenticate") != basicChallenge {
		t.Fatalf("raw PUT without credentials: %d %v %q", raw.status, raw.header, raw.body)
	}
	status(t, do(t, a.addr(), "GET", "/repository/demo-raw/anonymous.txt", nil), 404)
	publish := do(t, a.addr(), "PUT", "/repository/demo-npm/demo-anonymous", npmPublishBody("demo-anonymous", "1.0.0", npmTarballOf(t, "a")),
		"Content-Type", jsonType)
	status(t, publish, 401)
	status(t, do(t, a.addr(), "DELETE", "/repository/demo-npm/widget/-/widget-1.2.3.tgz/-rev/1-0", nil), 401)
	form, contentType := multipartBody(t, formField("raw.directory", "anonymous"), formFile("raw.asset1", "a.txt", []byte("a")),
		formField("raw.asset1.filename", "a.txt"))
	status(t, do(t, a.addr(), "POST", componentsPath+"?repository=demo-raw", form, "Content-Type", contentType), 401)

	anonymous := "Bearer " + a.dockerToken(t, "demo-docker")
	for _, headers := range [][]string{nil, {"Authorization", anonymous}} {
		r := a.pushBlob(t, "demo-docker", []byte("anonymous layer"), headers...)
		if r.status != 401 || !strings.Contains(string(r.body), "UNAUTHORIZED") ||
			!strings.HasSuffix(r.header.Get("WWW-Authenticate"), `,scope="repository:app:pull,push"`) {
			t.Fatalf("push %v: %d %v %q", headers, r.status, r.header, r.body)
		}
	}
	if row, _ := a.srv.rp.index.Lookup("demo-raw", "anonymous.txt"); row != nil {
		t.Fatalf("an anonymous write left %+v", row)
	}
}

// The challenge of a 401 on a Docker route names the scope the request
// needs, as the registry of the distribution project names it
// (registry/handlers/app.go, appendAccessRecords, and
// registry/auth/token/accesscontroller.go, scopeParam, read 2026-11-19):
// pull for GET and HEAD, pull and push for POST, PUT and PATCH, delete for
// DELETE, and pull of the source repository of a mount.
func TestTheChallengeOfAWriteNamesItsScope(t *testing.T) {
	a := newWriteTokenApp(t)
	realm := `Bearer realm="http://` + a.port("demo-docker") + `/v2/token",service="Pipesmith Registry"`
	for _, c := range []struct{ method, target, scope string }{
		{"GET", "/v2/app/manifests/1.0", `repository:app:pull`},
		{"HEAD", "/v2/app/blobs/" + digestOf([]byte("x")), `repository:app:pull`},
		{"POST", "/v2/app/blobs/uploads/", `repository:app:pull,push`},
		{"PATCH", "/v2/app/blobs/uploads/0123", `repository:app:pull,push`},
		{"PUT", "/v2/app/manifests/2.0", `repository:app:pull,push`},
		{"DELETE", "/v2/app/manifests/" + digestOf([]byte("x")), `repository:app:delete`},
		{"POST", "/v2/app/blobs/uploads/?mount=" + url.QueryEscape(digestOf([]byte("x"))) + "&from=other/name",
			`repository:app:pull,push repository:other/name:pull`},
	} {
		r := do(t, a.port("demo-docker"), c.method, c.target, nil, "Host", a.port("demo-docker"))
		if want := realm + `,scope="` + c.scope + `"`; r.status != 401 || r.header.Get("WWW-Authenticate") != want {
			t.Errorf("%s %s: %d %q, want %q", c.method, c.target, r.status, r.header.Get("WWW-Authenticate"), want)
		}
	}
}

// parallel sends n requests at one moment and gives their statuses; a
// request that fails in its transport gives 0.
func parallel(n int, request func(i int) (*http.Request, error)) []int {
	statuses := make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := request(i)
			if err != nil {
				return
			}
			<-start
			resp, err := noRedirect.Do(r)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			statuses[i] = resp.StatusCode
		}()
	}
	close(start)
	wg.Wait()
	return statuses
}

// The invented user of heldDirectory. Its group names the role of
// writerUser, so it holds every action on demo-raw, demo-npm and
// demo-docker.
const (
	heldUser = "demo-held"
	heldPass = "demo-held-pass-1"
)

// heldDirectory is a directory of heldUser whose sign ins wait while the
// test holds them, so the first check of a burst keeps its place of the pair
// until every write of the burst has arrived. logins counts the calls of
// Login.
type heldDirectory struct {
	mu     sync.Mutex
	gate   chan struct{}
	logins atomic.Int64
}

func newHeldDirectory() *heldDirectory {
	d := &heldDirectory{gate: make(chan struct{})}
	close(d.gate)
	return d
}

// hold makes the sign ins from now on wait; release lets them go.
func (d *heldDirectory) hold() (release func()) {
	gate := make(chan struct{})
	d.mu.Lock()
	d.gate = gate
	d.mu.Unlock()
	return func() { close(gate) }
}

func (d *heldDirectory) Login(ctx context.Context, user, pass string) (model.User, []string, error) {
	d.logins.Add(1)
	if user != heldUser || pass != heldPass {
		return model.User{}, nil, auth.ErrWrongPair
	}
	d.mu.Lock()
	gate := d.gate
	d.mu.Unlock()
	select {
	case <-gate:
	case <-ctx.Done():
		return model.User{}, nil, ctx.Err()
	}
	return model.User{ID: heldUser, Status: "active"}, []string{"demo-writers"}, nil
}

func (d *heldDirectory) Lookup(context.Context, string) (model.User, []string, error) {
	return model.User{}, nil, auth.ErrNoUser
}

// signIns counts the requests inside Subject: a request is there while it
// checks its credentials, waits for a place of its pair or waits for the
// check of another request with the same password.
type signIns struct {
	model.Security
	inside atomic.Int64
}

func (s *signIns) Subject(r *http.Request) (*model.Subject, error) {
	s.inside.Add(1)
	defer s.inside.Add(-1)
	return s.Security.Subject(r)
}

// More than five writes of one user at once pass with the right password
// (leg 4h). The sign in limits hold at most five checks of a pair in
// flight, a further request waits for a place, and parallel checks of one
// password share one comparison and then the cache of credentials. Each
// burst starts with an empty cache, and the directory holds the first check
// until every write of the burst is inside its sign in: with five places,
// the others wait at that moment. Then every write answers 201, and the
// directory compared the password once. The bursts are raw PUTs, the blobs
// of a Docker push and npm publishes, all with Basic credentials.
func TestParallelWritesOfOneUser(t *testing.T) {
	snap := writeTokenSnapshot(t)
	snap.Realms = append(snap.Realms, auth.RealmLDAP)
	dir := newHeldDirectory()
	var counted *signIns
	a := newTokenAppWith(t, snap, []auth.Directory{dir}, func(o *appOptions) {
		o.writes = true
		o.defs = append(o.defs, def("demo-raw", "raw", "hosted"))
		counted = &signIns{Security: o.security}
		o.security = counted
	})
	burst := func(name string, n int, request func(i int) (*http.Request, error)) {
		t.Helper()
		a.change(t, func(*model.Snapshot) {}) // a new generation
		// the reload of the new generation empties the cache of credentials,
		// here and not in the middle of the burst
		status(t, do(t, a.addr(), "GET", "/repository/demo-raw/parallel/none.txt", nil), 401)
		logins := dir.logins.Load()
		release := dir.hold()
		answers := make(chan []int)
		go func() { answers <- parallel(n, request) }()
		deadline := time.Now().Add(5 * time.Second)
		for counted.inside.Load() < int64(n) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		inside := counted.inside.Load()
		release()
		statuses := <-answers
		if inside != int64(n) {
			t.Errorf("%s: %d of %d writes inside their sign in while the first check was held", name, inside, n)
		}
		for i, got := range statuses {
			if got != 201 {
				t.Errorf("%s %d of %d: %d", name, i, n, got)
			}
		}
		if got := dir.logins.Load() - logins; got != 1 {
			t.Errorf("%s: the directory compared the password %d times, want once", name, got)
		}
	}
	burst("raw PUT", 24, func(i int) (*http.Request, error) {
		r, err := http.NewRequest("PUT", fmt.Sprintf("http://%s/repository/demo-raw/parallel/%d.txt", a.addr(), i), strings.NewReader("x"))
		if err == nil {
			r.SetBasicAuth(heldUser, heldPass)
		}
		return r, err
	})
	burst("Docker blob", 24, func(i int) (*http.Request, error) {
		data := []byte(fmt.Sprintf("parallel layer %d", i))
		r, err := http.NewRequest("POST", "http://"+a.port("demo-docker")+"/v2/app/blobs/uploads/?digest="+url.QueryEscape(digestOf(data)),
			bytes.NewReader(data))
		if err == nil {
			r.SetBasicAuth(heldUser, heldPass)
		}
		return r, err
	})
	burst("npm publish", 12, func(i int) (*http.Request, error) {
		name := fmt.Sprintf("demo-parallel-%d", i)
		r, err := http.NewRequest("PUT", "http://"+a.addr()+"/repository/demo-npm/"+name,
			bytes.NewReader(npmPublishBody(name, "1.0.0", []byte(fmt.Sprintf("\x1f\x8b package %d", i)))))
		if err == nil {
			r.SetBasicAuth(heldUser, heldPass)
			r.Header.Set("Content-Type", jsonType)
		}
		return r, err
	})
}
