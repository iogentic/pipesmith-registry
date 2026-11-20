package server

import (
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// The mock of the interface marks its answers with a header and has routes
// that reset and steer it (docs/ui/API.md, section Mock). Both exist only
// in the mock: the server never sends the header and has no such route.

// exampleHeader is the header of the mock that makes the interface show the
// band "Example data".
const exampleHeader = "X-Pipesmith-Example-Data"

// mockPaths are the paths of the routes of the mock.
var mockPaths = []string{"/__mock/reset", "/__mock/scenario/frozen", "/__mock/no-areas", "/__mock", "/__mock/fixtures/demo"}

// carriesExampleHeader tells whether an answer carries the header of the mock.
func carriesExampleHeader(h http.Header) bool {
	return len(h.Values(exampleHeader)) > 0
}

// mockRoutes are the entries of a route table that take a path below
// /__mock, as "METHOD pattern": by the literal prefix of the pattern, or
// because the pattern takes one of mockPaths.
func mockRoutes(routes []RouteInfo) []string {
	var found []string
	for _, r := range routes {
		prefix := literalPrefix(r.Pattern)
		below := prefix == "/__mock" || strings.HasPrefix(prefix, "/__mock/")
		if below || slices.ContainsFunc(mockPaths, regexp.MustCompile(r.Pattern).MatchString) {
			for _, method := range r.Methods {
				found = append(found, method+" "+r.Pattern)
			}
		}
	}
	return found
}

// literalPrefix is the text every match of pattern starts with, after its
// anchor.
func literalPrefix(pattern string) string {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return ""
	}
	parts := []*syntax.Regexp{re}
	if re.Op == syntax.OpConcat {
		parts = re.Sub
	}
	var b strings.Builder
	for _, part := range parts {
		switch {
		case part.Op == syntax.OpBeginText || part.Op == syntax.OpBeginLine:
		case part.Op == syntax.OpLiteral && part.Flags&syntax.FoldCase == 0:
			b.WriteString(string(part.Rune))
		default:
			return b.String()
		}
	}
	return b.String()
}

// sourcesNaming are the Go files below cmd and internal of fsys, test files
// left out, that hold word in any case, and the count of files read.
func sourcesNaming(fsys fs.FS, word string) ([]string, int) {
	var found []string
	read := 0
	for _, root := range []string{"cmd", "internal"} {
		err := fs.WalkDir(fsys, root, func(name string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return err
			}
			data, err := fs.ReadFile(fsys, name)
			if err != nil {
				return err
			}
			read++
			if strings.Contains(strings.ToLower(string(data)), strings.ToLower(word)) {
				found = append(found, name)
			}
			return nil
		})
		if err != nil {
			found = append(found, err.Error())
		}
	}
	return found, read
}

// No Go source of the server names the header, so no answer of any path,
// a success path included, can set it.
func TestNoSourceNamesTheExampleHeader(t *testing.T) {
	found, read := sourcesNaming(os.DirFS("../.."), exampleHeader)
	if len(found) > 0 {
		t.Errorf("Go sources name %s: %v", exampleHeader, found)
	}
	if read < 100 {
		t.Errorf("%d Go files read, want the sources of cmd and internal", read)
	}
	t.Logf("%d Go files read, none names %s", read, exampleHeader)
}

func TestSourceScanFindsAPlantedHeader(t *testing.T) {
	planted := fstest.MapFS{
		"cmd/x/main.go":        {Data: []byte(`h.Set("x-pipesmith-example-data", "1")`)},
		"internal/a/a.go":      {Data: []byte(`const name = "X-Pipesmith-Example-Data"`)},
		"internal/a/a_test.go": {Data: []byte(`const name = "X-Pipesmith-Example-Data"`)},
		"internal/b/b.go":      {Data: []byte(`const name = "X-Content-Type-Options"`)},
		"internal/c/notes.md":  {Data: []byte(`X-Pipesmith-Example-Data`)},
		"tools/t/t.go":         {Data: []byte(`X-Pipesmith-Example-Data`)},
	}
	found, read := sourcesNaming(planted, exampleHeader)
	if want := []string{"cmd/x/main.go", "internal/a/a.go"}; !slices.Equal(found, want) || read != 3 {
		t.Errorf("found %v in %d files, want %v in 3", found, read, want)
	}
}

