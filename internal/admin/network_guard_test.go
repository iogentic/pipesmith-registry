package admin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The network guard of phase 6 (rulings SB4 a and SB10 of leg 6-system):
// the source of the packages this phase added, and the files of leg
// 6-system in older packages, makes no outbound call and imports no package
// that may connect out, outside the list of files with reasons, and holds
// no dot import. The packages that may connect out are derived from the
// source of the module, not kept by hand.

const modulePath = "github.com/iogentic/pipesmith-registry"

// guardedPackages are the packages phase 6 added, relative to internal/.
var guardedPackages = []string{"admin", "cleanup", "routing", "tasks"}

// guardedFiles are the files of leg 6-system in packages of earlier
// phases, relative to internal/.
var guardedFiles = []string{"rest/capabilities.go", "rest/information.go", "rest/routing.go", "rest/system.go", "rest/tasks.go",
	"server/system.go"}

// mayDial are the guarded files that import a package that may connect
// out, each with its reason, relative to internal/.
var mayDial = map[string]string{
	"admin/identity.go":     "calls the permission and password checks of internal/auth, which imports net to read the address of a request; it opens no connection",
	"admin/ldap.go":         "checks an LDAP form with ldapauth.New, which builds the directory and opens no connection; the checks that connect live in internal/rest/ldap_check.go",
	"admin/repositories.go": "imports internal/proxy for proxy.Closed, the address classifier of the dial guard; it resolves only through the Lookup its caller gives",
	"server/system.go":      "builds the answers of internal/rest and runs in internal/server, which listens; it calls no LDAP check and no proxy",
}

// dialNames are the names that open a connection or resolve a host, of a
// package that a file imports for other names too (SB10: net/http for a
// client, crypto/tls to dial).
var dialNames = map[string][]string{
	"net/http":   {"Get", "Head", "Post", "PostForm", "Client", "DefaultClient", "Transport", "DefaultTransport"},
	"crypto/tls": {"Dial", "DialWithDialer", "Dialer"},
}

// dialRoots are packages outside the module that connect out in any use:
// net itself, and packages of the standard library and of the LDAP module
// that import it to dial.
var dialRoots = []string{"net", "net/smtp", "net/rpc", "net/rpc/jsonrpc", "net/http/httputil", "github.com/go-ldap/ldap/v3"}

// source is what the guard reads of one Go file: its imports by local name
// and the names it takes from each.
type source struct {
	imports map[string]string // local name -> import path
	dots    []string          // paths of dot imports
	used    map[string][]string
	lines   map[string]int // import path -> line of its import
}

func readSource(fset *token.FileSet, file *ast.File) source {
	src := source{imports: map[string]string{}, used: map[string][]string{}, lines: map[string]int{}}
	for _, spec := range file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		src.lines[path] = fset.Position(spec.Pos()).Line
		name := path[strings.LastIndex(path, "/")+1:]
		if path == "github.com/go-ldap/ldap/v3" {
			name = "ldap"
		}
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "." {
			src.dots = append(src.dots, path)
			continue
		}
		src.imports[name] = path
	}
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok {
				if path, ok := src.imports[x.Name]; ok {
					src.used[path] = append(src.used[path], sel.Sel.Name)
				}
			}
		}
		return true
	})
	return src
}

// direct lists what of a file connects out by itself: an import of a dial
// root, and a dial name of net/http or crypto/tls.
func (src source) direct() []string {
	var found []string
	for path, line := range src.lines {
		if slices.Contains(dialRoots, path) {
			found = append(found, strconv.Itoa(line)+": import "+path)
		}
	}
	for path, names := range src.used {
		for _, name := range names {
			if slices.Contains(dialNames[path], name) {
				found = append(found, strconv.Itoa(src.lines[path])+": "+path+"."+name)
			}
		}
	}
	slices.Sort(found)
	return found
}

