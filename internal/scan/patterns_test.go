package scan_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// patternBound is the longest time one call of a pattern of the server may
// take on one hostile input of patternInput bytes, the best of three runs.
const (
	patternBound = 250 * time.Millisecond
	patternInput = 100_000
)

// slowSites are sites over patternBound at patternInput bytes, each with its
// own bound and the reason it is kept. The search pattern of a value with
// 512 wildcards has 1,025 states; RE2 stays linear in the input, with that
// many states per byte (measured 0.69 to 0.82 s for 100,000 bytes on
// 2026-11-20, darwin/arm64 host in the container, without -race).
var slowSites = map[string]time.Duration{
	`internal/rest/search.go|"(?s)" + expr`: 3 * time.Second,
}

// standIns are the patterns of the sites whose text is built at run time,
// keyed by file and expression: the forms the code can build, with the
// longest input it allows where that makes them slower.
var standIns = map[string][]string{
	// argparse: "^" + nargsPattern(a), every return of nargsPattern.
	`internal/cli/argparse.go|"^" + pattern`: {"^(A?)", "^(-*A?-*)", "^([AO]{0})", "^((?:-*A){0}-*)", "^(A[AO]*)",
		"^(-*A[-AO]*)", "^([A])", "^(-*A-*)"},
	// search: a value of maxValue (1024) bytes, every other one a wildcard.
	`internal/rest/search.go|"(?s)" + expr`: {"(?s)" + strings.Repeat("a.*", 512), "(?s)^" + strings.Repeat("a.*", 512) + "$"},
	// routing rules: matchers of the operator, anchored as whole() does.
	`internal/routing/routing.go|anchored.String()`: {`^(?:(a|aa)*b)$`, `^(?:(.*)*x)$`, `^(?:(a+)+$)$`,
		`^(?:` + strings.Repeat(`(a|b|c)?`, 100) + `d)$`, `^(?:.*/(com|org)/.*\.(jar|pom))$`},
	// get(path) of internal/server: exact() of a REST path with segments.
	`internal/server/secure.go|path`: {`^/service/rest/v1/search/assets$`, `^/service/rest/v1/repositories/([^/]+)/([^/]+)$`,
		`^/service/rest/v1/components/([^/]+)$`},
	// identityRoute, ldapCheckRoute, storeRoute, routingRoute and taskRoute
	// of internal/server: exact() of a REST path with segments (phase 6).
	`internal/server/identity.go|path`: {`^/service/rest/v1/security/users/([^/]+)/change\-password$`,
		`^/service/rest/v1/security/ldap/([^/]+)$`, `^/api/ldap/verify\-connection$`},
	`internal/server/stores.go|path`: {`^/service/rest/v1/repositories/([^/]+)/([^/]+)/([^/]+)$`,
		`^/service/rest/v1/blobstores/file/([^/]+)$`, `^/service/rest/v1/repositories/([^/]+)/rebuild\-index$`},
	`internal/server/system.go|path`: {`^/service/rest/v1/routing\-rules/([^/]+)$`, `^/api/routing\-rules/preview$`,
		`^/service/rest/v1/tasks/([^/]+)/run$`, `^/service/rest/v1/tasks/([^/]+)$`},
	// freezeRoute: one of the three names of the read only mode.
	`internal/server/system.go|restBase+"/read-only/"+name`: {`^/service/rest/v1/read\-only/freeze$`,
		`^/service/rest/v1/read\-only/force\-release$`},
	// refusal templates: quoted text, segments, (.+) and word alternatives.
	`internal/server/secure.go|b.String()`: {`^/service/rest/v1/tasks/([^/]+)/(run|stop)$`,
		`^/service/rest/v1/read\-only/(freeze|release|force-release)$`, `^/api/metrics/(requests|downloads)$`, `^/api/x/(.+)$`},
	// the page: the two tags it reads.
	"internal/server/page.go|`(?s)<`+tag+`>(.*?)</`+tag+`>`": {`(?s)<script>(.*?)</script>`, `(?s)<style>(.*?)</style>`},
}

