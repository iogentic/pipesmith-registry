package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// The line of each request this server sends to a remote (task 16 of the
// phase 5 plan): repository, method, scheme, host and port of the address
// dialled, status or kind of failure, duration. No path, no query, no
// credential. tools/check_outgoing.py reads these lines.

// records keeps level, logger and message of every record.
type records struct {
	mu   sync.Mutex
	seen []slog.Record
}

func (r *records) Enabled(context.Context, slog.Level) bool { return true }
func (r *records) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, rec.Clone())
	return nil
}
func (r *records) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *records) WithGroup(string) slog.Handler      { return r }

// outgoing are the records of outgoing requests: their messages, after a
// check of level and logger.
func (r *records) outgoing(t *testing.T) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var lines []string
	for _, rec := range r.seen {
		if !strings.HasPrefix(rec.Message, "outgoing request: ") {
			continue
		}
		name := ""
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "logger" {
				name = a.Value.String()
			}
			return true
		})
		if rec.Level != slog.LevelInfo || name != logger {
			t.Errorf("level %s, logger %q: %q", rec.Level, name, rec.Message)
		}
		lines = append(lines, rec.Message)
	}
	return lines
}

var outgoingLine = regexp.MustCompile(`^outgoing request: repository "([^"]+)", (GET|HEAD) (http|https) host "([^"]+)" port (\d+), ` +
	`(status \d{3}|failed [a-z ()]+), \d+ ms$`)

func portOf(t *testing.T, address string) string {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

// Each hop of a redirect is one line with the address dialled, the path,
// the query and the credentials stay out.
func TestOutgoingLinePerHop(t *testing.T) {
	first, second := testkit.NewRemote(t), testkit.NewRemote(t)
	first.Script("/base/docs/secret-path.txt", testkit.Redirect(http.StatusFound, second.URLAt("localhost")+"/moved/secret-path.txt?other=q"))
	second.Script("/moved/secret-path.txt", testkit.OK("moved"))
	log := &records{}
	settings := settingsOf(first)
	settings.Auth = repos.RemoteAuth{Type: repos.AuthBasic, Username: "demo-user", Preemptive: true}
	c := newClient(t, Options{Repository: "demo-raw-proxy", Settings: settings, Secret: canaryPassword, Logger: slog.New(log)})
	resp, err := c.Do(context.Background(), http.MethodGet, "/docs/secret-path.txt", "token=query-value", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	lines := log.outgoing(t)
	want := []string{
		fmt.Sprintf(`repository "demo-raw-proxy", GET http host "127.0.0.1" port %s, status 302, `, portOf(t, first.URL())),
		fmt.Sprintf(`repository "demo-raw-proxy", GET http host "localhost" port %s, status 200, `, portOf(t, second.URL())),
	}
	if len(lines) != len(want) {
		t.Fatalf("%d lines, want %d: %q", len(lines), len(want), lines)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("demo-user:" + canaryPassword))
	for i, line := range lines {
		if !outgoingLine.MatchString(line) || !strings.Contains(line, want[i]) {
			t.Errorf("line %d: %q, want %q", i, line, want[i])
		}
		for _, secret := range []string{"secret-path", "moved", "query-value", "other=q", canaryPassword, encoded, "demo-user"} {
			if strings.Contains(line, secret) {
				t.Errorf("line %d holds %q: %q", i, secret, line)
			}
		}
	}
}

// A request that gets no answer is one line with the kind of its failure;
// every attempt of a retry is a line of its own.
func TestOutgoingLineOfAFailure(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "http://" + closed.Addr().String() + "/"
	_ = closed.Close()
	log := &records{}
	c := newClient(t, Options{Settings: repos.ProxySettings{RemoteURL: address, TimeoutSeconds: 1, Retries: 1}, Logger: slog.New(log)})
	if got := get(t, c, "/a.txt", nil); got.err == nil {
		t.Fatalf("an answer from a closed port: %+v", got)
	}
	lines := log.outgoing(t)
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2 for 2 attempts: %q", len(lines), lines)
	}
	for _, line := range lines {
		if !outgoingLine.MatchString(line) || !strings.Contains(line, "failed connection") {
			t.Errorf("%q", line)
		}
	}

	log = &records{}
	c = newClient(t, Options{Settings: repos.ProxySettings{RemoteURL: "http://127.0.0.1:9/", TimeoutSeconds: 1},
		dialer: blockingDialer{}, Logger: slog.New(log)})
	if got := get(t, c, "/a.txt", nil); got.err == nil {
		t.Fatalf("an answer through a dialer that never connects: %+v", got)
	}
	if lines := log.outgoing(t); len(lines) != 1 || !outgoingLine.MatchString(lines[0]) ||
		!strings.Contains(lines[0], `host "127.0.0.1" port 9, failed timeout`) {
		t.Fatalf("%q", lines)
	}
}

// An address the rule of inner addresses refuses is never dialled, and its
// line says so (ruling P5-8).
func TestOutgoingLineOfARefusedAddress(t *testing.T) {
	remote := testkit.NewRemote(t)
	remote.Script("/base/away", testkit.Redirect(http.StatusFound, "http://0.0.0.0:9/a"))
	log := &records{}
	c := newClient(t, Options{Settings: settingsOf(remote), Logger: slog.New(log)})
	if got := get(t, c, "/away", nil); got.err == nil {
		t.Fatalf("%+v", got)
	}
	lines := log.outgoing(t)
	if len(lines) != 2 || !strings.HasSuffix(strings.SplitN(lines[0], ", ", 4)[2], "status 302") ||
		!strings.Contains(lines[1], `host "0.0.0.0" port 9, failed not dialled (inner address)`) {
		t.Fatalf("%q", lines)
	}
	for _, line := range lines {
		if !outgoingLine.MatchString(line) {
			t.Errorf("%q", line)
		}
	}
}

// The token flow of a Docker proxy asks the realm through the same client:
// the realm request is a line too.
func TestOutgoingLineOfARealm(t *testing.T) {
	registry, realm := testkit.NewRemote(t), testkit.NewRemote(t)
	registry.Script("/base/v2/demo/app/manifests/1.0",
		testkit.Answer{Status: http.StatusUnauthorized, Header: http.Header{"Www-Authenticate": {
			fmt.Sprintf(`Bearer realm="%s/token",service="demo"`, realm.URL())}}},
		testkit.OK("{}"))
	realm.Script("/token", testkit.OK(`{"token":"demo-token-value"}`))
	log := &records{}
	c := newClient(t, Options{Settings: settingsOf(registry), Logger: slog.New(log), tokens: true})
	got := get(t, c, "/v2/demo/app/manifests/1.0", nil)
	if got.err != nil || got.resp.StatusCode != http.StatusOK {
		t.Fatalf("%+v", got)
	}
	lines := log.outgoing(t)
	ports := []string{portOf(t, registry.URL()), portOf(t, realm.URL()), portOf(t, registry.URL())}
	statuses := []string{"status 401", "status 200", "status 200"}
	if len(lines) != 3 {
		t.Fatalf("%d lines, want 3: %q", len(lines), lines)
	}
	for i, line := range lines {
		if !outgoingLine.MatchString(line) || !strings.Contains(line, "port "+ports[i]+", "+statuses[i]+",") ||
			strings.Contains(line, "demo-token-value") || strings.Contains(line, "demo/app") {
			t.Errorf("line %d: %q", i, line)
		}
	}
}
