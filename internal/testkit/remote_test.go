package testkit_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// client is a plain client of the standard library with a transport of its
// own, closed when the test ends. Each request takes a new connection, so
// the transport never sends a request again on its own after a connection
// that it used before was closed.
func client(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{Proxy: nil, DisableCompression: true, DisableKeepAlives: true}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

type sent struct {
	status int
	header http.Header
	body   string
	err    error
}

// send makes one request with the given headers and reads the whole body.
func send(t *testing.T, c *http.Client, method, target string, header map[string]string) sent {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return sent{err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return sent{status: resp.StatusCode, header: resp.Header, body: string(body), err: err}
}

// recorded is the request a remote records for a request with these
// headers, from the six it keeps.
func recorded(method, target, host string, header map[string]string) testkit.Request {
	h := http.Header{}
	for k, v := range header {
		h.Set(k, v)
	}
	return testkit.Request{Method: method, Target: target, Host: host, Header: h}
}

func sameRecord(t *testing.T, got, want []testkit.Request) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record\n%+v\nwant\n%+v", got, want)
	}
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	return u.Host
}

// The remote listens on loopback only, and its second name is localhost on
// the same port.
func TestRemoteListensOnLoopback(t *testing.T) {
	r := testkit.NewRemote(t)
	u, err := url.Parse(r.URL())
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "" {
		t.Fatalf("URL %s, want http://127.0.0.1:<port>", r.URL())
	}
	if got, want := r.URLAt("localhost"), "http://localhost:"+u.Port(); got != want {
		t.Fatalf("URLAt localhost %s, want %s", got, want)
	}
	other := testkit.NewRemote(t)
	if hostOf(other.URL()) == u.Host {
		t.Fatal("a second remote took the same port")
	}
}