type site struct {
	at       string // file:line
	key      string // file|expression
	patterns []string
}

// serverPatterns walks every Go file of internal/ and cmd/ outside tests
// and returns each call of regexp.MustCompile, regexp.Compile and exact
// (the route helper of internal/server) with the text of its pattern, from
// literals and the string constants of the same package, or from standIns.
func serverPatterns(t *testing.T) ([]site, []string) {
	t.Helper()
	var sites []site
	var unknown []string
	fset := token.NewFileSet()
	for _, top := range []string{"../../internal", "../../cmd"} {
		dirs := map[string][]*ast.File{}
		err := filepath.WalkDir(top, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && path != top && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			dirs[filepath.Dir(path)] = append(dirs[filepath.Dir(path)], f)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, files := range dirs {
			consts := map[string]ast.Expr{}
			for _, f := range files {
				for _, decl := range f.Decls {
					g, ok := decl.(*ast.GenDecl)
					if !ok || (g.Tok != token.CONST && g.Tok != token.VAR) {
						continue
					}
					for _, spec := range g.Specs {
						v := spec.(*ast.ValueSpec)
						for i, name := range v.Names {
							if i < len(v.Values) {
								consts[name.Name] = v.Values[i]
							}
						}
					}
				}
			}
			for _, f := range files {
				ast.Inspect(f, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok || len(call.Args) != 1 {
						return true
					}
					wrap := ""
					switch fun := call.Fun.(type) {
					case *ast.SelectorExpr:
						pkg, ok := fun.X.(*ast.Ident)
						if !ok || pkg.Name != "regexp" || (fun.Sel.Name != "MustCompile" && fun.Sel.Name != "Compile") {
							return true
						}
					case *ast.Ident:
						if fun.Name != "exact" || !strings.Contains(filepath.ToSlash(fset.Position(call.Pos()).Filename), "internal/server/") {
							return true
						}
						wrap = "exact"
					default:
						return true
					}
					pos := fset.Position(call.Pos())
					file := strings.TrimPrefix(filepath.ToSlash(pos.Filename), "../../")
					source := exprText(fset, call.Args[0])
					s := site{at: file + ":" + strconv.Itoa(pos.Line), key: file + "|" + source}
					if text, ok := evaluate(call.Args[0], consts, 0); ok {
						if wrap == "exact" {
							text = "^" + text + "$"
						}
						s.patterns = []string{text}
					} else if stand, ok := standIns[s.key]; ok {
						s.patterns = stand
					} else if s.key == `internal/server/secure.go|"^" + path + "$"` {
						return true // the body of exact; its callers are sites
					} else {
						unknown = append(unknown, s.at+" "+source)
						return true
					}
					sites = append(sites, s)
					return true
				})
			}
		}
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].at < sites[j].at })
	return sites, unknown
}

func exprText(fset *token.FileSet, e ast.Expr) string {
	start, end := fset.Position(e.Pos()), fset.Position(e.End())
	data, err := os.ReadFile(start.Filename)
	if err != nil {
		return ""
	}
	return string(data[start.Offset:end.Offset])
}

// evaluate is the string of a literal, a constant of the package or a sum
// of them.
func evaluate(e ast.Expr, consts map[string]ast.Expr, depth int) (string, bool) {
	if depth > 20 {
		return "", false
	}
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return evaluate(v.X, consts, depth+1)
	case *ast.Ident:
		if c, ok := consts[v.Name]; ok {
			return evaluate(c, consts, depth+1)
		}
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		a, ok := evaluate(v.X, consts, depth+1)
		if !ok {
			return "", false
		}
		b, ok := evaluate(v.Y, consts, depth+1)
		return a + b, ok
	}
	return "", false
}

