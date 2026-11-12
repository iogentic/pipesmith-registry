// Package release checks a directory of release files as docs/RELEASE.md of
// the ci repository describes them, in the order given there: it reads
// SHA256SUMS and the cosign bundle SHA256SUMS.sigstore.json, verifies the
// signature in the bundle with a public key from the configuration (never the
// cosign.pub of the same release), and compares the SHA-256 of the binary for
// the platform with its line in SHA256SUMS. Only the standard library is used:
// a key of cosign generate-key-pair is ECDSA on P-256, and a bundle made with
// such a key carries an ASN.1 signature over the SHA-256 of SHA256SUMS.
package release

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Names of the release files.
const (
	SumsFile      = "SHA256SUMS"
	BundleFile    = "SHA256SUMS.sigstore.json"
	PublicKeyFile = "cosign.pub"
)

// maxFile bounds the size of SHA256SUMS and of the bundle.
const maxFile = 1 << 20

// bundleMediaType is the bundle format of cosign 3.1.4, the version of the
// release job.
const bundleMediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"

// Refusals of Check. Every error of Check wraps exactly one of them.
var (
	ErrNoTrustedKey = errors.New("no trusted public key configured")
	ErrTrustedKey   = errors.New("trusted public key unusable")
	ErrKeyInRelease = errors.New("trusted public key lies in the release directory")
	ErrNoSums       = errors.New("SHA256SUMS missing")
	ErrSums         = errors.New("SHA256SUMS malformed")
	ErrNoBundle     = errors.New("SHA256SUMS.sigstore.json missing")
	ErrBundle       = errors.New("SHA256SUMS.sigstore.json malformed")
	ErrSignature    = errors.New("signature over SHA256SUMS does not verify with the trusted key")
	ErrNotListed    = errors.New("binary not listed in SHA256SUMS")
	ErrNoBinary     = errors.New("binary missing")
	ErrDigest       = errors.New("SHA-256 of the binary differs from SHA256SUMS")
)

// Options configure Check.
type Options struct {
	// TrustedKey is the PEM file of the trusted public key, as the
	// configuration names it. Empty refuses every release.
	TrustedKey string
	// GOOS and GOARCH select the binary; empty means the running platform.
	GOOS   string
	GOARCH string
}

// Sum is one line of SHA256SUMS.
type Sum struct {
	Name   string
	SHA256 string // hex, lower case
}

// Release is a release directory that passed Check.
type Release struct {
	Dir    string // absolute path of the release directory
	Name   string // file name of the binary for the platform
	Binary string // absolute path of that binary
	SHA256 string // its SHA-256 in hex, as SHA256SUMS lists it and as measured
	Sums   []Sum  // every line of SHA256SUMS, in order
}

// BinaryName is the file name of the binary for a platform, as tools/go.sh
// build names it.
func BinaryName(goos, goarch string) string {
	return "pipesmith-registry-" + goos + "-" + goarch
}

// Check checks the release directory dir and returns the release on success.
// The key configuration is checked first, then the signature over
// SHA256SUMS, then SHA256SUMS itself and the digest of the binary, so a
// changed SHA256SUMS is a signature refusal even when it matches the binary.
func Check(dir string, opt Options) (*Release, error) {
	if opt.TrustedKey == "" {
		return nil, ErrNoTrustedKey
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSums, err)
	}
	if err := keyOutside(opt.TrustedKey, abs); err != nil {
		return nil, err
	}
	key, err := LoadTrustedKey(opt.TrustedKey)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSums, err)
	}
	defer root.Close()

	sums, err := readLimited(root, SumsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNoSums, filepath.Join(abs, SumsFile))
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSums, err)
	}
	bundle, err := readLimited(root, BundleFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNoBundle, filepath.Join(abs, BundleFile))
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBundle, err)
	}
	if err := VerifyBundle(key, sums, bundle); err != nil {
		return nil, err
	}
	lines, err := ParseSums(sums)
	if err != nil {
		return nil, err
	}

	goos, goarch := opt.GOOS, opt.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	name := BinaryName(goos, goarch)
	want := ""
	for _, s := range lines {
		if s.Name == name {
			want = s.SHA256
		}
	}
	if want == "" {
		return nil, fmt.Errorf("%w: %s", ErrNotListed, name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoBinary, err)
	}
	got, err := digest(f)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoBinary, err)
	}
	if !sameHex(got, want) {
		return nil, fmt.Errorf("%w: %s has %s, SHA256SUMS lists %s", ErrDigest, name, got, want)
	}
	return &Release{Dir: abs, Name: name, Binary: filepath.Join(abs, name), SHA256: want, Sums: lines}, nil
}

// VerifyFile checks that the file at path has the SHA-256 of the checked
// binary, for a copy made after Check and before it runs.
func (r *Release) VerifyFile(path string) error {
	f, err := os.Open(path) // #nosec G304 -- the caller's own copy of the checked binary.
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoBinary, err)
	}
	got, err := digest(f)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoBinary, err)
	}
	if !sameHex(got, r.SHA256) {
		return fmt.Errorf("%w: %s has %s, SHA256SUMS lists %s", ErrDigest, path, got, r.SHA256)
	}
	return nil
}

