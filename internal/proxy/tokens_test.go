package proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// The token flow of the registry protocol for a Docker proxy (Token
// Authentication Specification, CNCF Distribution, read 2026-11-19), with
// ruling P5-10 of the phase 5 session: the credentials of the repository go
// to the realm only when the challenge came from the origin of the
// repository and the realm is https or has that origin, a redirect from the
// realm never carries them, and a token lives in memory per repository and
// scope until it runs out.

const (
	manifestPath = "/v2/demo/app/manifests/1.0"
	blobPath     = "/v2/demo/app/blobs/sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tokenValue   = "CANARY-REMOTE-TOKEN-5a18"
)

// bearerOnly is an answer of the registry that needs the bearer token and
// challenges every other request for it at realm.
func bearerOnly(then testkit.Answer, realm, token string) testkit.Answer {
	then.Authorization = "Bearer " + token
	then.Challenge = `Bearer realm="` + realm + `",service="demo-registry",scope="repository:demo/app:pull"`
	return then
}

// tokenAnswer is the answer of a realm: a token document, with expires_in
// when expires is above 0.
func tokenAnswer(token string, expires int) testkit.Answer {
	body := `{"token":"` + token + `","access_token":"` + token + `"`
	if expires > 0 {
		body += `,"expires_in":` + strconv.Itoa(expires)
	}
	return testkit.OK(body+`}`).With("Content-Type", "application/json")
}

