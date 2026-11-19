package release

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRepeatedLines pins decision P8-6: a name listed twice in SHA256SUMS is
// refused, with an equal hash (the shape of the published 0.1.2 file, where
// each binary is listed twice) and with another hash.
func TestRepeatedLines(t *testing.T) {
	s := newSigner(t)
	binaries := []string{"pipesmith-registry-darwin-arm64", "pipesmith-registry-linux-amd64", "pipesmith-registry-linux-arm64"}
	files := map[string][]byte{}
	var lines []string
	for _, name := range binaries {
		files[name] = []byte("binary " + name + "\n")
	}
	files[platformBinary()] = []byte("binary " + platformBinary() + "\n")
	for _, name := range append(append([]string{}, binaries...), "sbom.spdx.json", "sbom.cdx.json", "image.tar") {
		if _, ok := files[name]; !ok {
			files[name] = []byte(name)
		}
		lines = append(lines, hexSum(files[name])+"  "+name+"\n")
	}
	published := strings.Join(lines, "")
	for _, name := range binaries {
		published += hexSum(files[name]) + "  " + name + "\n"
	}
	other := strings.Join(lines, "") + strings.Repeat("0", 64) + "  " + platformBinary() + "\n"

	for name, c := range map[string]struct{ sums, twice string }{
		"each binary twice, equal hashes": {published, binaries[0]},
		"one name twice, another hash":    {other, platformBinary()},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "release")
			writeSigned(t, dir, s, files, c.sums)
			r, err := Check(dir, Options{TrustedKey: s.pub})
			if r != nil {
				t.Fatal("a refused check returned a release")
			}
			refused(t, err, ErrSums)
			if !strings.Contains(err.Error(), c.twice+" listed twice") {
				t.Fatalf("error %q does not name %s", err, c.twice)
			}
		})
	}

	// The same files with each name once pass, so the refusal above is the
	// repeated line and nothing else.
	dir := filepath.Join(t.TempDir(), "release")
	writeSigned(t, dir, s, files, strings.Join(lines, ""))
	if _, err := Check(dir, Options{TrustedKey: s.pub}); err != nil {
		t.Fatalf("the file with unique lines is refused: %v", err)
	}
}
