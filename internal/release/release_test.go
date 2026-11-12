package release

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cases come from tests/release/release_proof.py fixtures, which made the
// bundles with cosign 3.1.4; its cosign command runs cosign verify-blob on the
// same directories and expects the cosign verdict of each case.
type caseTable struct {
	Cases []struct {
		Name   string `json:"name"`
		Dir    string `json:"dir"`
		Key    string `json:"key"`
		Cosign *bool  `json:"cosign"`
		Check  string `json:"check"`
	} `json:"cases"`
}

var sentinels = map[string]error{
	"ErrNoTrustedKey": ErrNoTrustedKey,
	"ErrTrustedKey":   ErrTrustedKey,
	"ErrKeyInRelease": ErrKeyInRelease,
	"ErrNoSums":       ErrNoSums,
	"ErrSums":         ErrSums,
	"ErrNoBundle":     ErrNoBundle,
	"ErrBundle":       ErrBundle,
	"ErrSignature":    ErrSignature,
	"ErrNotListed":    ErrNotListed,
	"ErrNoBinary":     ErrNoBinary,
	"ErrDigest":       ErrDigest,
}

const trusted = "testdata/keys/trusted.pub"

func cases(t *testing.T) caseTable {
	t.Helper()
	data, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var table caseTable
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}
	return table
}

// refused fails unless err wraps want and no other refusal of this package.
func refused(t *testing.T, err error, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted, want %v", want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("error %q, want %v", err, want)
	}
	for name, other := range sentinels {
		if other != want && errors.Is(err, other) {
			t.Fatalf("error %q also wraps %s", err, name)
		}
	}
}

func linux(key string) Options { return Options{TrustedKey: key, GOOS: "linux", GOARCH: "amd64"} }

// copyRelease copies a fixture release into a temporary directory.
func copyRelease(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("testdata", "cases", name)
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func TestCases(t *testing.T) {
	table := cases(t)
	for _, c := range table.Cases {
		t.Run(c.Name, func(t *testing.T) {
			key := c.Key
			if key != "" {
				key = filepath.Join("testdata", key)
			}
			r, err := Check(filepath.Join("testdata", c.Dir), linux(key))
			if c.Check == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if r.Name != "pipesmith-registry-linux-amd64" || filepath.Base(r.Binary) != r.Name || !filepath.IsAbs(r.Binary) {
					t.Fatalf("binary %q at %q", r.Name, r.Binary)
				}
				if len(r.SHA256) != 64 || len(r.Sums) != 3 {
					t.Fatalf("digest %q, %d sums", r.SHA256, len(r.Sums))
				}
				return
			}
			want, ok := sentinels[c.Check]
			if !ok {
				t.Fatalf("cases.json names an unknown refusal %q", c.Check)
			}
			if r != nil {
				t.Fatalf("a refused check returned a release")
			}
			refused(t, err, want)
		})
	}
}

// TestCaseTable keeps the refusal cases of task 2 in the table, so a shorter
// cases.json cannot pass TestCases.
func TestCaseTable(t *testing.T) {
	want := map[string]string{
		"good":                      "",
		"changed sums":              "ErrSignature",
		"bundle of another key":     "ErrSignature",
		"other trusted key":         "ErrSignature",
		"changed signature":         "ErrSignature",
		"missing bundle":            "ErrNoBundle",
		"missing key configuration": "ErrNoTrustedKey",
		"wrong binary digest":       "ErrDigest",
	}
	seen := map[string]string{}
	for _, c := range cases(t).Cases {
		seen[c.Name] = c.Check
	}
	for name, check := range want {
		got, ok := seen[name]
		if !ok || got != check {
			t.Errorf("case %q: check %q, present %v; want %q", name, got, ok, check)
		}
	}
}

func TestGoodEveryPlatform(t *testing.T) {
	data, err := os.ReadFile("testdata/cases/good/SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range [][2]string{{"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		r, err := Check("testdata/cases/good", Options{TrustedKey: trusted, GOOS: p[0], GOARCH: p[1]})
		if err != nil {
			t.Fatalf("%s/%s: %v", p[0], p[1], err)
		}
		if !bytes.Contains(data, []byte(r.SHA256+"  "+BinaryName(p[0], p[1])+"\n")) {
			t.Fatalf("%s/%s: digest %s is not the line of SHA256SUMS", p[0], p[1], r.SHA256)
		}
	}
}

func TestBinaryName(t *testing.T) {
	if got := BinaryName("linux", "amd64"); got != "pipesmith-registry-linux-amd64" {
		t.Fatalf("BinaryName = %q", got)
	}
}

