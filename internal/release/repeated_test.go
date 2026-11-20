package release

import (
	"errors"
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

// ninePublished is the shape of the published SHA256SUMS of 0.1.2: six
// names, then the three binaries a second time with the same hash.
func ninePublished() (text string, unique int, twice []string) {
	line := func(c byte, name string) string { return strings.Repeat(string(c), 64) + "  " + name + "\n" }
	twice = []string{"pipesmith-registry-darwin-arm64", "pipesmith-registry-linux-amd64", "pipesmith-registry-linux-arm64"}
	hashes := []byte{'a', 'b', 'c'}
	for i, name := range twice {
		text += line(hashes[i], name)
	}
	text += line('d', "sbom.spdx.json") + line('e', "sbom.cdx.json") + line('f', "image.tar")
	for i, name := range twice {
		text += line(hashes[i], name)
	}
	return text, 6, twice
}

// TestRepeatedLinesParseSums pins decision H9 A and P8-6 A in ParseSums: the
// nine lines of 0.1.2 are refused at the first repeated name, and a repeated
// name with another hash is refused the same way.
func TestRepeatedLinesParseSums(t *testing.T) {
	text, unique, twice := ninePublished()
	if n := strings.Count(text, "\n"); n != 9 {
		t.Fatalf("fixture has %d lines, want 9", n)
	}
	lines := strings.SplitAfter(text, "\n")
	head := strings.Join(lines[:unique], "")
	sums, err := ParseSums([]byte(head))
	if err != nil || len(sums) != unique {
		t.Fatalf("the six unique lines: %d sums, %v", len(sums), err)
	}

	sums, err = ParseSums([]byte(text))
	if sums != nil || !errors.Is(err, ErrSums) {
		t.Fatalf("ParseSums of the nine lines = %v, %v, want ErrSums", sums, err)
	}
	if !strings.Contains(err.Error(), twice[0]+" listed twice") {
		t.Fatalf("error %q does not name %s", err, twice[0])
	}

	other := head + strings.Repeat("9", 64) + "  " + twice[1] + "\n"
	sums, err = ParseSums([]byte(other))
	if sums != nil || !errors.Is(err, ErrSums) || !strings.Contains(err.Error(), twice[1]+" listed twice") {
		t.Fatalf("ParseSums with another hash = %v, %v, want ErrSums naming %s", sums, err, twice[1])
	}
}

// TestRepeatedLinesUpdate pins P8-6 A on the update path: a signed release
// whose SHA256SUMS lists the binary twice, with an equal or another hash, is
// refused before anything below the root changes.
func TestRepeatedLinesUpdate(t *testing.T) {
	r := newRig(t, "0.1.0")
	bin := versionScript("0.2.0")
	files := map[string][]byte{platformBinary(): bin, "sbom.spdx.json": []byte("{}"), PublicKeyFile: []byte("not the trusted key\n")}
	once := hexSum(bin) + "  " + platformBinary() + "\n" + hexSum(files["sbom.spdx.json"]) + "  sbom.spdx.json\n"

	for name, sums := range map[string]string{
		"equal hash":   once + hexSum(bin) + "  " + platformBinary() + "\n",
		"another hash": once + strings.Repeat("0", 64) + "  " + platformBinary() + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "release")
			writeSigned(t, dir, r.s, files, sums)
			err := r.unchanged(ErrSums, func() error { return r.u.Update(dir, false) })
			if !strings.Contains(err.Error(), platformBinary()+" listed twice") {
				t.Fatalf("error %q does not name %s", err, platformBinary())
			}
		})
	}

	// The same release with each name once is taken.
	dir := filepath.Join(t.TempDir(), "release")
	writeSigned(t, dir, r.s, files, once)
	if err := r.u.Update(dir, false); err != nil {
		t.Fatalf("the release with unique lines is refused: %v\n%s", err, r.out.String())
	}
	if r.link(CurrentLink) != "releases/0.2.0" {
		t.Fatalf("current is %q after the update", r.link(CurrentLink))
	}
}