// LoadTrustedKey reads the trusted public key from the PEM file at path.
func LoadTrustedKey(path string) (*ecdsa.PublicKey, error) {
	if path == "" {
		return nil, ErrNoTrustedKey
	}
	f, err := os.Open(path) // #nosec G304 -- the trusted key file the operator configured.
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTrustedKey, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrTrustedKey, path, err)
	}
	if len(data) > maxFile {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrTrustedKey, path, maxFile)
	}
	key, err := ParseTrustedKey(data)
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", err, path)
	}
	return key, nil
}

// ParseTrustedKey parses one PEM block of type PUBLIC KEY that holds an
// ECDSA key on P-256, the form cosign generate-key-pair writes.
func ParseTrustedKey(data []byte) (*ecdsa.PublicKey, error) {
	block, rest := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%w: no PEM block", ErrTrustedKey)
	}
	if block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("%w: PEM block %q, want PUBLIC KEY", ErrTrustedKey, block.Type)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("%w: text after the PEM block", ErrTrustedKey)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTrustedKey, err)
	}
	key, ok := pub.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: %T, want ECDSA on P-256", ErrTrustedKey, pub)
	}
	return key, nil
}

type bundleDoc struct {
	MediaType        string           `json:"mediaType"`
	MessageSignature *messageDoc      `json:"messageSignature"`
	DSSEEnvelope     *json.RawMessage `json:"dsseEnvelope"`
}

type messageDoc struct {
	MessageDigest struct {
		Algorithm string `json:"algorithm"`
		Digest    []byte `json:"digest"`
	} `json:"messageDigest"`
	Signature []byte `json:"signature"`
}

// VerifyBundle verifies the cosign bundle over sums with key. The digest
// is computed from sums, never taken from the bundle; the digest in the bundle
// must equal it, as cosign verify-blob requires. The public key hint and any
// transparency log entry of the bundle are not used, as with
// --insecure-ignore-tlog.
func VerifyBundle(key *ecdsa.PublicKey, sums, bundle []byte) error {
	if key == nil {
		return ErrNoTrustedKey
	}
	if len(bundle) > maxFile {
		return fmt.Errorf("%w: larger than %d bytes", ErrBundle, maxFile)
	}
	var doc bundleDoc
	if err := json.Unmarshal(bundle, &doc); err != nil {
		return fmt.Errorf("%w: %v", ErrBundle, err)
	}
	if doc.MediaType != bundleMediaType {
		return fmt.Errorf("%w: media type %q, want %q", ErrBundle, doc.MediaType, bundleMediaType)
	}
	if doc.DSSEEnvelope != nil || doc.MessageSignature == nil {
		return fmt.Errorf("%w: no message signature", ErrBundle)
	}
	msg := doc.MessageSignature
	if msg.MessageDigest.Algorithm != "SHA2_256" || len(msg.MessageDigest.Digest) != sha256.Size {
		return fmt.Errorf("%w: digest %q of %d bytes, want SHA2_256", ErrBundle, msg.MessageDigest.Algorithm, len(msg.MessageDigest.Digest))
	}
	if len(msg.Signature) == 0 {
		return fmt.Errorf("%w: empty signature", ErrBundle)
	}
	sum := sha256.Sum256(sums)
	if subtle.ConstantTimeCompare(sum[:], msg.MessageDigest.Digest) != 1 {
		return fmt.Errorf("%w: the digest in the bundle is not the SHA-256 of SHA256SUMS", ErrSignature)
	}
	if !ecdsa.VerifyASN1(key, sum[:], msg.Signature) {
		return ErrSignature
	}
	return nil
}

var sumLine = regexp.MustCompile(`^([0-9a-f]{64})  ([A-Za-z0-9._-]+)$`)

// ParseSums parses SHA256SUMS as sha256sum writes it: per line the SHA-256
// in lower case hex, two spaces and a plain file name, each line ending in a
// newline. Names are unique and never "." or "..".
func ParseSums(data []byte) ([]Sum, error) {
	if len(data) > maxFile {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrSums, maxFile)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil, fmt.Errorf("%w: empty or without a final newline", ErrSums)
	}
	var sums []Sum
	seen := map[string]bool{}
	for i, line := range strings.Split(string(data[:len(data)-1]), "\n") {
		m := sumLine.FindStringSubmatch(line)
		if m == nil || m[2] == "." || m[2] == ".." {
			return nil, fmt.Errorf("%w: line %d is not \"<sha256>  <name>\"", ErrSums, i+1)
		}
		if seen[m[2]] {
			return nil, fmt.Errorf("%w: %s listed twice", ErrSums, m[2])
		}
		seen[m[2]] = true
		sums = append(sums, Sum{Name: m[2], SHA256: m[1]})
	}
	return sums, nil
}

// keyOutside refuses a trusted key file inside the release directory, such
// as the cosign.pub of the release itself.
func keyOutside(key, dir string) error {
	k, err := filepath.Abs(key)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTrustedKey, err)
	}
	if real, err := filepath.EvalSymlinks(k); err == nil {
		k = real
	}
	d := dir
	if real, err := filepath.EvalSymlinks(d); err == nil {
		d = real
	}
	rel, err := filepath.Rel(d, k)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s", ErrKeyInRelease, key)
	}
	return nil
}

func readLimited(root *os.Root, name string) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, maxFile)
	}
	return data, nil
}

func digest(f *os.File) (string, error) {
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", f.Name())
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sameHex(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