// tokenClient is the client of a Docker proxy with the remote address and
// the sign in given.
func tokenClient(t *testing.T, remote string, auth repos.RemoteAuth, secret string, now func() time.Time, log *lines) *Client {
	t.Helper()
	o := Options{Repository: "demo-docker-proxy", Settings: repos.ProxySettings{RemoteURL: remote, TimeoutSeconds: 5, Retries: 0, Auth: auth},
		Secret: secret, tokens: true, now: now}
	if log != nil {
		o.Logger = slog.New(log)
	}
	c, err := NewClient(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func tokenGet(t *testing.T, c *Client, path string) (int, string) {
	t.Helper()
	resp, err := c.Do(context.Background(), http.MethodGet, strings.TrimPrefix(path, "/"), "", nil)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// Anonymous: the challenge leads to a token request without credentials,
// with service and scope, and the request goes again with the token; the
// next request of the same name carries the token at once, and no log line
// holds it.
func TestTokenFlowWithoutCredentials(t *testing.T) {
	registry, realm := testkit.NewRemote(t), testkit.NewRemote(t)
	registry.Script(manifestPath, bearerOnly(testkit.OK("the manifest"), realm.URL()+"/token", tokenValue))
	registry.Script(blobPath, bearerOnly(testkit.OK("the blob"), realm.URL()+"/token", tokenValue))
	realm.Script("/token", tokenAnswer(tokenValue, 300))
	log := &lines{}
	c := tokenClient(t, registry.URL()+"/", repos.RemoteAuth{}, "", nil, log)
	if status, body := tokenGet(t, c, manifestPath); status != 200 || body != "the manifest" {
		t.Fatalf("manifest: %d %q", status, body)
	}
	requests := registry.Requests()
	if len(requests) != 2 || requests[0].Header.Get("Authorization") != "" || requests[1].Header.Get("Authorization") != "Bearer "+tokenValue {
		t.Fatalf("requests at the registry: %+v", requests)
	}
	asked := realm.Requests()
	if len(asked) != 1 || asked[0].Header.Get("Authorization") != "" {
		t.Fatalf("requests at the realm: %+v", asked)
	}
	query, err := url.ParseQuery(strings.SplitN(asked[0].Target, "?", 2)[1])
	if err != nil || query.Get("service") != "demo-registry" || query.Get("scope") != "repository:demo/app:pull" {
		t.Fatalf("the token request %s", asked[0].Target)
	}
	if status, body := tokenGet(t, c, blobPath); status != 200 || body != "the blob" {
		t.Fatalf("blob: %d %q", status, body)
	}
	if n := registry.Count(blobPath); n != 1 {
		t.Fatalf("%d requests of the blob: the kept token goes at once", n)
	}
	if n := len(realm.Requests()); n != 1 {
		t.Fatalf("%d token requests within the life of the token", n)
	}
	for _, line := range log.all() {
		if strings.Contains(line, tokenValue) {
			t.Fatalf("a log line holds the token: %q", line)
		}
	}
}

// With Basic credentials of the repository and a realm of its origin, the
// credentials go to the realm and the token to the registry.
func TestTokenFlowWithBasicCredentials(t *testing.T) {
	registry := testkit.NewRemote(t)
	registry.Script(manifestPath, bearerOnly(testkit.OK("the manifest"), registry.URL()+"/token", tokenValue))
	registry.Script("/token", testkit.BasicChallenge("demo-puller", "CANARY-REMOTE-PASSWORD-5a17", tokenAnswer(tokenValue, 0)))
	c := tokenClient(t, registry.URL()+"/", repos.RemoteAuth{Type: repos.AuthBasic, Username: "demo-puller"}, "CANARY-REMOTE-PASSWORD-5a17", nil, nil)
	if status, body := tokenGet(t, c, manifestPath); status != 200 || body != "the manifest" {
		t.Fatalf("manifest: %d %q", status, body)
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("demo-puller:CANARY-REMOTE-PASSWORD-5a17"))
	var token []string
	for _, req := range registry.Requests() {
		if strings.HasPrefix(req.Target, "/token") {
			token = append(token, req.Header.Get("Authorization"))
		}
	}
	if len(token) != 1 || token[0] != basic {
		t.Fatalf("the token requests carried %q", token)
	}
}

// A realm on another origin over http gets no credentials: the token
// request goes without them, the realm refuses it, and the 401 of the
// registry stands.
func TestTokenCredentialsStayOffAnotherOriginOverHTTP(t *testing.T) {
	registry, realm := testkit.NewRemote(t), testkit.NewRemote(t)
	registry.Script(manifestPath, bearerOnly(testkit.OK("the manifest"), realm.URL()+"/token", tokenValue))
	realm.Script("/token", testkit.BasicChallenge("demo-puller", "CANARY-REMOTE-PASSWORD-5a17", tokenAnswer(tokenValue, 0)))
	c := tokenClient(t, registry.URL()+"/", repos.RemoteAuth{Type: repos.AuthBasic, Username: "demo-puller"}, "CANARY-REMOTE-PASSWORD-5a17", nil, nil)
	if status, _ := tokenGet(t, c, manifestPath); status != http.StatusUnauthorized {
		t.Fatalf("status %d, want the 401 of the registry", status)
	}
	asked := realm.Requests()
	if len(asked) != 1 || asked[0].Header.Get("Authorization") != "" {
		t.Fatalf("requests at the realm: %+v", asked)
	}
	if n := registry.Count(manifestPath); n != 1 {
		t.Fatalf("%d requests at the registry, want 1", n)
	}
}

// The rule of ruling P5-10 for the credentials: https or the origin of the
// repository, and Basic credentials only.
func TestTokenCredentialsGoWhereTheRuleSays(t *testing.T) {
	c := tokenClient(t, "http://registry.example.test:5000/", repos.RemoteAuth{Type: repos.AuthBasic, Username: "demo-puller"}, "secret", nil, nil)
	anonymous := tokenClient(t, "http://registry.example.test:5000/", repos.RemoteAuth{}, "", nil, nil)
	for _, row := range []struct {
		realm string
		want  bool
	}{
		{"https://auth.example.test/token", true},
		{"https://auth.example.test:8443/token", true},
		{"http://registry.example.test:5000/token", true},
		{"HTTP://REGISTRY.example.test:5000/token", true},
		{"http://registry.example.test:5001/token", false},
		{"http://registry.example.test/token", false},
		{"http://auth.example.test/token", false},
	} {
		u, err := url.Parse(row.realm)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.mayCarry(u); got != row.want {
			t.Errorf("%s: %v, want %v", row.realm, got, row.want)
		}
		if anonymous.mayCarry(u) {
			t.Errorf("%s: a client without credentials sends some", row.realm)
		}
	}
}

// A redirect of the realm never carries the credentials, also to its own
// origin.
func TestTokenRedirectOfTheRealmCarriesNoCredentials(t *testing.T) {
	registry := testkit.NewRemote(t)
	registry.Script(manifestPath, bearerOnly(testkit.OK("the manifest"), registry.URL()+"/token", "token-of-the-moved-realm"))
	registry.Script("/token", testkit.Redirect(http.StatusFound, "/moved-token"))
	registry.Script("/moved-token", tokenAnswer("token-of-the-moved-realm", 0))
	c := tokenClient(t, registry.URL()+"/", repos.RemoteAuth{Type: repos.AuthBasic, Username: "demo-puller"}, "CANARY-REMOTE-PASSWORD-5a17", nil, nil)
	if status, body := tokenGet(t, c, manifestPath); status != 200 || body != "the manifest" {
		t.Fatalf("manifest: %d %q", status, body)
	}
	for _, req := range registry.Requests() {
		switch {
		case strings.HasPrefix(req.Target, "/token") && req.Header.Get("Authorization") == "":
			t.Errorf("the realm on the origin got no credentials: %+v", req)
		case strings.HasPrefix(req.Target, "/moved-token") && req.Header.Get("Authorization") != "":
			t.Errorf("the redirect of the realm carried credentials: %+v", req)
		}
	}
}

// A token is kept until it runs out: 60 s without expires_in, then the realm
// is asked again; a token the registry refuses is never sent again.
func TestTokenRunsOut(t *testing.T) {
	clock := testkit.NewClock(time.Date(2026, 11, 19, 7, 0, 0, 0, time.UTC))
	registry, realm := testkit.NewRemote(t), testkit.NewRemote(t)
	registry.Script(manifestPath, bearerOnly(testkit.OK("the manifest"), realm.URL()+"/token", "token-1"))
	realm.Script("/token", tokenAnswer("token-1", 0))
	c := tokenClient(t, registry.URL()+"/", repos.RemoteAuth{}, "", clock.Now, nil)
	for i, advance := range []time.Duration{0, 59 * time.Second, 2 * time.Second} {
		clock.Advance(advance)
		if status, _ := tokenGet(t, c, manifestPath); status != 200 {
			t.Fatalf("request %d: %d", i+1, status)
		}
	}
	if n := len(realm.Requests()); n != 2 {
		t.Fatalf("%d token requests: one at the start, one after 61 s", n)
	}
	// the registry refuses the kept token: the client asks the realm for a new one
	registry.Script(manifestPath, bearerOnly(testkit.OK("the manifest"), realm.URL()+"/token", "token-2"))
	realm.Script("/token", tokenAnswer("token-2", 300))
	if status, _ := tokenGet(t, c, manifestPath); status != 200 {
		t.Fatalf("after a refused token: %d", status)
	}
	if n := len(realm.Requests()); n != 3 {
		t.Fatalf("%d token requests after a refused token, want 3", n)
	}
}

// A challenge that comes from another origin than the remote address, here a
// host the registry redirects to, starts no token flow.
func TestTokenChallengeOfAnotherOrigin(t *testing.T) {
	registry, cdn, realm := testkit.NewRemote(t), testkit.NewRemote(t), testkit.NewRemote(t)
	registry.Script(blobPath, testkit.Redirect(http.StatusTemporaryRedirect, cdn.URL()+"/layer"))
	cdn.Script("/layer", bearerOnly(testkit.OK("the blob"), realm.URL()+"/token", tokenValue))
	realm.Script("/token", tokenAnswer(tokenValue, 300))
	c := tokenClient(t, registry.URL()+"/", repos.RemoteAuth{}, "", nil, nil)
	if status, _ := tokenGet(t, c, blobPath); status != http.StatusUnauthorized {
		t.Fatalf("status %d, want the 401 of the other host", status)
	}
	if n := len(realm.Requests()); n != 0 {
		t.Fatalf("%d token requests for a challenge of another origin", n)
	}
}

// The realm is dialled under ruling P5-8: an inner address of another kind
// than the remote address is refused.
func TestTokenRealmIsDialledUnderTheRuleOfInnerAddresses(t *testing.T) {
	registry, realm := testkit.NewRemote(t), testkit.NewRemote(t)
	registry.Script(manifestPath, bearerOnly(testkit.OK("the manifest"), realm.URL()+"/token", tokenValue))
	realm.Script("/token", tokenAnswer(tokenValue, 300))
	c, err := NewClient(Options{Repository: "demo-docker-proxy", Settings: repos.ProxySettings{RemoteURL: registry.URL() + "/", TimeoutSeconds: 5},
		tokens: true, classify: publicAt(addrPortOf(t, registry.URL()))})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	resp, err := c.Do(context.Background(), http.MethodGet, manifestPath[1:], "", nil)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("status %d: the realm on an inner address was asked", resp.StatusCode)
	}
	if !errors.Is(err, ErrInnerAddress) || len(realm.Requests()) != 0 {
		t.Fatalf("error %v, %d requests at the realm", err, len(realm.Requests()))
	}
}

// The parameters of a challenge: quoted values with commas and escapes, a
// token value, and the first Bearer challenge with a realm.
func TestBearerChallenges(t *testing.T) {
	for _, c := range []struct {
		values []string
		want   challenge
		ok     bool
	}{
		{[]string{`Bearer realm="https://auth.example.test/token",service="registry.example.test",scope="repository:demo/app:pull,push"`},
			challenge{"https://auth.example.test/token", "registry.example.test", "repository:demo/app:pull,push"}, true},
		{[]string{`bearer Realm="https://auth.example.test/t" , Service=registry.example.test`},
			challenge{"https://auth.example.test/t", "registry.example.test", ""}, true},
		{[]string{`Basic realm="registry"`, `Bearer realm="https://auth.example.test/token",scope="a\"b"`},
			challenge{"https://auth.example.test/token", "", `a"b`}, true},
		{[]string{`Basic realm="registry"`}, challenge{}, false},
		{[]string{`Bearer service="no realm"`}, challenge{}, false},
	} {
		resp := &http.Response{Header: http.Header{"Www-Authenticate": c.values}}
		got, ok := bearerChallenge(resp)
		if ok != c.ok || got != c.want {
			t.Errorf("%q: %+v %v, want %+v %v", c.values, got, ok, c.want, c.ok)
		}
	}
}