func TestKeyConfigurationFirst(t *testing.T) {
	_, err := Check("testdata/cases/missing-bundle", Options{})
	refused(t, err, ErrNoTrustedKey)
	_, err = Check(filepath.Join(t.TempDir(), "no-such-release"), Options{})
	refused(t, err, ErrNoTrustedKey)
}

func TestTrustedKeyUnusable(t *testing.T) {
	dir := t.TempDir()
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, pub any) string {
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	garbage := filepath.Join(dir, "garbage.pub")
	if err := os.WriteFile(garbage, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(trusted)
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(dir, "private.pem")
	block, _ := pem.Decode(good)
	if err := os.WriteFile(private, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: block.Bytes}), 0o600); err != nil {
		t.Fatal(err)
	}
	trailing := filepath.Join(dir, "trailing.pub")
	if err := os.WriteFile(trailing, append(append([]byte{}, good...), "extra\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"missing file":          filepath.Join(dir, "missing.pub"),
		"P-384":                 write("p384.pub", &p384.PublicKey),
		"RSA":                   write("rsa.pub", &rsaKey.PublicKey),
		"no PEM":                garbage,
		"other PEM type":        private,
		"text after PEM":        trailing,
		"directory, not a file": dir,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Check("testdata/cases/good", linux(path))
			refused(t, err, ErrTrustedKey)
		})
	}
	if _, err := LoadTrustedKey(trusted); err != nil {
		t.Fatalf("the cosign key is refused: %v", err)
	}
}

func TestMissingSums(t *testing.T) {
	dir := copyRelease(t, "good")
	if err := os.Remove(filepath.Join(dir, SumsFile)); err != nil {
		t.Fatal(err)
	}
	_, err := Check(dir, linux(trusted))
	refused(t, err, ErrNoSums)
	_, err = Check(filepath.Join(t.TempDir(), "no-such-release"), linux(trusted))
	refused(t, err, ErrNoSums)
}

func TestSumsTooLarge(t *testing.T) {
	dir := copyRelease(t, "good")
	big := bytes.Repeat([]byte("x"), maxFile+1)
	if err := os.WriteFile(filepath.Join(dir, SumsFile), big, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Check(dir, linux(trusted))
	refused(t, err, ErrSums)
}

func TestNotListed(t *testing.T) {
	_, err := Check("testdata/cases/good", Options{TrustedKey: trusted, GOOS: "windows", GOARCH: "amd64"})
	refused(t, err, ErrNotListed)
}

func TestNoBinary(t *testing.T) {
	dir := copyRelease(t, "good")
	if err := os.Remove(filepath.Join(dir, "pipesmith-registry-linux-amd64")); err != nil {
		t.Fatal(err)
	}
	_, err := Check(dir, linux(trusted))
	refused(t, err, ErrNoBinary)
}

// A binary that is a link out of the release directory is not read.
func TestBinaryLinkOutside(t *testing.T) {
	dir := copyRelease(t, "good")
	outside := filepath.Join(t.TempDir(), "binary")
	data, err := os.ReadFile(filepath.Join(dir, "pipesmith-registry-linux-amd64"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, data, 0o600); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "pipesmith-registry-linux-amd64")
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, name); err != nil {
		t.Fatal(err)
	}
	_, err = Check(dir, linux(trusted))
	refused(t, err, ErrNoBinary)
}

