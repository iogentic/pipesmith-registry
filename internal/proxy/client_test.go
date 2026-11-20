package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

const (
	canaryPassword = "CANARY-REMOTE-PASSWORD-5a31"
	canaryToken    = "CANARY-REMOTE-TOKEN-5a32"
	testVersion    = "9.9.9-test"
)

// settingsOf are the settings of a proxy of the scripted remote, below the
// base path /base/, with a timeout of 1 s and no retry.
func settingsOf(remote *testkit.Remote) repos.ProxySettings {
	return repos.ProxySettings{RemoteURL: remote.URL() + "/base/", ContentMaxAge: 1440, MetadataMaxAge: 1440,
		NotFoundCache: true, NotFoundTTL: 1440, AutoBlock: true, Retries: 0, TimeoutSeconds: 1}
}

func newClient(t *testing.T, o Options) *Client {
	t.Helper()
	if o.Repository == "" {
		o.Repository = "demo-proxy"
	}
	if o.Version == "" {
		o.Version = testVersion
	}
	c, err := NewClient(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

type answer struct {
	resp *http.Response
	body []byte
	err  error // of the request or of the body
}

func get(t *testing.T, c *Client, path string, header http.Header) answer {
	t.Helper()
	resp, err := c.Do(context.Background(), http.MethodGet, path, "", header)
	if err != nil {
		return answer{err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return answer{resp: resp, body: body, err: err}
}

func basic(user, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+secret))
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// A request goes to the remote address plus the request path, with the
// headers of the caller and the user agent of this server.
func TestClientAsksTheRemoteAddressPlusThePath(t *testing.T) {
	remote := testkit.NewRemote(t)
	remote.Script("/base/com/example/a/1.0/a-1.0.jar", testkit.OK("jar bytes").With("ETag", `"a1"`))
	s := settingsOf(remote)
	s.UserAgentSuffix = "demo-suffix/1"
	c := newClient(t, Options{Settings: s})
	got := get(t, c, "/com/example/a/1.0/a-1.0.jar", http.Header{"Range": {"bytes=0-3"}, "If-None-Match": {`"a0"`},
		"Accept": {"*/*"}, "User-Agent": {"caller/1"}, "Authorization": {"Basic c2VjcmV0"}})
	if got.err != nil || got.resp.StatusCode != 200 || string(got.body) != "jar bytes" || got.resp.Header.Get("ETag") != `"a1"` {
		t.Fatalf("%+v", got)
	}
	want := []testkit.Request{{Method: "GET", Target: "/base/com/example/a/1.0/a-1.0.jar", Host: strings.TrimPrefix(remote.URL(), "http://"),
		Header: http.Header{"Range": {"bytes=0-3"}, "If-None-Match": {`"a0"`}, "Accept": {"*/*"},
			"User-Agent": {"Pipesmith-Registry/" + testVersion + " demo-suffix/1"}}}}
	if got := remote.Requests(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("record\n%+v\nwant\n%+v", got, want)
	}
	// without a suffix the agent names the server and its version only, and a path that leaves the base sends nothing
	c = newClient(t, Options{Settings: settingsOf(remote)})
	get(t, c, "/com/example/a/1.0/a-1.0.jar", nil)
	if agent := remote.Requests()[1].Header.Get("User-Agent"); agent != "Pipesmith-Registry/"+testVersion {
		t.Fatalf("user agent %q", agent)
	}
	if got := get(t, c, "/../secret", nil); !errors.Is(got.err, ErrPath) || len(remote.Requests()) != 2 {
		t.Fatalf("%+v, %d requests", got, len(remote.Requests()))
	}
}

// blockingDialer never connects; only the dial timeout of the client ends
// its wait.
type blockingDialer struct{}

func (blockingDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// The dial timeout, the response header timeout and the total timeout each
// end a request.
func TestClientTimeouts(t *testing.T) {
	t.Run("dial", func(t *testing.T) {
		remote := testkit.NewRemote(t)
		remote.Script("/base/a", testkit.OK("a"))
		c := newClient(t, Options{Settings: settingsOf(remote), dialer: blockingDialer{}})
		start := time.Now()
		got := get(t, c, "/a", nil)
		if got.err == nil || !isTimeout(got.err) {
			t.Fatalf("%+v", got)
		}
		if waited := time.Since(start); waited < time.Second || waited > 15*time.Second {
			t.Fatalf("the dial timeout of 1 s ended the request after %s", waited)
		}
		if n := remote.Count("/base/a"); n != 0 {
			t.Fatalf("%d requests reached the remote", n)
		}
	})
	t.Run("response header", func(t *testing.T) {
		remote := testkit.NewRemote(t)
		remote.Script("/base/slow", testkit.SlowHeader(time.Minute, testkit.OK("late")))
		c := newClient(t, Options{Settings: settingsOf(remote)})
		start := time.Now()
		got := get(t, c, "/slow", nil)
		if got.err == nil || !isTimeout(got.err) {
			t.Fatalf("%+v", got)
		}
		if waited := time.Since(start); waited < time.Second || waited > 15*time.Second {
			t.Fatalf("the header timeout of 1 s ended the request after %s", waited)
		}
		if n := remote.Count("/base/slow"); n != 1 {
			t.Fatalf("%d requests, want 1 without a retry", n)
		}
	})
	t.Run("total", func(t *testing.T) {
		remote := testkit.NewRemote(t)
		remote.Script("/base/drip", testkit.SlowBody(time.Minute, "first half second half"))
		c := newClient(t, Options{Settings: settingsOf(remote), TotalTimeout: 300 * time.Millisecond})
		start := time.Now()
		got := get(t, c, "/drip", nil)
		if got.resp == nil || got.resp.StatusCode != 200 || !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("%+v", got)
		}
		if waited := time.Since(start); waited > 15*time.Second {
			t.Fatalf("the total timeout of 300 ms ended the body after %s", waited)
		}
	})
}

// redirects scripts a chain of n redirects from /base/r0 to /base/end.
func redirects(remote *testkit.Remote, n int) {
	for i := range n {
		remote.Script(fmt.Sprintf("/base/r%d", i), testkit.Redirect(http.StatusFound, fmt.Sprintf("/base/r%d", i+1)))
	}
	remote.Script(fmt.Sprintf("/base/r%d", n), testkit.OK("end"))
}

// At most 5 redirects are followed, the sixth is refused; a redirect to
// another scheme or to an address with a user part is refused too.
func TestClientRedirects(t *testing.T) {
	remote := testkit.NewRemote(t)
	redirects(remote, 5)
	c := newClient(t, Options{Settings: settingsOf(remote)})
	if got := get(t, c, "/r0", nil); got.err != nil || string(got.body) != "end" || len(remote.Requests()) != 6 {
		t.Fatalf("5 redirects: %+v, %d requests", got, len(remote.Requests()))
	}
	six := testkit.NewRemote(t)
	redirects(six, 6)
	c = newClient(t, Options{Settings: settingsOf(six)})
	if got := get(t, c, "/r0", nil); !errors.Is(got.err, ErrRedirects) || len(six.Requests()) != 6 {
		t.Fatalf("6 redirects: %+v, %d requests", got, len(six.Requests()))
	}
	for location, want := range map[string]error{"ftp://files.example.com/a": ErrRedirectScheme,
		"http://demo-user:" + canaryPassword + "@127.0.0.1:9/a": ErrRedirectUser} {
		other := testkit.NewRemote(t)
		other.Script("/base/away", testkit.Redirect(http.StatusFound, location))
		c := newClient(t, Options{Settings: settingsOf(other)})
		got := get(t, c, "/away", nil)
		if !errors.Is(got.err, want) || len(other.Requests()) != 1 {
			t.Fatalf("%s: %+v", want, got)
		}
		if strings.Contains(got.err.Error(), canaryPassword) {
			t.Fatalf("the refusal repeats the user part: %v", got.err)
		}
	}
}

// Credentials go only to the origin of the repository: scheme, host name and
// port. A redirect to another port of the same name and one to another name
// of the same listener arrive without them, one inside the origin with them.
func TestClientCredentialsStayWithTheOrigin(t *testing.T) {
	for _, auth := range []repos.RemoteAuth{{Type: repos.AuthBasic, Username: "demo-user", Preemptive: true},
		{Type: repos.AuthBearer, Preemptive: true}} {
		remote, otherPort := testkit.NewRemote(t), testkit.NewRemote(t)
		otherName := remote.URLAt("localhost")
		remote.Script("/base/inside", testkit.Redirect(http.StatusFound, "/base/moved"))
		remote.Script("/base/moved", testkit.OK("inside"))
		remote.Script("/base/to-port", testkit.Redirect(http.StatusFound, otherPort.URL()+"/elsewhere"))
		remote.Script("/base/to-name", testkit.Redirect(http.StatusTemporaryRedirect, otherName+"/elsewhere"))
		otherPort.Script("/elsewhere", testkit.OK("port"))
		remote.Script("/elsewhere", testkit.OK("name"))
		s := settingsOf(remote)
		s.Auth = auth
		secret := map[string]string{repos.AuthBasic: canaryPassword, repos.AuthBearer: canaryToken}[auth.Type]
		c := newClient(t, Options{Settings: s, Secret: secret})
		credential := map[string]string{repos.AuthBasic: basic("demo-user", canaryPassword),
			repos.AuthBearer: "Bearer " + canaryToken}[auth.Type]
		for path, body := range map[string]string{"/inside": "inside", "/to-port": "port", "/to-name": "name"} {
			if got := get(t, c, path, nil); got.err != nil || string(got.body) != body {
				t.Fatalf("%s %s: %+v", auth.Type, path, got)
			}
		}
		for _, r := range append(remote.Requests(), otherPort.Requests()...) {
			atOrigin := r.Host == strings.TrimPrefix(remote.URL(), "http://")
			if has := r.Header.Get("Authorization") == credential; has != atOrigin {
				t.Errorf("%s: %s %s at %s carries the credentials: %v", auth.Type, r.Method, r.Target, r.Host, has)
			}
		}
	}
}

// On a challenge the client asks once more with the credentials; at once it
// sends them with the first request: exactly one request less. A wrong
// secret ends after the second request, a challenge of another scheme and a
// missing secret get no credentials.
func TestClientChallenge(t *testing.T) {
	cases := []struct {
		name     string
		auth     repos.RemoteAuth
		secret   string
		answer   testkit.Answer
		status   int
		requests int
	}{
		{"Basic on challenge", repos.RemoteAuth{Type: repos.AuthBasic, Username: "demo-user"}, canaryPassword,
			testkit.BasicChallenge("demo-user", canaryPassword, testkit.OK("in")), 200, 2},
		{"Basic at once", repos.RemoteAuth{Type: repos.AuthBasic, Username: "demo-user", Preemptive: true}, canaryPassword,
			testkit.BasicChallenge("demo-user", canaryPassword, testkit.OK("in")), 200, 1},
		{"bearer on challenge", repos.RemoteAuth{Type: repos.AuthBearer}, canaryToken,
			testkit.BearerChallenge(canaryToken, testkit.OK("in")), 200, 2},
		{"bearer at once", repos.RemoteAuth{Type: repos.AuthBearer, Preemptive: true}, canaryToken,
			testkit.BearerChallenge(canaryToken, testkit.OK("in")), 200, 1},
		{"a wrong secret", repos.RemoteAuth{Type: repos.AuthBasic, Username: "demo-user"}, "wrong-" + canaryPassword,
			testkit.BasicChallenge("demo-user", canaryPassword, testkit.OK("in")), 401, 2},
		{"a challenge of another scheme", repos.RemoteAuth{Type: repos.AuthBasic, Username: "demo-user"}, canaryPassword,
			testkit.BearerChallenge(canaryToken, testkit.OK("in")), 401, 1},
		{"no secret", repos.RemoteAuth{Type: repos.AuthBasic, Username: "demo-user"}, "",
			testkit.BasicChallenge("demo-user", canaryPassword, testkit.OK("in")), 401, 1},
		{"a type this server does not build", repos.RemoteAuth{Type: "ntlm", Username: "demo-user"}, canaryPassword,
			testkit.BasicChallenge("demo-user", canaryPassword, testkit.OK("in")), 401, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remote := testkit.NewRemote(t)
			remote.Script("/base/private", tc.answer)
			s := settingsOf(remote)
			s.Auth = tc.auth
			c := newClient(t, Options{Settings: s, Secret: tc.secret})
			got := get(t, c, "/private", nil)
			if got.err != nil || got.resp.StatusCode != tc.status || remote.Count("/base/private") != tc.requests {
				t.Fatalf("%+v, %d requests; want %d and %d requests", got, remote.Count("/base/private"), tc.status, tc.requests)
			}
		})
	}
}

// A body over the limit of the repository is cut and discarded: a declared
// length over it ends the request at once, a body of unknown length ends at
// the first byte past it, with an error of its own.
func TestClientBodyLimit(t *testing.T) {
	remote := testkit.NewRemote(t)
	remote.Script("/base/declared", testkit.OK(strings.Repeat("d", 101)))
	remote.Script("/base/chunked", testkit.Chunked(strings.Repeat("c", 101)))
	remote.Script("/base/exact", testkit.Chunked(strings.Repeat("e", 100)))
	c := newClient(t, Options{Settings: settingsOf(remote), MaxBodyBytes: 100})
	if got := get(t, c, "/declared", nil); !errors.Is(got.err, ErrBodyTooLarge) || got.resp != nil {
		t.Fatalf("a declared length over the limit: %+v", got)
	}
	resp, err := c.Do(context.Background(), http.MethodGet, "/chunked", "", nil)
	if err != nil || resp.ContentLength != -1 {
		t.Fatalf("a body of unknown length: %v, length %d", err, resp.ContentLength)
	}
	body, err := io.ReadAll(resp.Body)
	if !errors.Is(err, ErrBodyTooLarge) || len(body) > 100 {
		t.Fatalf("a body of unknown length over the limit: %v, %d bytes", err, len(body))
	}
	// the cut is final: the body gives no further byte, only the error again
	if n, err := resp.Body.Read(make([]byte, 10)); n != 0 || !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("a read after the cut: %d bytes, %v", n, err)
	}
	_ = resp.Body.Close()
	if got := get(t, c, "/exact", nil); got.err != nil || len(got.body) != 100 {
		t.Fatalf("a body of the limit: %+v", got)
	}
	if c.limit != 100 || newClient(t, Options{Settings: settingsOf(remote)}).limit != repos.DefaultMaxBodyBytes {
		t.Fatal("the limit is not the one of the options, else the default of the write settings")
	}
}

// The retry count of the settings is kept: a GET that fails before an answer
// goes out once more per retry, a timeout of the header counts as such a
// failure, an answer of the remote, 500 among them, is never repeated, and
// neither is a PUT.
func TestClientRetries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		retries  int
		method   string
		answers  []testkit.Answer
		ok       bool
		requests int
	}{
		{"two retries reach the third answer", 2, http.MethodGet, []testkit.Answer{testkit.Closed(), testkit.Closed(), testkit.OK("third")}, true, 3},
		{"one retry is not enough", 1, http.MethodGet, []testkit.Answer{testkit.Closed(), testkit.Closed(), testkit.OK("third")}, false, 2},
		{"no retry", 0, http.MethodGet, []testkit.Answer{testkit.Closed(), testkit.OK("second")}, false, 1},
		{"HEAD is repeated", 1, http.MethodHead, []testkit.Answer{testkit.Closed(), testkit.OK("second")}, true, 2},
		{"a timeout of the header is repeated", 1, http.MethodGet,
			[]testkit.Answer{testkit.SlowHeader(time.Minute, testkit.OK("late")), testkit.OK("second")}, true, 2},
		{"500 is an answer", 2, http.MethodGet, []testkit.Answer{testkit.ServerError(), testkit.OK("second")}, true, 1},
		{"a PUT is not repeated", 2, http.MethodPut, []testkit.Answer{testkit.Closed(), testkit.OK("second")}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := testkit.NewRemote(t)
			remote.Script("/base/flaky", tc.answers...)
			s := settingsOf(remote)
			s.Retries = tc.retries
			c := newClient(t, Options{Settings: s})
			resp, err := c.Do(context.Background(), tc.method, "/flaky", "", nil)
			if resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			if (err == nil) != tc.ok || remote.Count("/base/flaky") != tc.requests {
				t.Fatalf("error %v, %d requests; want ok %v and %d requests", err, remote.Count("/base/flaky"), tc.ok, tc.requests)
			}
		})
	}
}

