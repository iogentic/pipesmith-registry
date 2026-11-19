package release

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testCert is a self signed ECDSA P-256 certificate made in the test, for a
// DNS name that is not the loopback address the wait asks.
func testCert(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "registry.example.test"},
		DNSNames: []string{"registry.example.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// writeFile writes data to a new file below a temporary directory outside
// the install root and returns its path.
func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// tlsRig is a rig whose fake server speaks TLS with a certificate made in
// the test; Address gives http:// as the command builds it from config.json.
func tlsRig(t *testing.T, current string, others ...string) (*rig, []byte) {
	t.Helper()
	r := newRig(t, current, others...)
	r.srv.Close()
	cert, certPEM := testCert(t)
	r.srv = httptest.NewUnstartedServer(r.svc)
	r.srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	r.srv.StartTLS()
	t.Cleanup(r.srv.Close)
	addr := r.srv.Listener.Addr().String()
	r.u.Address = func(*UpdateConfig) (string, error) { return "http://" + addr, nil }
	return r, certPEM
}

func TestHealthTLSUpdate(t *testing.T) {
	r, certPEM := tlsRig(t, "0.1.0")
	r.config(fmt.Sprintf(`, "health_tls_cert": %q`, writeFile(t, "server.pem", certPEM)))
	if err := r.u.Update(r.release(versionScript("0.2.0")), false); err != nil {
		t.Fatalf("update: %v\n%s", err, r.out.String())
	}
	if !strings.Contains(r.out.String(), "healthy: https://"+r.srv.Listener.Addr().String()+" answers with version 0.2.0") {
		t.Fatalf("output:\n%s", r.out.String())
	}
}

// The certificate of config.json, given by HealthCert, is used without the
// key of update.json.
func TestHealthTLSOfConfig(t *testing.T) {
	r, certPEM := tlsRig(t, "0.1.0")
	path := writeFile(t, "server.pem", certPEM)
	r.u.HealthCert = func(*UpdateConfig) (string, error) { return path, nil }
	if err := r.u.Update(r.release(versionScript("0.2.0")), false); err != nil {
		t.Fatalf("update: %v\n%s", err, r.out.String())
	}
	if !strings.Contains(r.out.String(), "healthy: https://") {
		t.Fatalf("output:\n%s", r.out.String())
	}
}

func TestHealthTLSOtherCert(t *testing.T) {
	r, _ := tlsRig(t, "0.1.0")
	_, otherPEM := testCert(t)
	other := writeFile(t, "other.pem", otherPEM)
	r.config(fmt.Sprintf(`, "health_tls_cert": %q`, other))
	r.u.HealthTimeout = 300 * time.Millisecond
	before := tree(t, r.l.Root)
	err := r.u.Update(r.release(versionScript("0.2.0")), false)
	r.wayBack(err, ErrHealth, before, "")
	if !strings.Contains(err.Error(), "the server presents another certificate than "+other) {
		t.Fatalf("message %q", err)
	}
}

// The wait of 0.2.0 fails, the way back to 0.1.0 answers over TLS.
func TestHealthTLSWayBack(t *testing.T) {
	r, certPEM := tlsRig(t, "0.1.0")
	r.config(fmt.Sprintf(`, "health_tls_cert": %q`, writeFile(t, "server.pem", certPEM)))
	r.u.HealthTimeout = 300 * time.Millisecond
	r.svc.unhealthy["0.2.0"] = true
	before := tree(t, r.l.Root)
	err := r.u.Update(r.release(versionScript("0.2.0")), false)
	r.wayBack(err, ErrHealth, before, "")
	if !strings.Contains(err.Error(), "which answers again") || !strings.Contains(r.out.String(), "healthy: https://") {
		t.Fatalf("message %q\n%s", err, r.out.String())
	}
}

func TestHealthTLSRollback(t *testing.T) {
	r, certPEM := tlsRig(t, "0.2.0", "0.1.0")
	if err := os.Symlink("releases/0.1.0", filepath.Join(r.l.Root, PreviousLink)); err != nil {
		t.Fatal(err)
	}
	r.config(fmt.Sprintf(`, "health_tls_cert": %q`, writeFile(t, "server.pem", certPEM)))
	if err := r.u.Rollback(); err != nil {
		t.Fatalf("rollback: %v\n%s", err, r.out.String())
	}
	if !strings.Contains(r.out.String(), "healthy: https://"+r.srv.Listener.Addr().String()+" answers with version 0.1.0") {
		t.Fatalf("output:\n%s", r.out.String())
	}
}

// A certificate is named and the server speaks plain HTTP.
func TestHealthTLSPlainServer(t *testing.T) {
	r := newRig(t, "0.1.0")
	_, certPEM := testCert(t)
	r.config(fmt.Sprintf(`, "health_tls_cert": %q`, writeFile(t, "server.pem", certPEM)))
	r.u.HealthTimeout = 300 * time.Millisecond
	before := tree(t, r.l.Root)
	err := r.u.Update(r.release(versionScript("0.2.0")), false)
	r.wayBack(err, ErrHealth, before, "")
	if !strings.Contains(err.Error(), "the server did not speak TLS") {
		t.Fatalf("message %q", err)
	}
}

// No certificate is named and the server speaks TLS: the wait fails as
// before.
func TestHealthTLSNotNamed(t *testing.T) {
	r, _ := tlsRig(t, "0.1.0")
	r.u.HealthTimeout = 300 * time.Millisecond
	before := tree(t, r.l.Root)
	err := r.u.Update(r.release(versionScript("0.2.0")), false)
	r.wayBack(err, ErrHealth, before, "")
	if !strings.Contains(err.Error(), "at http://") {
		t.Fatalf("message %q", err)
	}
}

func TestHealthTLSRefusals(t *testing.T) {
	_, keyPEM := testCert(t)
	keyPEM = []byte(strings.ReplaceAll(string(keyPEM), "CERTIFICATE", "PUBLIC KEY"))
	for name, path := range map[string]string{
		"missing":     filepath.Join(t.TempDir(), "absent.pem"),
		"empty":       writeFile(t, "empty.pem", nil),
		"no block":    writeFile(t, "key.pem", keyPEM),
		"not PEM":     writeFile(t, "text.pem", []byte("not a certificate\n")),
		"bad DER":     writeFile(t, "bad.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")})),
		"too large":   writeFile(t, "large.pem", make([]byte, maxFile+1)),
		"a directory": t.TempDir(),
	} {
		t.Run(name, func(t *testing.T) {
			for _, viaConfig := range []bool{false, true} {
				r, _ := tlsRig(t, "0.1.0")
				key := "health_tls_cert of " + ConfigFile
				if viaConfig {
					r.u.HealthCert = func(*UpdateConfig) (string, error) { return path, nil }
					key = "tls_cert of config.json"
				} else {
					r.config(fmt.Sprintf(`, "health_tls_cert": %q`, path))
				}
				dir := r.release(versionScript("0.2.0"))
				err := r.unchanged(ErrConfig, func() error { return r.u.Update(dir, false) })
				if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), key) {
					t.Fatalf("message %q, want %s and %s", err, path, key)
				}
				err = r.unchanged(ErrConfig, func() error { return r.u.Rollback() })
				if !strings.Contains(err.Error(), key) {
					t.Fatalf("rollback: %q", err)
				}
			}
		})
	}
}

