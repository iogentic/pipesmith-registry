package channel

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// The vectors of tests/release/release_proof.py fixtures: bundles that
// cosign 3.1.4 made with a test key, with cases.json beside them.
const vectors = "../release/testdata"

// serveVector copies the case folder name of the vectors into the stub as
// version 0.2.0 and points trusted_key at the trusted key of the vectors.
func (r *rig) serveVector(name string) {
	r.t.Helper()
	src := filepath.Join(vectors, "cases", name)
	dst := filepath.Join(r.stubDir, "0.2.0")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		r.t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	key, err := filepath.Abs(filepath.Join(vectors, "keys", "trusted.pub"))
	if err != nil {
		r.t.Fatal(err)
	}
	r.start(nil, map[string]any{"trusted_key": key})
}

const signatureLine = "signature: SHA256SUMS verified with the trusted key\n"

func TestSignatureGoodVector(t *testing.T) {
	r := newRig(t)
	r.serveVector("good")
	_, err := r.f.Fetch(Options{})
	if !strings.Contains(r.out.String(), signatureLine) {
		t.Fatalf("the good bundle did not pass the signature step: %v\n%s", err, r.out.String())
	}
	for _, refusal := range []error{release.ErrSignature, release.ErrNoBundle, release.ErrBundle, release.ErrNoTrustedKey} {
		if errors.Is(err, refusal) {
			t.Fatalf("good bundle refused: %v", err)
		}
	}
	if exists(filepath.Join(r.root, "releases", "0.2.0.partial")) {
		t.Fatal("a partial directory stayed")
	}
}

func TestSignatureRefusals(t *testing.T) {
	messages := map[string]string{}
	for _, c := range []struct {
		name, vector string
		want         error
		says         string
	}{
		{"changed byte of SHA256SUMS", "changed-sums", release.ErrSignature, "the digest in the bundle is not the SHA-256 of SHA256SUMS"},
		{"bundle of another key", "other-key", release.ErrSignature, "signed with another key"},
		{"changed signature", "changed-signature", release.ErrSignature, "the bundle names the trusted key"},
		{"missing bundle", "missing-bundle", release.ErrNoBundle, "SHA256SUMS.sigstore.json missing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.serveVector(c.vector)
			err := r.refused(c.want)
			if !strings.Contains(err.Error(), c.says) {
				t.Fatalf("message %q lacks %q", err, c.says)
			}
			if strings.Contains(r.out.String(), signatureLine) {
				t.Fatalf("the signature step passed:\n%s", r.out.String())
			}
			for _, path := range r.requested() {
				if strings.HasSuffix(path, "/"+platformBinary()) {
					t.Fatalf("the binary was requested after a refused signature")
				}
			}
			messages[err.Error()] = c.name
		})
	}
	if len(messages) != 4 {
		t.Fatalf("the four refusals share messages: %v", messages)
	}
	if strings.Contains(firstKey(messages, "changed signature"), "another key") {
		t.Fatal("a changed signature is named as another key")
	}
}

func firstKey(m map[string]string, value string) string {
	for k, v := range m {
		if v == value {
			return k
		}
	}
	return ""
}

func TestMissingKeyConfiguration(t *testing.T) {
	for name, cfg := range map[string]map[string]any{"no key": {}, "empty key": {"trusted_key": ""}} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.serveVector("good")
			full := map[string]any{"channel": map[string]any{"server": r.srv.URL, "owner": "acme"}}
			for k, v := range cfg {
				full[k] = v
			}
			r.config(full)
			err := r.refused(release.ErrNoTrustedKey)
			if !strings.Contains(err.Error(), "trusted_key") || len(r.stub.Requests()) != 0 {
				t.Fatalf("%v after %d requests", err, len(r.stub.Requests()))
			}
		})
	}
	r := newRig(t)
	r.serveVector("good")
	bad := filepath.Join(t.TempDir(), "bad.pub")
	os.WriteFile(bad, []byte("no key\n"), 0o644)
	r.config(map[string]any{"trusted_key": bad, "channel": map[string]any{"server": r.srv.URL, "owner": "acme"}})
	r.refused(release.ErrTrustedKey)
	if len(r.stub.Requests()) != 0 {
		t.Fatal("requests with an unusable key")
	}
}

// TestCosignPubNeverRead serves a cosign.pub whose body never ends: the
// stage passes, so it never read the body, and stages no cosign.pub.
func TestCosignPubNeverRead(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	closed := make(chan struct{}, 8)
	r.wrap = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if !strings.HasSuffix(req.URL.Path, "/"+release.PublicKeyFile) {
				next.ServeHTTP(w, req)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("-----BEGIN PUBLIC KEY-----\n"))
			w.(http.Flusher).Flush()
			select {
			case <-req.Context().Done():
				closed <- struct{}{}
			case <-time.After(30 * time.Second):
			}
		})
	}
	r.start()
	done := make(chan error, 1)
	go func() { _, err := r.f.Fetch(Options{}); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("fetch: %v\n%s", err, r.out.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the fetch stage waits for the body of cosign.pub")
	}
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the connection of cosign.pub stayed open")
	}
	staged := filepath.Join(r.root, "releases", "0.2.0")
	if exists(filepath.Join(staged, release.PublicKeyFile)) || !exists(staged) {
		t.Fatalf("staged %v", r.releases())
	}
}