func TestBundleMalformed(t *testing.T) {
	good, err := os.ReadFile("testdata/cases/good/" + BundleFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(good, &doc); err != nil {
		t.Fatal(err)
	}
	edit := func(change func(m map[string]any)) []byte {
		var m map[string]any
		if err := json.Unmarshal(good, &m); err != nil {
			t.Fatal(err)
		}
		change(m)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	message := func(m map[string]any) map[string]any { return m["messageSignature"].(map[string]any) }
	for name, data := range map[string][]byte{
		"not JSON":             []byte("{"),
		"empty":                {},
		"too large":            bytes.Repeat([]byte(" "), maxFile+1),
		"other media type":     edit(func(m map[string]any) { m["mediaType"] = "application/vnd.dev.sigstore.bundle.v0.4+json" }),
		"no media type":        edit(func(m map[string]any) { delete(m, "mediaType") }),
		"no message signature": edit(func(m map[string]any) { delete(m, "messageSignature") }),
		"DSSE envelope": edit(func(m map[string]any) {
			m["dsseEnvelope"] = map[string]any{"payload": "e30=", "payloadType": "application/vnd.in-toto+json"}
		}),
		"other digest algorithm": edit(func(m map[string]any) {
			message(m)["messageDigest"].(map[string]any)["algorithm"] = "SHA2_384"
		}),
		"no signature":         edit(func(m map[string]any) { delete(message(m), "signature") }),
		"signature not base64": edit(func(m map[string]any) { message(m)["signature"] = "not base64!" }),
		"short digest": edit(func(m map[string]any) {
			message(m)["messageDigest"].(map[string]any)["digest"] = "AAAA"
		}),
	} {
		t.Run(name, func(t *testing.T) {
			dir := copyRelease(t, "good")
			if err := os.WriteFile(filepath.Join(dir, BundleFile), data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Check(dir, linux(trusted))
			refused(t, err, ErrBundle)
		})
	}
}

func TestParseSums(t *testing.T) {
	line := func(c byte, name string) string { return strings.Repeat(string(c), 64) + "  " + name + "\n" }
	good := line('a', "sbom.spdx.json") + line('b', "pipesmith-registry-linux-amd64")
	sums, err := ParseSums([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	want := []Sum{{"sbom.spdx.json", strings.Repeat("a", 64)}, {"pipesmith-registry-linux-amd64", strings.Repeat("b", 64)}}
	if len(sums) != 2 || sums[0] != want[0] || sums[1] != want[1] {
		t.Fatalf("ParseSums = %v", sums)
	}
	for name, text := range map[string]string{
		"empty":            "",
		"no final newline": strings.TrimSuffix(good, "\n"),
		"one space":        strings.Repeat("a", 64) + " name\n",
		"binary marker":    strings.Repeat("a", 64) + " *name\n",
		"upper case hex":   line('A', "name"),
		"short digest":     strings.Repeat("a", 63) + "  name\n",
		"long digest":      strings.Repeat("a", 65) + "  name\n",
		"not hex":          line('g', "name"),
		"slash in name":    line('a', "dir/name"),
		"dot dot":          line('a', ".."),
		"dot":              line('a', "."),
		"space in name":    line('a', "a name"),
		"empty name":       line('a', ""),
		"carriage return":  strings.Repeat("a", 64) + "  name\r\n",
		"empty line":       good + "\n",
		"leading space":    " " + line('a', "name"),
		"trailing space":   strings.Repeat("a", 64) + "  name \n",
		"duplicate name":   line('a', "name") + line('b', "name"),
		"backslash escape": "\\" + line('a', "na\\nme"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSums([]byte(text)); !errors.Is(err, ErrSums) {
				t.Fatalf("ParseSums(%q) = %v, want ErrSums", text, err)
			}
		})
	}
}

func TestVerifyBundleDirect(t *testing.T) {
	key, err := LoadTrustedKey(trusted)
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile("testdata/cases/good/" + SumsFile)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile("testdata/cases/good/" + BundleFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBundle(key, sums, bundle); err != nil {
		t.Fatalf("good bundle refused: %v", err)
	}
	changed := append([]byte{}, sums...)
	changed[len(changed)-2] ^= 0x01
	refused(t, VerifyBundle(key, changed, bundle), ErrSignature)
	refused(t, VerifyBundle(nil, sums, bundle), ErrNoTrustedKey)
}

func TestVerifyFile(t *testing.T) {
	r, err := Check("testdata/cases/good", linux(trusted))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyFile(r.Binary); err != nil {
		t.Fatalf("the checked binary is refused: %v", err)
	}
	other := filepath.Join(t.TempDir(), "copy")
	data, err := os.ReadFile(r.Binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	refused(t, r.VerifyFile(other), ErrDigest)
	refused(t, r.VerifyFile(filepath.Join(t.TempDir(), "missing")), ErrNoBinary)
}

// TestCheckReleaseDir checks a release that tools/release.sh wrote, for the
// three platforms of tools/go.sh build. tests/release/release_proof.py build
// sets RELEASE_CHECK_DIR and RELEASE_CHECK_KEY; without them it is skipped.
func TestCheckReleaseDir(t *testing.T) {
	dir, key := os.Getenv("RELEASE_CHECK_DIR"), os.Getenv("RELEASE_CHECK_KEY")
	if dir == "" || key == "" {
		t.Skip("RELEASE_CHECK_DIR and RELEASE_CHECK_KEY not set")
	}
	for _, p := range [][2]string{{"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		r, err := Check(dir, Options{TrustedKey: key, GOOS: p[0], GOARCH: p[1]})
		if err != nil {
			t.Fatalf("%s/%s: %v", p[0], p[1], err)
		}
		t.Logf("%s sha256 %s", r.Name, r.SHA256)
	}
}
