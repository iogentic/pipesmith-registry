package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// helperURL names the remote of TestHelperTLS; the helper skips without it.
const helperURL = "PSR_TLS_HELPER_URL"

// The positive case of TLS (report 5a, section Open; decision 5.14): a
// process whose SSL_CERT_FILE names the certificate of the TLS remote gets
// 200 through the client, and the same request without the variable is
// refused. The system roots read the variable once per process, so each
// case is a helper process of the test binary. No option of the client adds
// a root or skips the check.
func TestClientTrustsTheCertificateFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the system roots of Linux read SSL_CERT_FILE; the tests run in the Linux container of tools/go.sh")
	}
	remote := testkit.NewTLSRemote(t)
	remote.Script("/files/over-tls.txt", testkit.OK("bytes over TLS"))
	certificate := filepath.Join(t.TempDir(), "remote.pem")
	if err := os.WriteFile(certificate, remote.CertificatePEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SSL_CERT_FILE=") && !strings.HasPrefix(kv, "SSL_CERT_DIR=") {
			env = append(env, kv)
		}
	}
	for _, c := range []struct {
		name  string
		extra []string
		want  string
	}{
		{"with SSL_CERT_FILE", []string{"SSL_CERT_FILE=" + certificate}, "answer 200 bytes over TLS"},
		{"without it", nil, "refused: "},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperTLS$", "-test.v")
		cmd.Env = append(append(append([]string(nil), env...), helperURL+"="+remote.URL()+"/"), c.extra...)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), c.want) {
			t.Fatalf("%s: %v\n%s", c.name, err, out)
		}
		if c.extra == nil && !strings.Contains(string(out), "certificate") {
			t.Fatalf("%s: refused for another reason:\n%s", c.name, out)
		}
	}
	if n := remote.Count("/files/over-tls.txt"); n != 1 {
		t.Fatalf("%d requests reached the remote, want the one of the trusting process", n)
	}
}

// TestHelperTLS is the helper process of TestClientTrustsTheCertificateFile.
func TestHelperTLS(t *testing.T) {
	base := os.Getenv(helperURL)
	if base == "" {
		t.Skip("a helper process of TestClientTrustsTheCertificateFile")
	}
	c, err := NewClient(Options{Repository: "demo-tls-proxy", Settings: repos.ProxySettings{RemoteURL: base, TimeoutSeconds: 5}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := c.Do(ctx, http.MethodGet, "files/over-tls.txt", "", nil)
	if err != nil {
		fmt.Printf("refused: %v\n", err)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("answer %d %s\n", resp.StatusCode, body)
}
