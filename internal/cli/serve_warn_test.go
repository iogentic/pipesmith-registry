package cli

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

const proxyWarningText = "trusted_proxies is empty"

// heldPort is a loopback port this test holds, so a start on it ends at its
// bind after the warnings of the start.
func heldPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

// Review 3-20, option D as a warning: a start on a network address with
// trusted_proxies empty logs one warning that clients behind a reverse
// proxy share its address for the sign in limiter and get no Secure cookie,
// and names trusted_proxies. A loopback bind, or trusted_proxies set, logs
// none. The address of TEST-NET-1 cannot be bound, so the start ends after
// the warnings.
func TestServeWarnsOfAnEmptyTrustedProxies(t *testing.T) {
	dir := dataDir(t)
	writeState(t, dir, []state.Kind{state.KindUsers, state.KindRealms},
		&model.Snapshot{Users: []model.User{activeUser(t)}, Realms: []string{auth.RealmLocal}})
	port := fmt.Sprint(freePort(t))
	network := []string{"serve", "--port", port, "--bind", "192.0.2.1", "--server-names", "registry.test"}
	code, _, errOut := run(t, network...)
	if code != 1 || strings.Count(errOut, proxyWarningText) != 1 || !strings.Contains(errOut, "WARNING pipesmith_registry.security: ") ||
		!strings.Contains(errOut, "sign in limiter") || !strings.Contains(errOut, "Secure") {
		t.Fatalf("network bind, trusted_proxies empty: %d %q", code, errOut)
	}
	if code, _, errOut := run(t, "serve", "--port", heldPort(t)); code != 1 || strings.Contains(errOut, proxyWarningText) {
		t.Fatalf("loopback bind: %d %q", code, errOut)
	}
	saveConfig(t, func(c *config.Config) { c.TrustedProxies = []string{"192.0.2.10"} })
	if code, _, errOut := run(t, network...); code != 1 || strings.Contains(errOut, proxyWarningText) {
		t.Fatalf("network bind, trusted_proxies set: %d %q", code, errOut)
	}
}

// Owner choice 3-25: a start with a TLS key file the group or others may
// read or write logs one warning and refuses nothing; a key file of mode
// 0600 logs none.
func TestServeWarnsOfAWideTLSKeyFile(t *testing.T) {
	dataDir(t)
	certFile, keyFile, _ := tlsFiles(t)
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"serve", "--port", heldPort(t), "--tls-cert", certFile, "--tls-key", keyFile}
	code, _, errOut := run(t, args...)
	if code != 1 || strings.Count(errOut, "TLS key file") != 1 || !strings.Contains(errOut, keyFile) ||
		!strings.Contains(errOut, "0644") || !strings.Contains(errOut, "TLS on every listener") {
		t.Fatalf("key file of mode 0644: %d %q", code, errOut)
	}
	if err := os.Chmod(keyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, args...); code != 1 || strings.Contains(errOut, "TLS key file") {
		t.Fatalf("key file of mode 0600: %d %q", code, errOut)
	}
}
