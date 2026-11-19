package cli

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Process values of config.json for running in place of the old service:
// tls_cert and tls_key (decision E9 of the execution plan) and
// trusted_proxies (E8). Each shows in config.json and in config --json only
// while it differs from its default.

// referenceKeys are the keys of config --json at the defaults, those of the
// reference.
var referenceKeys = []string{"bind", "config_path", "data_dir", "docker_ports", "index_path", "port", "repositories_path", "stores"}

func shownKeys(t *testing.T) (map[string]any, []string) {
	t.Helper()
	code, out, errOut := run(t, "config", "--json")
	var shown map[string]any
	if err := json.Unmarshal([]byte(out), &shown); code != 0 || err != nil {
		t.Fatalf("config --json: %d %v %q", code, err, errOut)
	}
	var keys []string
	for k := range shown {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return shown, keys
}

func saveConfig(t *testing.T, change func(*config.Config)) {
	t.Helper()
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	change(cfg)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestConfigShowsTrustedProxies(t *testing.T) {
	dir := dataDir(t)
	if _, keys := shownKeys(t); !slices.Equal(keys, referenceKeys) {
		t.Fatalf("default keys %q, want %q", keys, referenceKeys)
	}
	saveConfig(t, func(c *config.Config) { c.TrustedProxies = []string{"10.0.0.0/8", "192.0.2.7"} })
	shown, keys := shownKeys(t)
	if want := append(slices.Clone(referenceKeys), "trusted_proxies"); !slices.Equal(keys, slices.Sorted(slices.Values(want))) {
		t.Fatalf("keys %q", keys)
	}
	if list, _ := shown["trusted_proxies"].([]any); len(list) != 2 || list[0] != "10.0.0.0/8" || list[1] != "192.0.2.7" {
		t.Fatalf("trusted_proxies %v", shown["trusted_proxies"])
	}
	_, out, _ := run(t, "config")
	if line := fmt.Sprintf("%-17s  %s\n", "trusted_proxies", "10.0.0.0/8, 192.0.2.7"); !strings.Contains(out, line) {
		t.Fatalf("config shows %q, want the line %q", out, line)
	}
	// a command that writes config.json keeps the list
	tree := demoTree(t, dir+"-trees")
	if code, _, errOut := run(t, "store", "add", "tree", "--kind", "directory", "--root", tree); code != 0 {
		t.Fatalf("store add: %d %q", code, errOut)
	}
	data, err := os.ReadFile(dir + "/config.json")
	if err != nil || !strings.Contains(string(data), "\"trusted_proxies\": [\n    \"10.0.0.0/8\",\n    \"192.0.2.7\"\n  ]") {
		t.Fatalf("config.json after store add: %q %v", data, err)
	}
}

// buildSecurity hands the list to the identity code: the sign in limits count
// per client behind a trusted proxy.
func TestSecurityTakesTrustedProxies(t *testing.T) {
	dir := importedFixture(t)
	var log bytes.Buffer
	info := slog.New(fieldHandler{newLogHandler(&log, true)})
	sec, err := buildSecurity(dir, auth.TrustedProxies{netip.MustParsePrefix("10.0.0.0/8")}, nil, info)
	if err != nil || sec == nil {
		t.Fatalf("%v %v", sec, err)
	}
	via := func(client, pass string) error {
		r := basicRequest("admin", pass)
		r.RemoteAddr = "10.0.0.1:50000"
		r.Header.Set("X-Forwarded-For", client)
		_, err := sec.Subject(r)
		return err
	}
	for i := range auth.FreeFailures {
		if err := via("203.0.113.9", "example-admin-2"); !errors.Is(err, model.ErrUnauthorized) {
			t.Fatalf("failure %d: %v", i+1, err)
		}
	}
	var me *model.Error
	if err := via("203.0.113.9", "example-admin-1"); !errors.As(err, &me) || me.Status != http.StatusTooManyRequests {
		t.Fatalf("the throttled client: %v", err)
	}
	if err := via("203.0.113.10", "example-admin-1"); err != nil {
		t.Fatalf("another client behind the proxy: %v", err)
	}
	if !strings.Contains(log.String(), "forwarded headers count from the trusted proxies 10.0.0.0/8") {
		t.Fatalf("log %q", log.String())
	}
}

// serve reads trusted_proxies from config.json: behind a proxy that says
// https the session cookie is Secure.
func TestServeTrustedProxyCookie(t *testing.T) {
	importedFixture(t)
	saveConfig(t, func(c *config.Config) { c.TrustedProxies = []string{"127.0.0.1"} })
	// loopback is a trusted proxy and not in FORWARDED_ALLOW_IPS: the URLs
	// the server builds take https from trusted_proxies alone (review S3)
	t.Setenv("FORWARDED_ALLOW_IPS", "192.0.2.1")
	base, stop := serveData(t)
	noFollow := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, _ := callWith(t, noFollow, "GET", base+"/api/health/", nil, map[string]string{"X-Forwarded-Proto": "https"}); resp.StatusCode != 307 ||
		!strings.HasPrefix(resp.Header.Get("Location"), "https://") {
		t.Fatalf("redirect behind the trusted proxy: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	header := map[string]string{"Content-Type": "application/json", "X-Forwarded-Proto": "https"}
	resp, _ := call(t, "POST", base+"/api/session", strings.NewReader(`{"username":"admin","password":"example-admin-1"}`), header)
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			cookie = c
		}
	}
	if resp.StatusCode != 200 || cookie == nil || !cookie.Secure {
		t.Fatalf("sign in behind the trusted proxy: %d %+v", resp.StatusCode, cookie)
	}
	log := strings.Join(stop(), "\n")
	if !strings.Contains(log, "forwarded headers count from the trusted proxies 127.0.0.1") || strings.Contains(log, "no trusted proxy") {
		t.Fatalf("log %q", log)
	}
	// a loopback bind without TLS warns of nothing
	if strings.Contains(log, "clear text") || strings.Contains(log, "TLS") {
		t.Fatalf("log %q", log)
	}
}

// Review D1: forwarded headers from a peer that is no trusted proxy give one
// warning that names the peer and no header value.
func TestServeWarnsOfForwardedHeadersFromAnUntrustedPeer(t *testing.T) {
	importedFixture(t)
	base, stop := serveData(t)
	for range 3 {
		call(t, "GET", base+"/api/health", nil, map[string]string{"X-Forwarded-For": "203.0.113.250"})
	}
	var warnings []string
	for _, line := range stop() {
		if strings.Contains(line, "no trusted proxy") {
			warnings = append(warnings, line)
		}
	}
	// the access line names the client of X-Forwarded-For, as uvicorn does
	// for FORWARDED_ALLOW_IPS; the warning names the peer only
	if len(warnings) != 1 || !strings.Contains(warnings[0], "WARNING") ||
		!strings.Contains(warnings[0], "forwarded headers from 127.0.0.1") || strings.Contains(warnings[0], "203.0.113.250") {
		t.Fatalf("warnings %q", warnings)
	}
}

// Decision E9 of the execution plan: serve --tls-cert FILE --tls-key FILE,
// or tls_cert and tls_key of config.json, put TLS on every listener.

func tlsFiles(t *testing.T) (certFile, keyFile string, client *http.Client) {
	t.Helper()
	certFile, keyFile, pool, err := testkit.TLSPair(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, &http.Client{Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

func canBind(port int) bool {
	l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// Over the TLS listener the session cookie is Secure (review 6, finding 4).
func TestServeTLS(t *testing.T) {
	importedFixture(t)
	certFile, keyFile, client := tlsFiles(t)
	base, stop := serveWith(t, client, "https", "--tls-cert", certFile, "--tls-key", keyFile)
	resp, _ := callWith(t, client, "POST", base+"/api/session", strings.NewReader(`{"username":"admin","password":"example-admin-1"}`),
		map[string]string{"Content-Type": "application/json"})
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			cookie = c
		}
	}
	if resp.StatusCode != 200 || resp.TLS == nil || cookie == nil || !cookie.Secure || !cookie.HttpOnly {
		t.Fatalf("sign in over TLS: %d %v %+v", resp.StatusCode, resp.TLS, cookie)
	}
	if resp, err := (&http.Client{Timeout: 10 * time.Second}).Get("http" + strings.TrimPrefix(base, "https") + "/api/health"); err == nil {
		resp.Body.Close()
		t.Fatalf("plain HTTP on the TLS port: %d", resp.StatusCode)
	}
	log := strings.Join(stop(), "\n")
	if !strings.Contains(log, "INFO pipesmith_registry.listeners: TLS on every listener with the certificate "+certFile) ||
		strings.Contains(log, "clear text") {
		t.Fatalf("log %q", log)
	}
}

func TestServeTLSFromConfig(t *testing.T) {
	dataDir(t)
	certFile, keyFile, client := tlsFiles(t)
	saveConfig(t, func(c *config.Config) { c.TLSCert, c.TLSKey = certFile, keyFile })
	shown, _ := shownKeys(t)
	if shown["tls_cert"] != certFile || shown["tls_key"] != keyFile {
		t.Fatalf("config --json %v", shown)
	}
	_, out, _ := run(t, "config")
	for _, kv := range [][2]string{{"tls_cert", certFile}, {"tls_key", keyFile}} {
		if line := fmt.Sprintf("%-17s  %s\n", kv[0], kv[1]); !strings.Contains(out, line) {
			t.Fatalf("config shows %q, want the line %q", out, line)
		}
	}
	if code, out, _ := run(t, "config", "--check"); code != 0 || out != "" {
		t.Fatalf("config --check: %d %q", code, out)
	}
	base, stop := serveWith(t, client, "https")
	if resp, _ := callWith(t, client, "GET", base+"/api/health", nil, nil); resp.StatusCode != 200 || resp.TLS == nil {
		t.Fatalf("health over TLS: %d %v", resp.StatusCode, resp.TLS)
	}
	stop()
}

// Every refusal of the TLS files comes before a bind.
func TestServeTLSRefusals(t *testing.T) {
	dataDir(t)
	certFile, keyFile, _ := tlsFiles(t)
	missing := certFile + ".missing"
	port := fmt.Sprint(freePort(t))
	half := "error: TLS needs a certificate and a key: give --tls-cert and --tls-key, or tls_cert and tls_key in config.json\n"
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--tls-cert", certFile}, half},
		{[]string{"--tls-key", keyFile}, half},
		{[]string{"--tls-cert", certFile, "--tls-key", ""}, half},
		{[]string{"--tls-cert", certFile, "--tls-key", missing},
			"error: TLS certificate " + certFile + " with key " + missing + ": open " + missing + ": no such file or directory\n"},
		{[]string{"--tls-cert", keyFile, "--tls-key", certFile}, "error: TLS certificate " + keyFile + " with key " + certFile + ": "},
	} {
		code, out, errOut := run(t, append([]string{"serve", "--port", port}, c.args...)...)
		if code != 1 || out != "" || !strings.HasPrefix(errOut, c.want) {
			t.Errorf("%q: %d %q %q, want %q", c.args, code, out, errOut, c.want)
		}
	}
	saveConfig(t, func(c *config.Config) { c.TLSCert = certFile })
	if code, _, errOut := run(t, "serve", "--port", port); code != 1 || errOut != half {
		t.Errorf("tls_cert alone in config.json: %d %q", code, errOut)
	}
	code, out, _ := run(t, "config", "--check")
	if want := "TLS needs a certificate and a key: give --tls-cert and --tls-key, or tls_cert and tls_key in config.json\n"; code != 1 || out != want {
		t.Errorf("config --check with tls_cert alone: %d %q", code, out)
	}
	saveConfig(t, func(c *config.Config) { c.TLSKey = missing })
	code, out, _ = run(t, "config", "--check")
	if want := "TLS certificate " + certFile + " with key " + missing + ": open " + missing + ": no such file or directory\n"; code != 1 || out != want {
		t.Errorf("config --check with a missing key: %d %q", code, out)
	}
	p, _ := strconv.Atoi(port)
	if !canBind(p) {
		t.Fatal("a refused start bound its port")
	}
}

// Hardening line 14 of the phase 3 plan: a bind other than loopback without
// TLS logs one warning that names Basic credentials in clear text. The
// address of TEST-NET-1 cannot be bound, so the start ends after the warning.
func TestServeWarnsOfClearText(t *testing.T) {
	dir := dataDir(t)
	writeState(t, dir, []state.Kind{state.KindUsers, state.KindRealms},
		&model.Snapshot{Users: []model.User{activeUser(t)}, Realms: []string{auth.RealmLocal}})
	port := fmt.Sprint(freePort(t))
	code, _, errOut := run(t, "serve", "--port", port, "--bind", "192.0.2.1", "--server-names", "registry.test")
	want := "WARNING pipesmith_registry.security: bind address 192.0.2.1 is not local and TLS is off: Basic credentials and " +
		"session cookies cross the network in clear text; give --tls-cert and --tls-key, or bind to loopback behind a " +
		"reverse proxy on this host that ends TLS"
	if code != 1 || strings.Count(errOut, "clear text") != 1 || !strings.Contains(errOut, want) {
		t.Fatalf("%d %q", code, errOut)
	}
	if warning, failure := strings.Index(errOut, "clear text"), strings.LastIndex(errOut, "\nerror: "); failure < 0 || warning > failure {
		t.Fatalf("the warning must come before the failed bind: %q", errOut)
	}
	certFile, keyFile, _ := tlsFiles(t)
	code, _, errOut = run(t, "serve", "--port", port, "--bind", "192.0.2.1", "--server-names", "registry.test",
		"--tls-cert", certFile, "--tls-key", keyFile)
	if code != 1 || strings.Contains(errOut, "clear text") || !strings.Contains(errOut, "TLS on every listener") {
		t.Fatalf("with TLS: %d %q", code, errOut)
	}
}