// modulePackages reads every package of the module below internal/ and
// cmd/, tests left out, by import path.
func modulePackages(t *testing.T) map[string][]source {
	t.Helper()
	pkgs := map[string][]source{}
	for _, top := range []string{"internal", "cmd"} {
		root := filepath.Join("..", "..", top)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(filepath.Join("..", ".."), filepath.Dir(path))
			pkgs[modulePath+"/"+filepath.ToSlash(rel)] = append(pkgs[modulePath+"/"+filepath.ToSlash(rel)], readSource(fset, file))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return pkgs
}

// dialingPackages derives the packages of the module that may connect out
// (SB10): one with a file that connects out by itself, or that imports such
// a package, transitively.
func dialingPackages(pkgs map[string][]source) map[string]bool {
	dialing := map[string]bool{}
	for path, files := range pkgs {
		for _, f := range files {
			if len(f.direct()) > 0 {
				dialing[path] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for path, files := range pkgs {
			if dialing[path] {
				continue
			}
			for _, f := range files {
				for _, imported := range f.imports {
					if dialing[imported] {
						dialing[path], changed = true, true
					}
				}
			}
		}
	}
	return dialing
}

// outbound lists what of a guarded file connects out or may: its own calls,
// an import of a derived package that is not guarded itself, and a dot
// import.
func outbound(src source, dialing map[string]bool) []string {
	found := src.direct()
	guarded := map[string]bool{}
	for _, p := range guardedPackages {
		guarded[modulePath+"/internal/"+p] = true
	}
	for _, path := range src.imports {
		if dialing[path] && !guarded[path] {
			found = append(found, strconv.Itoa(src.lines[path])+": import "+path)
		}
	}
	for _, path := range src.dots {
		found = append(found, strconv.Itoa(src.lines[path])+": dot import "+path)
	}
	slices.Sort(found)
	return found
}

// guardedSources are the Go files the guard reads, tests left out,
// relative to internal/.
func guardedSources(t *testing.T) []string {
	t.Helper()
	files := slices.Clone(guardedFiles)
	for _, pkg := range guardedPackages {
		entries, err := os.ReadDir(filepath.Join("..", pkg))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if name := e.Name(); strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
				files = append(files, pkg+"/"+name)
			}
		}
	}
	return files
}

func parseSource(t *testing.T, name string, text any) source {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, text, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	return readSource(fset, file)
}

// The derived list holds the packages that dial and those that import
// them, and leaves out a package without either.
func TestTheDerivedListOfDialingPackages(t *testing.T) {
	dialing := dialingPackages(modulePackages(t))
	for _, p := range []string{"ldapauth", "proxy", "listeners", "notify", "server", "cli", "rest"} {
		if !dialing[modulePath+"/internal/"+p] {
			t.Errorf("internal/%s is not in the derived list", p)
		}
	}
	for _, p := range []string{"pystr", "model", "tasks", "routing"} {
		if dialing[modulePath+"/internal/"+p] {
			t.Errorf("internal/%s is in the derived list", p)
		}
	}
}

func TestNoOutboundCallOutsideTheList(t *testing.T) {
	dialing := dialingPackages(modulePackages(t))
	files := guardedSources(t)
	if len(files) < len(guardedPackages)+len(guardedFiles) {
		t.Fatalf("the guard reads %d files", len(files))
	}
	found := map[string]bool{}
	for _, name := range files {
		calls := outbound(parseSource(t, filepath.Join("..", name), nil), dialing)
		if len(calls) == 0 {
			continue
		}
		found[name] = true
		if _, listed := mayDial[name]; !listed {
			t.Errorf("%s may connect out outside the list of files with reasons: %s", name, strings.Join(calls, ", "))
		}
	}
	for name := range mayDial {
		if !found[name] {
			t.Errorf("%s is listed as a file that may connect out and imports nothing that does; take it off the list", name)
		}
	}
}

// The guard finds each form: a dial root, a dial name under another local
// name, a package of the derived list, and a dot import; a file without
// one gives none.
func TestTheGuardFindsEachOutboundCall(t *testing.T) {
	text := `package demo

import (
	"crypto/tls"
	web "net/http"
	"net"
	"github.com/go-ldap/ldap/v3"
	"github.com/iogentic/pipesmith-registry/internal/notify"
	. "strings"
)

var _ = notify.FromEnv
var _ = ToUpper

func demo() {
	net.Dial("tcp", "registry.example.com:443")
	web.Get("https://registry.example.com/")
	tls.Dial("tcp", "registry.example.com:443", nil)
	ldap.DialURL("ldap://ldap.example.com")
	_ = web.StatusOK
}
`
	dialing := map[string]bool{modulePath + "/internal/notify": true}
	got := strings.Join(outbound(parseSource(t, "demo.go", text), dialing), "; ")
	want := "4: crypto/tls.Dial; 5: net/http.Get; 6: import net; 7: import github.com/go-ldap/ldap/v3; " +
		"8: import github.com/iogentic/pipesmith-registry/internal/notify; 9: dot import strings"
	if got != want {
		t.Fatalf("found %s\nwant  %s", got, want)
	}
	quiet := parseSource(t, "quiet.go", "package demo\n\nimport \"net/http\"\n\nvar status = http.StatusOK\n")
	if calls := outbound(quiet, dialing); len(calls) != 0 {
		t.Fatalf("a file without a call: %v", calls)
	}
}