// health_tls_cert wins over the certificate of config.json, and a relative
// path is taken relative to the root.
func TestHealthTarget(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `{"health_tls_cert": "tls/server.pem", "health_address": "127.0.0.1:14771"}`)
	cfg, err := LoadUpdateConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	u := &Updater{Root: root, HealthCert: func(*UpdateConfig) (string, error) { return "/other.pem", nil }}
	base, cert, key, err := u.HealthTarget(cfg)
	if err != nil || base != "https://127.0.0.1:14771" || cert != filepath.Join(root, "tls", "server.pem") || key != "health_tls_cert of "+ConfigFile {
		t.Fatalf("%q %q %q %v", base, cert, key, err)
	}
	cfg.HealthTLSCert = ""
	base, cert, key, err = u.HealthTarget(cfg)
	if err != nil || base != "https://127.0.0.1:14771" || cert != "/other.pem" || key != "tls_cert of config.json" {
		t.Fatalf("config.json: %q %q %q %v", base, cert, key, err)
	}
	u.HealthCert = func(*UpdateConfig) (string, error) { return "", nil }
	if base, cert, _, err = u.HealthTarget(cfg); err != nil || base != "http://127.0.0.1:14771" || cert != "" {
		t.Fatalf("plain: %q %q %v", base, cert, err)
	}
	u.HealthCert = func(*UpdateConfig) (string, error) { return "", errors.New("config.json unreadable") }
	if _, _, _, err = u.HealthTarget(cfg); err == nil {
		t.Fatal("the error of HealthCert was dropped")
	}
}
