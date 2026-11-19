package coords

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
)

type expected struct {
	Group      string `json:"group"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Classifier string `json:"classifier"`
	Extension  string `json:"extension"`
	Prerelease bool   `json:"prerelease"`
}

type vector struct {
	Row    string    `json:"row"`
	Path   string    `json:"path"`
	Expect *expected `json:"expect"`
}

type vectorFile struct {
	Format string   `json:"format"`
	Cases  []vector `json:"cases"`
}

var parsers = map[string]func(string) (Coordinates, bool){"maven2": Maven, "npm": Npm, "docker": Docker, "raw": Raw}

func vectors(t testing.TB, format string) []vector {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/coords/" + format + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var file vectorFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if file.Format != format {
		t.Fatalf("%s.json names format %q", format, file.Format)
	}
	return file.Cases
}

// The rows of the table Coordinates of the phase 1c plan, and those of them
// that give a component.
var (
	tableRows = map[string][]string{"maven2": {"artifact file", "metadata above a version directory"},
		"npm": {"tarball", "package document"}, "docker": {"manifest", "blob"}, "raw": {"any path"}}
	componentRows = map[string][]string{"maven2": {"artifact file"}, "npm": {"tarball", "package document"},
		"docker": {"manifest"}, "raw": {"any path"}}
)

// Every vector gives its coordinates through the parser of its format and
// through Of. Every row of the table has a vector; a row that gives a
// component has a path that parses and one that does not, a row of no
// component only paths that give none.
func TestVectors(t *testing.T) {
	for format, parse := range parsers {
		parsed, refused := map[string]int{}, map[string]int{}
		for _, c := range vectors(t, format) {
			got, ok := parse(c.Path)
			viaOf, okOf := Of(format, c.Path)
			if ok != okOf || got != viaOf {
				t.Errorf("%s %q: the parser gives %+v %v, Of gives %+v %v", format, c.Path, got, ok, viaOf, okOf)
			}
			switch {
			case c.Expect == nil && ok:
				t.Errorf("%s %q: %+v, want no component", format, c.Path, got)
			case c.Expect != nil && !ok:
				t.Errorf("%s %q: no component, want %+v", format, c.Path, *c.Expect)
			case c.Expect != nil && got != Coordinates(*c.Expect):
				t.Errorf("%s %q: %+v, want %+v", format, c.Path, got, *c.Expect)
			}
			if c.Expect != nil {
				parsed[c.Row]++
			} else {
				refused[c.Row]++
			}
		}
		for _, row := range tableRows[format] {
			gives := slices.Contains(componentRows[format], row)
			switch {
			case gives && (parsed[row] == 0 || refused[row] == 0):
				t.Errorf("%s row %q: %d paths parse, %d do not; want both", format, row, parsed[row], refused[row])
			case !gives && (parsed[row] != 0 || refused[row] == 0):
				t.Errorf("%s row %q gives no component: %d paths parse, %d do not", format, row, parsed[row], refused[row])
			}
			delete(parsed, row)
			delete(refused, row)
		}
		if len(parsed)+len(refused) > 0 {
			t.Errorf("%s: vectors of rows the table does not have: %v %v", format, parsed, refused)
		}
	}
}

// A format without a parser gives no component, whatever the path.
func TestOtherFormatsGiveNoComponent(t *testing.T) {
	for _, format := range []string{"", "unknown", "pypi", "nuget", "Maven2", "maven"} {
		for _, path := range []string{"com/example/demo-lib/1.2.0/demo-lib-1.2.0.jar", "demo-pkg", "docs/readme.txt"} {
			if c, ok := Of(format, path); ok {
				t.Errorf("format %q, path %q: %+v", format, path, c)
			}
		}
	}
}

// parts are the pieces of the group that a path must hold.
func parts(format, group string) []string {
	switch format {
	case "maven2":
		return strings.Split(group, ".")
	case "raw":
		return strings.Split(strings.TrimPrefix(group, "/"), "/")
	}
	return []string{group}
}

// holds checks that every value of c is a substring of path: no part of the
// coordinates is made up.
func holds(t testing.TB, format, path string, c Coordinates) {
	t.Helper()
	values := append(parts(format, c.Group), c.Name, c.Version, c.Classifier, c.Extension)
	for _, value := range values {
		if !strings.Contains(path, value) {
			t.Fatalf("%s %q: %q of %+v is not in the path", format, path, value, c)
		}
	}
	if c.Name == "" {
		t.Fatalf("%s %q: %+v has no name", format, path, c)
	}
}

const alphabet = "abcdeABC019.-_@:+/ "

func randomText(r *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

func word(r *rand.Rand) string {
	words := []string{"com", "example", "demo-lib", "1.2.0", "1.3.0-SNAPSHOT", "sources", "-", "v2", "manifests",
		"blobs", "@demo", "widgets", "demo-pkg", "latest", "maven-metadata.xml", "sha256:ab", "docs", ""}
	if r.IntN(4) == 0 {
		return randomText(r, 1+r.IntN(6))
	}
	return words[r.IntN(len(words))]
}

// generated is a path in one of the forms the parsers know, often changed
// at one place, or a path of random words.
func generated(r *rand.Rand) string {
	group := []string{"com", "example"}[:1+r.IntN(2)]
	artifact, version := word(r), word(r)
	if r.IntN(2) == 0 {
		artifact, version = "demo-lib", []string{"1.2.0", "1.3.0-SNAPSHOT", "2.0-20261118.101500-1"}[r.IntN(3)]
	}
	var path string
	switch r.IntN(5) {
	case 0:
		name := artifact + "-" + version
		if strings.HasSuffix(version, "-SNAPSHOT") && r.IntN(2) == 0 {
			name = artifact + "-" + strings.TrimSuffix(version, "-SNAPSHOT") + "-20261118.101500-" + word(r)
		}
		if r.IntN(3) == 0 {
			name += "-" + word(r)
		}
		name += "." + []string{"jar", "pom", "jar.sha1", "tar.gz", word(r)}[r.IntN(5)]
		path = strings.Join(append(group, artifact, version, name), "/")
	case 1:
		scope := ""
		if r.IntN(2) == 0 {
			scope = "@" + word(r) + "/"
		}
		name := []string{"demo-pkg", "widgets", word(r)}[r.IntN(3)]
		if r.IntN(3) == 0 {
			path = scope + name
		} else {
			path = scope + name + "/-/" + name + "-" + version + ".tgz"
		}
	case 2:
		reference := []string{"latest", "1.4.2", "sha256:" + strings.Repeat("ab", 32), word(r)}[r.IntN(4)]
		path = "v2/" + strings.Join([]string{"demo", "app", word(r)}[:1+r.IntN(3)], "/") + "/manifests/" + reference
	case 3:
		path = strings.Join(append(group, word(r), word(r)), "/")
	default:
		segments := make([]string, 1+r.IntN(7))
		for i := range segments {
			segments[i] = word(r)
		}
		path = strings.Join(segments, "/")
	}
	if r.IntN(10) == 0 {
		cut := r.IntN(len(path) + 1)
		path = path[:cut] + randomText(r, 1) + path[cut:]
	}
	return path
}

// Over 100,000 generated paths every value of the coordinates any parser
// gives is a substring of the path, and each parser places a good share of
// them, so the check does not pass on parsers that place nothing.
func TestCoordinatesComeFromThePath(t *testing.T) {
	r := rand.New(rand.NewPCG(20261118, 6))
	placed := map[string]int{}
	for range 100_000 {
		path := generated(r)
		for format, parse := range parsers {
			if c, ok := parse(path); ok {
				holds(t, format, path, c)
				placed[format]++
			}
		}
	}
	for format := range parsers {
		if placed[format] < 5_000 {
			t.Errorf("%s placed %d of 100,000 paths", format, placed[format])
		}
	}
	t.Logf("placed of 100,000 paths: %v", placed)
}

func fuzz(f *testing.F, format string) {
	for _, c := range vectors(f, format) {
		f.Add(c.Path)
	}
	parse := parsers[format]
	f.Fuzz(func(t *testing.T, path string) {
		if c, ok := parse(path); ok {
			holds(t, format, path, c)
		}
	})
}

func FuzzMaven(f *testing.F)  { fuzz(f, "maven2") }
func FuzzNpm(f *testing.F)    { fuzz(f, "npm") }
func FuzzDocker(f *testing.F) { fuzz(f, "docker") }
func FuzzRaw(f *testing.F)    { fuzz(f, "raw") }
