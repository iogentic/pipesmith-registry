// Package releasetest holds test helpers for release files: an ECDSA P-256
// key pair made in the test process, a cosign bundle in the form
// release.VerifyBundle accepts, and signed release directories. Only tests
// import it. It does not import package release, so the tests inside that
// package can use it.
package releasetest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// BundleMediaType is the bundle format of cosign 3.1.4.
const BundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"

// Names of the release files, as package release names them.
const (
	SumsFile   = "SHA256SUMS"
	BundleFile = "SHA256SUMS.sigstore.json"
)

// Signer is a key pair with its public key as a PEM file.
type Signer struct {
	Priv *ecdsa.PrivateKey
	PEM  []byte // the public key, PEM of type PUBLIC KEY
	Pub  string // a file holding PEM
}

// NewSigner makes a key pair and writes its public key into a temporary
// directory of t.
func NewSigner(t testing.TB) *Signer {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	s := &Signer{Priv: priv, PEM: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}),
		Pub: filepath.Join(t.TempDir(), "trusted.pub")}
	if err := os.WriteFile(s.Pub, s.PEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

// Hint is the key hint cosign writes into a bundle: the base64 of the
// SHA-256 of the DER form of the public key.
func (s *Signer) Hint(t testing.TB) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&s.Priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Bundle is a cosign bundle with a message signature over sums and the key
// hint of s.
func (s *Signer) Bundle(t testing.TB, sums []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(sums)
	sig, err := ecdsa.SignASN1(rand.Reader, s.Priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"mediaType":            BundleMediaType,
		"verificationMaterial": map[string]any{"publicKey": map[string]any{"hint": s.Hint(t)}},
		"messageSignature": map[string]any{
			"messageDigest": map[string]any{"algorithm": "SHA2_256", "digest": sum[:]},
			"signature":     sig,
		},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// HexSum is the SHA-256 of data in lower case hex.
func HexSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SumsOf is SHA256SUMS for the named files, in the order given.
func SumsOf(files map[string][]byte, names ...string) string {
	var sums string
	for _, name := range names {
		sums += HexSum(files[name]) + "  " + name + "\n"
	}
	return sums
}

// WriteSigned writes files, SHA256SUMS with the text sums and its bundle
// signed by s into dir.
func WriteSigned(t testing.TB, dir string, s *Signer, files map[string][]byte, sums string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- test support: a release folder with the modes tools/release.sh gives it.
		t.Fatal(err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil { // #nosec G306 -- test support: release binaries are executable.
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, SumsFile), []byte(sums), 0o644); err != nil { // #nosec G306 -- test support: a release file every user reads.
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, BundleFile), s.Bundle(t, []byte(sums)), 0o644); err != nil { // #nosec G306 -- test support: a release file every user reads.
		t.Fatal(err)
	}
}