// Each of the twelve cases answers as its name says, and the record equals
// the requests the test sent.
func TestRemoteAnswersEachCase(t *testing.T) {
	all := map[string]string{"Authorization": "Basic ZGVtbzpkZW1v", "Range": "bytes=0-3", "If-None-Match": `"v1"`,
		"If-Modified-Since": "Tue, 14 Nov 2023 22:13:20 GMT", "User-Agent": "remote-test/1", "Accept": "application/json"}
	agent := map[string]string{"User-Agent": "remote-test/1"}

	t.Run("200", func(t *testing.T) {
		r := testkit.NewRemote(t)
		r.Script("/a/b.jar", testkit.OK("demo bytes").With("ETag", `"v1"`))
		got := send(t, client(t), "GET", r.URL()+"/a/b.jar?x=1&y=2", all)
		if got.err != nil || got.status != 200 || got.body != "demo bytes" || got.header.Get("ETag") != `"v1"` {
			t.Fatalf("%+v", got)
		}
		sameRecord(t, r.Requests(), []testkit.Request{recorded("GET", "/a/b.jar?x=1&y=2", hostOf(r.URL()), all)})
	})
	t.Run("304", func(t *testing.T) {
		r := testkit.NewRemote(t)
		r.Script("/m.xml", testkit.NotModified())
		h := map[string]string{"If-None-Match": `"v1"`, "User-Agent": "remote-test/1"}
		got := send(t, client(t), "GET", r.URL()+"/m.xml", h)
		if got.err != nil || got.status != 304 || got.body != "" {
			t.Fatalf("%+v", got)
		}
		sameRecord(t, r.Requests(), []testkit.Request{recorded("GET", "/m.xml", hostOf(r.URL()), h)})
	})
	t.Run("404", func(t *testing.T) {
		r := testkit.NewRemote(t)
		r.Script("/gone", testkit.NotFound())
		got := send(t, client(t), "HEAD", r.URL()+"/gone", agent)
		if got.err != nil || got.status != 404 {
			t.Fatalf("%+v", got)
		}
		// a path without a script is a 404 as well, and it is recorded
		other := send(t, client(t), "GET", r.URL()+"/never/scripted", agent)
		if other.err != nil || other.status != 404 {
			t.Fatalf("%+v", other)
		}
		sameRecord(t, r.Requests(), []testkit.Request{recorded("HEAD", "/gone", hostOf(r.URL()), agent),
			recorded("GET", "/never/scripted", hostOf(r.URL()), agent)})
	})
	t.Run("500", func(t *testing.T) {
		r := testkit.NewRemote(t)
		r.Script("/broken", testkit.ServerError())
		got := send(t, client(t), "GET", r.URL()+"/broken", agent)
		if got.err != nil || got.status != 500 {
			t.Fatalf("%+v", got)
		}
		sameRecord(t, r.Requests(), []testkit.Request{recorded("GET", "/broken", hostOf(r.URL()), agent)})
	})
	t.Run("slow header", func(t *testing.T) {
		r := testkit.NewRemote(t)
		r.Script("/slow", testkit.SlowHeader(time.Minute, testkit.OK("late")))
		c := client(t)
		c.Transport.(*http.Transport).ResponseHeaderTimeout = 100 * time.Millisecond
		start := time.Now()
		got := send(t, c, "GET", r.URL()+"/slow", agent)
		if got.err == nil || !strings.Contains(got.err.Error(), "timeout") {
			t.Fatalf("%+v", got)
		}
		if waited := time.Since(start); waited > 20*time.Second {
			t.Fatalf("the header timeout ended the request after %s", waited)
		}
		sameRecord(t, r.Requests(), []testkit.Request{recorded("GET", "/slow", hostOf(r.URL()), agent)})
		// a short delay answers in full after it
		r.Script("/soon", testkit.SlowHeader(50*time.Millisecond, testkit.OK("soon")))
		start = time.Now()
		if got := send(t, client(t), "GET", r.URL()+"/soon", agent); got.err != nil || got.body != "soon" {
			t.Fatalf("%+v", got)
		} else if time.Since(start) < 50*time.Millisecond {
			t.Fatal("the header came before its delay")
		}
	})
	t.Run("slow body", func(t *testing.T) {
		r := testkit.NewRemote(t)
		r.Script("/drip", testkit.SlowBody(time.Minute, "first half second half"))
		c := client(t)
		req, err := http.NewRequest("GET", r.URL()+"/drip", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("User-Agent", "remote-test/1")
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		resp, err := c.Do(req.WithContext(ctx))
		if err != nil {
			t.Fatalf("the header of a slow body came late: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !errors.Is(err, context.DeadlineExceeded) || string(body) != "first half " {
			t.Fatalf("body %q, error %v; want the first half, then the deadline", body, err)
		}
		sameRecord(t, r.Requests(), []testkit.Request{recorded("GET", "/drip", hostOf(r.URL()), agent)})
	})
	t.Run("short body", func(t *testing.T) {
		r := testkit.NewRemote(t)
		r.Script("/short", testkit.ShortBody(100, "only ten b"))
		got := send(t, client(t), "GET", r.URL()+"/short", agent)
		if !errors.Is(got.err, io.ErrUnexpectedEOF) || got.status != 200 || got.body != "only ten b" ||
			got.header.Get("Content-Length") != "100" {
			t.Fatalf("%+v", got)
		}
		sameRecord(t, r.Requests(), []testkit.Request{recorded("GET", "/short", hostOf(r.URL()), agent)})
	})
	t.Run("redirect same host", func(t *testing.T) {
		r := testkit.NewRemote(t)
		r.Script("/old", testkit.Redirect(http.StatusFound, "/new?z=1"))
		r.Script("/new", testkit.OK("moved"))
		got := send(t, client(t), "GET", r.URL()+"/old", agent)
		if got.err != nil || got.status != 200 || got.body != "moved" {
			t.Fatalf("%+v", got)
		}
		sameRecord(t, r.Requests(), []testkit.Request{recorded("GET", "/old", hostOf(r.URL()), agent),
			recorded("GET", "/new?z=1", hostOf(r.URL()), agent)})
	})
	t.Run("redirect other host", func(t *testing.T) {
		r, other := testkit.NewRemote(t), testkit.NewRemote(t)
		for target, remote := range map[string]*testkit.Remote{other.URL(): other, r.URLAt("localhost"): r} {
			r.Script("/away", testkit.Redirect(http.StatusTemporaryRedirect, target+"/there"))
			remote.Script("/there", testkit.OK("there"))
			before, beforeOther := len(r.Requests()), len(remote.Requests())
			got := send(t, client(t), "GET", r.URL()+"/away", agent)
			if got.err != nil || got.status != 200 || got.body != "there" {
				t.Fatalf("%s: %+v", target, got)
			}
			if remote == other {
				sameRecord(t, r.Requests()[before:], []testkit.Request{recorded("GET", "/away", hostOf(r.URL()), agent)})
				sameRecord(t, other.Requests()[beforeOther:], []testkit.Request{recorded("GET", "/there", hostOf(other.URL()), agent)})
			} else {
				sameRecord(t, r.Requests()[before:], []testkit.Request{recorded("GET", "/away", hostOf(r.URL()), agent),
					recorded("GET", "/there", hostOf(target), agent)})
			}
		}
	})
	t.Run("Basic challenge", func(t *testing.T) {
		r := testkit.NewRemote(t)
		r.Script("/private", testkit.BasicChallenge("demo-user", "demo-secret", testkit.OK("private")))
		without := send(t, client(t), "GET", r.URL()+"/private", agent)
		if without.err != nil || without.status != 401 || without.header.Get("WWW-Authenticate") != `Basic realm="remote"` {
			t.Fatalf("%+v", without)
		}
		wrong := map[string]string{"User-Agent": "remote-test/1",
			"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("demo-user:other"))}
		if got := send(t, client(t), "GET", r.URL()+"/private", wrong); got.status != 401 {
			t.Fatalf("a wrong secret: %+v", got)
		}
		right := map[string]string{"User-Agent": "remote-test/1",
			"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("demo-user:demo-secret"))}
		with := send(t, client(t), "GET", r.URL()+"/private", right)
		if with.err != nil || with.status != 200 || with.body != "private" {
			t.Fatalf("%+v", with)
		}
		sameRecord(t, r.Requests(), []testkit.Request{recorded("GET", "/private", hostOf(r.URL()), agent),
			recorded("GET", "/private", hostOf(r.URL()), wrong), recorded("GET", "/private", hostOf(r.URL()), right)})
	})
	t.Run("bearer challenge", func(t *testing.T) {
		r := testkit.NewRemote(t)
		r.Script("/v2/demo/manifests/1.0", testkit.BearerChallenge("demo-token", testkit.OK("manifest")))
		without := send(t, client(t), "GET", r.URL()+"/v2/demo/manifests/1.0", agent)
		if without.err != nil || without.status != 401 || !strings.HasPrefix(without.header.Get("WWW-Authenticate"), "Bearer ") {
			t.Fatalf("%+v", without)
		}
		right := map[string]string{"User-Agent": "remote-test/1", "Authorization": "Bearer demo-token"}
		with := send(t, client(t), "GET", r.URL()+"/v2/demo/manifests/1.0", right)
		if with.err != nil || with.status != 200 || with.body != "manifest" {
			t.Fatalf("%+v", with)
		}
		sameRecord(t, r.Requests(), []testkit.Request{recorded("GET", "/v2/demo/manifests/1.0", hostOf(r.URL()), agent),
			recorded("GET", "/v2/demo/manifests/1.0", hostOf(r.URL()), right)})
	})
	t.Run("closed connection", func(t *testing.T) {
		r := testkit.NewRemote(t)
		r.Script("/cut", testkit.Closed())
		got := send(t, client(t), "GET", r.URL()+"/cut", agent)
		if got.err == nil || got.status != 0 {
			t.Fatalf("%+v", got)
		}
		sameRecord(t, r.Requests(), []testkit.Request{recorded("GET", "/cut", hostOf(r.URL()), agent)})
	})
}

// A script answers its requests in order and repeats its last answer; Count
// counts the requests of a path and Reset forgets the record.
func TestRemoteScriptOrderAndRecord(t *testing.T) {
	r := testkit.NewRemote(t)
	r.Script("/flaky", testkit.ServerError(), testkit.Closed(), testkit.OK("third"))
	c := client(t)
	var statuses []int
	for range 4 {
		statuses = append(statuses, send(t, c, "GET", r.URL()+"/flaky", nil).status)
	}
	if !reflect.DeepEqual(statuses, []int{500, 0, 200, 200}) {
		t.Fatalf("statuses %v", statuses)
	}
	if n := r.Count("/flaky"); n != 4 {
		t.Fatalf("count %d, want 4", n)
	}
	if n := r.Count("/other"); n != 0 {
		t.Fatalf("count of another path %d", n)
	}
	r.Reset()
	if len(r.Requests()) != 0 || r.Count("/flaky") != 0 {
		t.Fatalf("record after Reset: %+v", r.Requests())
	}
	// a request record holds only the six headers of a proxy, and a copy of them
	send(t, c, "GET", r.URL()+"/flaky", map[string]string{"X-Other": "kept out", "Accept": "*/*"})
	got := r.Requests()
	if len(got) != 1 || len(got[0].Header) != 2 || got[0].Header.Get("Accept") != "*/*" || got[0].Header.Get("X-Other") != "" {
		t.Fatalf("%+v", got)
	}
	got[0].Header.Set("Accept", "changed")
	if r.Requests()[0].Header.Get("Accept") != "*/*" {
		t.Fatal("Requests handed out the record itself")
	}
}

// A chunked answer has no length, and its body arrives whole.
func TestRemoteChunkedBody(t *testing.T) {
	r := testkit.NewRemote(t)
	r.Script("/chunks", testkit.Chunked("a body of unknown length"))
	req, err := http.NewRequest("GET", r.URL()+"/chunks", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "a body of unknown length" || resp.ContentLength != -1 ||
		len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
		t.Fatalf("%q %v, length %d, encoding %v", body, err, resp.ContentLength, resp.TransferEncoding)
	}
}

// A path keeps its encoded form: the script of /a%2Fb is not the one of /a/b.
func TestRemoteKeepsTheEncodedPath(t *testing.T) {
	r := testkit.NewRemote(t)
	r.Script("/a%2Fb", testkit.OK("encoded"))
	r.Script("/a/b", testkit.OK("plain"))
	c := client(t)
	if got := send(t, c, "GET", r.URL()+"/a%2Fb", nil); got.body != "encoded" {
		t.Fatalf("%+v", got)
	}
	if got := send(t, c, "GET", r.URL()+"/a/b", nil); got.body != "plain" {
		t.Fatalf("%+v", got)
	}
	if got := r.Requests(); got[0].Target != "/a%2Fb" || got[1].Target != "/a/b" {
		t.Fatalf("%+v", got)
	}
}

// A remote over TLS has a certificate no system root signs.
func TestTLSRemote(t *testing.T) {
	r := testkit.NewTLSRemote(t)
	if !strings.HasPrefix(r.URL(), "https://127.0.0.1:") {
		t.Fatalf("URL %s", r.URL())
	}
	r.Script("/x", testkit.OK("x"))
	if got := send(t, client(t), "GET", r.URL()+"/x", nil); got.err == nil {
		t.Fatalf("a client with the system roots took the test certificate: %+v", got)
	}
	if got := send(t, r.TrustingClient(), "GET", r.URL()+"/x", nil); got.err != nil || got.body != "x" {
		t.Fatalf("%+v", got)
	}
	if n := r.Count("/x"); n != 1 {
		t.Fatalf("count %d, want 1: the refused handshake sent no request", n)
	}
}

// A remote that the test leaves with a request still waiting closes at once.
func TestRemoteClosesWithAWaitingRequest(t *testing.T) {
	r := testkit.NewRemote(t)
	r.Script("/hang", testkit.SlowHeader(time.Hour, testkit.OK("never")))
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	done := make(chan error, 1)
	go func() {
		resp, err := (&http.Client{Transport: tr}).Get(r.URL() + "/hang")
		if err == nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	for r.Count("/hang") == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	r.Close()
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("Close waited %s", waited)
	}
	<-done
	r.Close() // a second Close does nothing
}
