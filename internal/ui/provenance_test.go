package ui

import (
	"crypto/sha1" // #nosec G505 -- git object ids are SHA-1; the test recomputes the ids of the taken files.
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// contractDir holds docs/ui of the interface repository, taken by
// tools/take_ui.sh, with PROVENANCE.md beside the taken files.
const contractDir = "../../docs/ui"

// takenCommit is the commit of the interface repository that web and
// docs/ui are taken from.
const takenCommit = "1e6d884a06f44de2a3c42b5ce91476b8540a80d0"

// contractTreeID is the git tree id of docs/ui at takenCommit and licenceBlob
// the blob id of its LICENSE, read with git rev-parse 1e6d884:docs/ui and
// git rev-parse 1e6d884:LICENSE on 2026-11-20.
const (
	contractTreeID = "bba132c70e3034b9232ff50cf026d305ff7c5676"
	licenceBlob    = "d645695673349e3947e8e5ae42332d0ac3164cd7"
)

type provenance struct {
	fields map[string]string
	files  map[string]struct {
		blob string
		size int
	}
}

var (
	provenanceField = regexp.MustCompile("^\\| ([A-Za-z0-9/ -]+) \\| `?([^`|]+?)`? \\|$")
	provenanceFile  = regexp.MustCompile("^\\| `([^`]+)` \\| `([0-9a-f]{40})` \\| ([0-9]+) \\|$")
)

// readProvenance reads the field table and the file table of PROVENANCE.md.
func readProvenance(t *testing.T, fsys fs.FS) provenance {
	t.Helper()
	data, err := fs.ReadFile(fsys, "PROVENANCE.md")
	if err != nil {
		t.Fatalf("docs/ui/PROVENANCE.md: %v", err)
	}
	p := provenance{fields: map[string]string{}, files: map[string]struct {
		blob string
		size int
	}{}}
	for _, line := range strings.Split(string(data), "\n") {
		if m := provenanceFile.FindStringSubmatch(line); m != nil {
			size, _ := strconv.Atoi(m[3])
			p.files[m[1]] = struct {
				blob string
				size int
			}{m[2], size}
			continue
		}
		if m := provenanceField.FindStringSubmatch(line); m != nil {
			p.fields[m[1]] = m[2]
		}
	}
	return p
}

func blobID(data []byte) string {
	id := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(data))), data...)) // #nosec G401 -- git object id.
	return hex.EncodeToString(id[:])
}

// docs/ui is the tree of the taken commit: without PROVENANCE.md its git
// tree id is the one PROVENANCE.md names, and every file has the blob id
// and the size listed there.
func TestContractDocumentsAreTheTakenTree(t *testing.T) {
	fsys := os.DirFS(contractDir)
	p := readProvenance(t, fsys)
	want := map[string]string{"Repository": "pipesmith-registry-ui", "Commit": takenCommit}
	for field, value := range want {
		if p.fields[field] != value {
			t.Errorf("field %s is %q, want %q", field, p.fields[field], value)
		}
	}
	for _, field := range []string{"Tree of docs/ui", "Licence SHA-256", "Commit date", "Takeover date"} {
		if p.fields[field] == "" {
			t.Errorf("field %s is missing", field)
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(p.fields["Licence SHA-256"]) {
		t.Errorf("licence SHA-256 %q is not 64 hex digits", p.fields["Licence SHA-256"])
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range entries {
		if e.Name() == "PROVENANCE.md" {
			continue
		}
		data, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		listed, ok := p.files[e.Name()]
		switch {
		case !ok:
			t.Errorf("%s is not listed in PROVENANCE.md", e.Name())
		case listed.blob != blobID(data) || listed.size != len(data):
			t.Errorf("%s has blob %s and %d bytes, PROVENANCE.md lists %s and %d", e.Name(), blobID(data), len(data), listed.blob, listed.size)
		default:
			seen++
		}
	}
	if seen != len(p.files) || seen == 0 {
		t.Errorf("%d files match, PROVENANCE.md lists %d", seen, len(p.files))
	}
	tree := gitTree(t, fsys, ".", func(_ string, id []byte) []byte { return id }, func(name string) bool { return name == "PROVENANCE.md" })
	if got := hex.EncodeToString(tree); got != p.fields["Tree of docs/ui"] || got != contractTreeID {
		t.Errorf("tree of docs/ui is %s, PROVENANCE.md names %s, the taken commit has %s", got, p.fields["Tree of docs/ui"], contractTreeID)
	}
	t.Logf("docs/ui: %d files, tree %s", seen, p.fields["Tree of docs/ui"])
}

// The licence of this repository is the licence of the taken commit, and
// PROVENANCE.md names the SHA-256 of that licence.
func TestLicenceIsTheLicenceOfTheTakenCommit(t *testing.T) {
	data, err := os.ReadFile("../../LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	if got := blobID(data); got != licenceBlob {
		t.Errorf("LICENSE has the blob id %s, the taken commit %s", got, licenceBlob)
	}
	sum := sha256.Sum256(data)
	if named := readProvenance(t, os.DirFS(contractDir)).fields["Licence SHA-256"]; named != hex.EncodeToString(sum[:]) {
		t.Errorf("PROVENANCE.md names the licence SHA-256 %s, the licence blob has %s", named, hex.EncodeToString(sum[:]))
	}
}

// The provenance names the source by its name only: no path of a machine.
func TestProvenanceNamesNoMachinePath(t *testing.T) {
	data, err := os.ReadFile(contractDir + "/PROVENANCE.md")
	if err != nil {
		t.Fatal(err)
	}
	if path := regexp.MustCompile("(^|[\\s`(|])(/|~/|file:)[^\\s`|)]*").FindString(string(data)); path != "" {
		t.Errorf("PROVENANCE.md holds the path %q", strings.TrimSpace(path))
	}
	if !strings.Contains(string(data), "pipesmith-registry-ui") {
		t.Error("PROVENANCE.md does not name the repository")
	}
}