// hostileInputs are lines of patternInput bytes made to keep a matcher
// busy: long runs of one character class, runs that almost match and fail
// at the end, and repeated separators.
func hostileInputs() map[string]string {
	n := patternInput
	runs := map[string]string{}
	for name, unit := range map[string]string{"a": "a", "space": " ", "dash": "-", "slash": "/", "digit": "0",
		"dot": ".", "brace": "{", "lt": "<", "newline": "\n", "A": "A", "colon": ":", "hex": "f", "quote": `"`,
		"word.": "a.", "a-": "a-", "slash-a": "/a", "AO": "AO", "-A": "-A", "script": "<script>", "W/": `W/"`,
		"x.y": "x.y", "1.": "1."} {
		line := strings.Repeat(unit, n/len(unit))
		runs[name] = line
		runs[name+"!"] = line[:len(line)-1] + "!"
	}
	return runs
}

// best is the time of one call, or the best of three when the first takes
// more than a tenth of patternBound.
func best(f func()) time.Duration {
	start := time.Now()
	f()
	bestTime := time.Since(start)
	if bestTime < patternBound/10 {
		return bestTime
	}
	for range 2 {
		start := time.Now()
		f()
		if d := time.Since(start); d < bestTime {
			bestTime = d
		}
	}
	return bestTime
}

// TestEveryServerPatternIsFastOnLongHostileLines times each pattern of the
// server on each hostile line with MatchString, FindStringSubmatchIndex and
// FindAllStringSubmatchIndex, the calls the code makes. Go's regexp is RE2
// and linear in the input, so a failure here names a pattern whose
// constant factor is too high at 100,000 bytes.
func TestEveryServerPatternIsFastOnLongHostileLines(t *testing.T) {
	sites, unknown := serverPatterns(t)
	if len(unknown) > 0 {
		t.Fatalf("sites whose pattern is built at run time and has no stand-in in standIns:\n%s", strings.Join(unknown, "\n"))
	}
	if len(sites) < 60 {
		t.Fatalf("%d sites found; the walk lost its files", len(sites))
	}
	for _, s := range sites {
		for _, text := range s.patterns {
			if _, err := regexp.Compile(text); err != nil {
				t.Errorf("%s: %q does not compile: %v", s.at, text, err)
			}
		}
	}
	if testing.Short() || raceOn {
		t.Skip("the times need a run without -short and without -race")
	}
	inputs := hostileInputs()
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	patterns := 0
	var slowest time.Duration
	slowestAt := ""
	for _, s := range sites {
		for _, text := range s.patterns {
			re, err := regexp.Compile(text)
			if err != nil {
				t.Errorf("%s: %q does not compile: %v", s.at, text, err)
				continue
			}
			patterns++
			for _, name := range names {
				line := inputs[name]
				for call, f := range map[string]func(){
					"MatchString":                func() { re.MatchString(line) },
					"FindStringSubmatchIndex":    func() { re.FindStringSubmatchIndex(line) },
					"FindAllStringSubmatchIndex": func() { re.FindAllStringSubmatchIndex(line, -1) },
				} {
					d := best(f)
					if d > slowest {
						slowest, slowestAt = d, s.at+" "+call+" "+name
					}
					bound := patternBound
					if slow, ok := slowSites[s.key]; ok {
						bound = slow
					}
					if d > bound {
						t.Errorf("%s: %s of %.80q on %d bytes of %q took %v, bound %v", s.at, call, text, len(line), name, d, bound)
					}
				}
			}
		}
	}
	t.Logf("%d sites, %d patterns, %d lines of %d bytes, 3 calls each; slowest %v at %s", len(sites), patterns,
		len(inputs), patternInput, slowest, slowestAt)
	for key := range slowSites {
		if !slices.ContainsFunc(sites, func(s site) bool { return s.key == key }) {
			t.Errorf("slow site %s is gone; remove it from slowSites", key)
		}
	}
	search := regexp.MustCompile(standIns[`internal/rest/search.go|"(?s)" + expr`][1])
	for _, size := range []int{1_000, 8_000, 100_000} {
		line := strings.Repeat("a", size-1) + "!"
		t.Logf("anchored search pattern of 512 wildcards, MatchString on %d bytes: %v", size, best(func() { search.MatchString(line) }))
	}
	for _, s := range sites {
		t.Logf("site %s: %d pattern(s)", s.at, len(s.patterns))
	}
}