// The interface reads the header under this name.
func TestTheExampleHeaderIsTheOneTheInterfaceReads(t *testing.T) {
	api, err := os.ReadFile("../ui/web/js/api.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(api), `headers.get("`+exampleHeader+`")`) {
		t.Fatalf("js/api.js does not read the header %s", exampleHeader)
	}
	if !carriesExampleHeader(http.Header{http.CanonicalHeaderKey(exampleHeader): {"1"}}) {
		t.Error("an answer with the header is not seen")
	}
	if carriesExampleHeader(http.Header{"Content-Type": {"application/json"}}) {
		t.Error("an answer without the header is seen")
	}
}

// Every request of the contract and every example of the route table, as
// an administrator and without credentials, with writes off and on: no
// answer carries the header.
func TestNoAnswerCarriesTheExampleHeader(t *testing.T) {
	for _, c := range []struct {
		name   string
		app    *secureApp
		writes bool
	}{{"writes off", newSecureApp(t, newFakeSecurity()), false}, {"writes on", writingSecureApp(t), true}} {
		t.Run(c.name, func(t *testing.T) {
			var targets [][2]string
			for key := range contractRequests(t) {
				method, path, _ := strings.Cut(key, " ")
				targets = append(targets, [2]string{method, filled(path)})
			}
			for _, r := range Routes(c.writes) {
				for _, example := range r.Examples {
					targets = append(targets, [2]string{r.Methods[0], example})
				}
			}
			targets = append(targets, [2]string{"GET", "/"}, [2]string{"GET", "/manual/"}, [2]string{"GET", "/no/such/path"})
			sent := 0
			for _, target := range targets {
				for _, who := range [][]string{{"basic", asAdmin}, nil} {
					a := c.app.send(target[0], target[1], who...)
					if a.status == 0 || len(a.header) == 0 {
						t.Fatalf("%s %s: no answer", target[0], target[1])
					}
					if carriesExampleHeader(a.header) {
						t.Errorf("%s %s answers %d with %s", target[0], target[1], a.status, exampleHeader)
					}
					sent++
				}
			}
			t.Logf("%d answers, none with %s", sent, exampleHeader)
		})
	}
}

// No entry of the route table takes a path of the mock, and a request to
// one answers 404.
func TestNoRouteOfTheMock(t *testing.T) {
	for _, writes := range []bool{false, true} {
		routes := Routes(writes)
		if len(routes) == 0 {
			t.Fatal("the route table is empty")
		}
		if found := mockRoutes(routes); len(found) > 0 {
			t.Errorf("writes %v: the route table takes paths of the mock: %v", writes, found)
		}
	}
	a := writingSecureApp(t)
	for _, path := range mockPaths {
		for _, method := range []string{"GET", "POST"} {
			if r := a.send(method, path, "basic", asAdmin); r.status != http.StatusNotFound {
				t.Errorf("%s %s answers %d, want 404", method, path, r.status)
			}
		}
	}
}

func TestMockRouteGuardFindsAPlantedRoute(t *testing.T) {
	routes := slices.Concat(Routes(true), []RouteInfo{
		{Table: "planted", Pattern: `^/__mock/reset\n?$`, Methods: []string{"POST"}},
		{Table: "planted", Pattern: `^/__mock/scenario/([^/]+)\n?$`, Methods: []string{"GET"}},
		{Table: "planted", Pattern: `^/__mock/other/([^/]+)/deep\n?$`, Methods: []string{"PUT"}},
		{Table: "planted", Pattern: `^/__mockery\n?$`, Methods: []string{"GET"}},
	})
	want := []string{`POST ^/__mock/reset\n?$`, `GET ^/__mock/scenario/([^/]+)\n?$`, `PUT ^/__mock/other/([^/]+)/deep\n?$`}
	if got := mockRoutes(routes); !slices.Equal(got, want) {
		t.Errorf("found %v, want %v", got, want)
	}
	for _, r := range Routes(true) {
		if regexp.MustCompile(r.Pattern).MatchString("/api/session") && len(mockRoutes([]RouteInfo{r})) > 0 {
			t.Errorf("the route of the session counts as a route of the mock")
		}
	}
}
