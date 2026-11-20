package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// The log lines of the proxies (fix part B of leg 5b, after the security
// assessment of the leg): a path a client chose, and the text of an error
// that a remote may have chosen, are quoted, so they cannot start a new log
// line or pass as another field.

// lines records the messages of a logger.
type lines struct {
	mu   sync.Mutex
	text []string
}

func (l *lines) Enabled(context.Context, slog.Level) bool { return true }
func (l *lines) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.text = append(l.text, r.Message)
	return nil
}
func (l *lines) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *lines) WithGroup(string) slog.Handler      { return l }

func (l *lines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.text...)
}

// failingState is a proxy_state whose reads and writes fail.
type failingState struct{}

func (failingState) ProxyState(string, string) (catalog.ProxyState, error) {
	return catalog.ProxyState{}, errors.New("the table is gone")
}
func (failingState) PutProxyRow(catalog.ProxyRow) error  { return errors.New("the table is gone") }
func (failingState) ResetProxyState(string, int64) error { return errors.New("the table is gone") }

// rowsThatFail is a proxy_state that reads no row and fails every write.
type rowsThatFail struct{ failingState }

func (rowsThatFail) ProxyState(string, string) (catalog.ProxyState, error) {
	return catalog.ProxyState{}, nil
}

func TestLogLinesQuoteWhatClientsAndRemotesChose(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "confirmed.txt") {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer remote.Close()
	repo := repos.Repo{Name: "demo-raw-proxy", Format: "raw", Type: "proxy", Attributes: map[string]any{
		"proxy":      map[string]any{"remoteUrl": remote.URL + "/", "contentMaxAge": 0},
		"httpclient": map[string]any{"autoBlock": false, "connection": map[string]any{"retries": 0}}}}
	const injected = "\nWARNING pipesmith_registry.proxy: forged\r\x1b[2K"
	cases := []struct {
		name  string
		state StateStore
		path  string
	}{
		{"the warning of a failed fetch", nil, "docs/failed" + injected + ".txt"},
		{"the warning of a row that is not read", failingState{}, "docs/unread" + injected + ".txt"},
		{"the warning of a row that is not written", rowsThatFail{}, "docs/confirmed" + injected + "confirmed.txt"},
	}
	for _, c := range cases {
		log := &lines{}
		p := New(Config{State: c.state, Logger: slog.New(log)})
		q := Query{Repo: repo, Path: c.path, Cached: &Cached{CreatedMS: 0}, Look: func() (*Cached, error) { return &Cached{}, nil }}
		if _, err := p.Get(context.Background(), q); err != nil {
			t.Fatal(err)
		}
		p.Close()
		got := log.all()
		if len(got) == 0 {
			t.Fatalf("%s: no log line", c.name)
		}
		for _, line := range got {
			if strings.ContainsFunc(line, unicode.IsControl) {
				t.Errorf("%s: a control character in the log line %q", c.name, line)
			}
			if strings.Contains(line, "docs/") && !strings.Contains(line, `"`+strings.Split(c.path, "\n")[0]) {
				t.Errorf("%s: the path is not quoted in %q", c.name, line)
			}
		}
	}
}

// A fetch that panics answers 502, and its panic is logged once, with the
// repository and the path quoted (carry over part a of leg 5c): the warning
// of a failed fetch never comes, as the fetch did not return.
func TestAPanicOfAFetchIsLoggedOnce(t *testing.T) {
	remote := testkit.NewRemote(t)
	remote.Script("/raw/docs/panic.txt", testkit.OK("the bytes of the remote"))
	repo := repos.Repo{Name: "demo-raw-proxy", Format: "raw", Type: "proxy", Attributes: map[string]any{
		"proxy":      map[string]any{"remoteUrl": remote.URL() + "/raw/"},
		"httpclient": map[string]any{"autoBlock": false, "connection": map[string]any{"retries": 0}}}}
	log := &lines{}
	p := New(Config{Logger: slog.New(log)})
	defer p.Close()
	q := Query{Repo: repo, Path: "docs/panic.txt", Store: func(f Fetched) error {
		_, _ = io.Copy(io.Discard, f.Body)
		panic("the store broke\nWARNING pipesmith_registry.proxy: forged")
	}}
	outcome, err := p.Get(context.Background(), q)
	if err != nil || outcome != Failed {
		t.Fatalf("outcome %v, error %v, want failed", outcome, err)
	}
	var named []string
	for _, line := range log.all() {
		if strings.ContainsFunc(line, unicode.IsControl) {
			t.Errorf("a control character in the log line %q", line)
		}
		if strings.Contains(line, "panic.txt") {
			named = append(named, line)
		}
	}
	if len(named) != 1 {
		t.Fatalf("%d log lines name the path, want 1: %q", len(named), log.all())
	}
	for _, want := range []string{`"demo-raw-proxy"`, `"docs/panic.txt"`, `the store broke\nWARNING`} {
		if !strings.Contains(named[0], want) {
			t.Errorf("no %s in the log line %q", want, named[0])
		}
	}
	if n := remote.Count("/raw/docs/panic.txt"); n != 1 {
		t.Fatalf("%d requests at the remote, want 1", n)
	}
}

// The text of an error that a remote chose reaches the log quoted (carry over
// part b of leg 5c, the open point of fix part B of leg 5b). The TLS remote
// names itself in its certificate with a line break in the name; the client
// asks it as localhost, and the host name error of crypto/x509 joins the
// names of the certificate into its text as they are.
func TestAnErrorTextOfARemoteIsQuotedInTheLog(t *testing.T) {
	const name = "demo-remote.example\nWARNING pipesmith_registry.proxy: forged"
	remote := testkit.NewTLSRemoteNamed(t, name)
	remote.Script("/raw/docs/guide.txt", testkit.OK("never sent"))
	repo := repos.Repo{Name: "demo-tls-proxy", Format: "raw", Type: "proxy", Attributes: map[string]any{
		"proxy":      map[string]any{"remoteUrl": remote.URLAt("localhost") + "/raw/"},
		"httpclient": map[string]any{"autoBlock": false, "connection": map[string]any{"retries": 1, "timeout": 5}}}}
	log := &lines{}
	p := New(Config{Logger: slog.New(log)})
	defer p.Close()
	outcome, err := p.Get(context.Background(), Query{Repo: repo, Path: "docs/guide.txt", Store: func(Fetched) error { return nil }})
	if err != nil || outcome != Failed {
		t.Fatalf("outcome %v, error %v, want failed", outcome, err)
	}
	quoted := 0
	for _, line := range log.all() {
		if strings.ContainsFunc(line, unicode.IsControl) {
			t.Errorf("a control character in the log line %q", line)
		}
		if strings.Contains(line, `demo-remote.example\nWARNING`) {
			quoted++
		}
	}
	if quoted == 0 {
		t.Fatalf("no log line holds the name of the certificate quoted: %q", log.all())
	}
	if n := len(remote.Requests()); n != 0 {
		t.Fatalf("%d requests reached the remote past the refused certificate", n)
	}
}
