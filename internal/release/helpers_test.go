package release

import (
	"runtime"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/release/releasetest"
)

// signer is an ECDSA P-256 key pair made in the test process, with its
// public key as a PEM file.
type signer struct {
	*releasetest.Signer
	pub string
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	s := releasetest.NewSigner(t)
	return &signer{Signer: s, pub: s.Pub}
}

// bundle is a cosign 3.1.4 bundle with a message signature over sums.
func (s *signer) bundle(t *testing.T, sums []byte) []byte { return s.Bundle(t, sums) }

func hexSum(data []byte) string { return releasetest.HexSum(data) }

// platformBinary is the binary name of the running platform.
func platformBinary() string { return BinaryName(runtime.GOOS, runtime.GOARCH) }

// writeSigned writes files, SHA256SUMS with the text sums and its bundle
// signed by s into dir.
func writeSigned(t *testing.T, dir string, s *signer, files map[string][]byte, sums string) {
	t.Helper()
	releasetest.WriteSigned(t, dir, s.Signer, files, sums)
}

// TestHelperNames keeps the names of the test support package equal to the
// names of this package.
func TestHelperNames(t *testing.T) {
	if releasetest.BundleMediaType != bundleMediaType || releasetest.SumsFile != SumsFile || releasetest.BundleFile != BundleFile {
		t.Fatal("releasetest names differ from package release")
	}
}