// The client ignores the proxy variables of the environment (decision 5.14)
// and verifies certificates with the system roots, TLS 1.2 or newer, with no
// way to switch the check off.
func TestClientTransport(t *testing.T) {
	remote, trap := testkit.NewRemote(t), testkit.NewRemote(t)
	remote.Script("/base/a", testkit.OK("a"))
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(name, trap.URL())
	}
	t.Setenv("NO_PROXY", "")
	c := newClient(t, Options{Settings: settingsOf(remote)})
	if got := get(t, c, "/a", nil); got.err != nil || remote.Count("/base/a") != 1 || len(trap.Requests()) != 0 {
		t.Fatalf("%+v, remote %d, trap %d", got, remote.Count("/base/a"), len(trap.Requests()))
	}
	if c.transport.Proxy != nil {
		t.Fatal("the transport reads a proxy")
	}
	tlsConfig := c.transport.TLSClientConfig
	if tlsConfig == nil || tlsConfig.MinVersion < 0x0303 || tlsConfig.InsecureSkipVerify || tlsConfig.RootCAs != nil {
		t.Fatalf("TLS %+v", tlsConfig)
	}
	secure := testkit.NewTLSRemote(t)
	secure.Script("/base/a", testkit.OK("a"))
	s := settingsOf(secure)
	c = newClient(t, Options{Settings: s})
	if got := get(t, c, "/a", nil); got.err == nil || !strings.Contains(got.err.Error(), "certificate") || secure.Count("/base/a") != 0 {
		t.Fatalf("a certificate no system root signs: %+v", got)
	}
}

// An address the client cannot use is refused when the client is made.
func TestNewClientRefusals(t *testing.T) {
	for _, address := range []string{"", "ftp://files.example.com/", "https:///maven2/", "https://user:pass@repo.example.com/",
		"https://repo.example.com/#top"} {
		if _, err := NewClient(Options{Repository: "demo", Settings: repos.ProxySettings{RemoteURL: address, TimeoutSeconds: 1}}); err == nil {
			t.Errorf("%q made a client", address)
		} else if strings.Contains(err.Error(), "pass") {
			t.Errorf("the refusal repeats the user part: %v", err)
		}
	}
	if _, err := NewClient(Options{Repository: "demo", Settings: repos.ProxySettings{RemoteURL: "https://repo.example.com/"}}); err == nil {
		t.Error("a timeout of 0 s made a client")
	}
}

// A canary password and a canary token reach the remote at its origin and
// appear in no log line, no error text and no answer of the client, over
// redirects, challenges, wrong secrets, retries and timeouts.
func TestClientCanaries(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	remote, other := testkit.NewRemote(t), testkit.NewRemote(t)
	remote.Script("/base/private", testkit.BasicChallenge("demo-user", canaryPassword, testkit.OK("private")))
	remote.Script("/base/away", testkit.Redirect(http.StatusFound, other.URL()+"/x"))
	other.Script("/x", testkit.ServerError())
	remote.Script("/base/flaky", testkit.Closed(), testkit.Closed(), testkit.ServerError())
	remote.Script("/base/token", testkit.BearerChallenge(canaryToken, testkit.OK("token")))
	// a remote that knows the secrets from a request can send them back in the user part of a redirect
	remote.Script("/base/echo", testkit.Redirect(http.StatusFound, "http://demo-user:"+canaryPassword+"@127.0.0.1:9/x"))
	remote.Script("/base/echo-token", testkit.Redirect(http.StatusFound, "http://"+canaryToken+"@127.0.0.1:9/x"))
	var seen []string
	for _, auth := range []struct {
		auth   repos.RemoteAuth
		secret string
	}{
		{repos.RemoteAuth{Type: repos.AuthBasic, Username: "demo-user"}, canaryPassword},
		{repos.RemoteAuth{Type: repos.AuthBasic, Username: "demo-user", Preemptive: true}, canaryPassword},
		{repos.RemoteAuth{Type: repos.AuthBasic, Username: "demo-user"}, "wrong-" + canaryPassword},
		{repos.RemoteAuth{Type: repos.AuthBearer}, canaryToken},
		{repos.RemoteAuth{Type: repos.AuthBearer, Preemptive: true}, "wrong-" + canaryToken},
	} {
		s := settingsOf(remote)
		s.Auth, s.Retries = auth.auth, 2
		c := newClient(t, Options{Settings: s, Secret: auth.secret, Logger: logger})
		for _, path := range []string{"/private", "/away", "/flaky", "/token", "/echo", "/echo-token", "/../x"} {
			resp, err := c.Do(context.Background(), http.MethodGet, path, "", nil)
			if err != nil {
				seen = append(seen, err.Error(), fmt.Sprintf("%+v", err))
			}
			if resp != nil {
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				seen = append(seen, string(body), fmt.Sprintf("%+v", resp), fmt.Sprintf("%v", resp.Header))
				if resp.Request != nil {
					seen = append(seen, fmt.Sprintf("%+v", resp.Request.Header))
				}
				if err != nil {
					seen = append(seen, err.Error())
				}
			}
		}
	}
	seen = append(seen, logs.String())
	text := strings.Join(seen, "\n")
	for _, secret := range []string{canaryPassword, canaryToken, base64.StdEncoding.EncodeToString([]byte("demo-user:" + canaryPassword))} {
		if strings.Contains(text, secret) {
			t.Fatalf("%q appears in what the client gave back:\n%s", secret, text)
		}
	}
	if !strings.Contains(logs.String(), "trying again") {
		t.Fatalf("no retry was logged:\n%s", logs.String())
	}
	// the positive twin: the secrets did reach the remote, at its origin only
	var atOrigin int
	for _, r := range remote.Requests() {
		if a := r.Header.Get("Authorization"); a == basic("demo-user", canaryPassword) || a == "Bearer "+canaryToken {
			atOrigin++
		}
	}
	for _, r := range other.Requests() {
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("credentials left the origin: %+v", r)
		}
	}
	if atOrigin == 0 {
		t.Fatal("no request carried the secrets, so the search above proves nothing")
	}
}
