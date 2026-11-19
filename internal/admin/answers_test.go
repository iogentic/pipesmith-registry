package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// answerRow is a row of answers.json: a case of section Answers of the phase
// 6 plan, or the row that the contract adds.
type answerRow struct {
	Row    string `json:"row"`
	Status int    `json:"status"`
	Form   string `json:"form"`
	Note   string `json:"note"`
}

// rowsWithoutCode are the rows of answers.json that internal/auth answers.
// No code of this package maps to them.
var rowsWithoutCode = map[int]string{
	http.StatusUnauthorized:    "no subject",
	http.StatusTooManyRequests: "sign in is slowed down",
}

func answerRows(t *testing.T) []answerRow {
	t.Helper()
	var file struct {
		Family string      `json:"family"`
		Note   string      `json:"note"`
		Rows   []answerRow `json:"rows"`
	}
	readJSON(t, "answers", &file)
	if len(file.Rows) == 0 {
		t.Fatal("answers.json holds no row")
	}
	return file.Rows
}

// foundCode is a constant of the type Code found in a source file.
type foundCode struct {
	name  string
	value Code
}

// scanCodes gives every constant of the type Code in a parsed file: one
// with the type written, one with a value Code(...), and one that repeats
// such a line of its block. A code whose value is no string literal is a
// problem, and so is a package level constant or variable whose name starts
// with Code and that the scan does not take as a code.
func scanCodes(fset *token.FileSet, file *ast.File) (found []foundCode, problems []string) {
	taken := map[*ast.Ident]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		decl, ok := n.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		var typ ast.Expr
		var values []ast.Expr
		for _, spec := range decl.Specs {
			vs := spec.(*ast.ValueSpec)
			if len(vs.Values) > 0 {
				typ, values = vs.Type, vs.Values
			}
			for i, ident := range vs.Names {
				if i >= len(values) {
					continue
				}
				value, isCode, literal := codeValue(typ, values[i])
				taken[ident] = isCode
				switch {
				case !isCode:
				case !literal:
					problems = append(problems, fmt.Sprintf("%s: the code %s has no string literal as its value",
						fset.Position(ident.Pos()), ident.Name))
				default:
					found = append(found, foundCode{ident.Name, value})
				}
			}
		}
		return false
	})
	for _, d := range file.Decls {
		decl, ok := d.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST && decl.Tok != token.VAR {
			continue
		}
		for _, spec := range decl.Specs {
			for _, ident := range spec.(*ast.ValueSpec).Names {
				if strings.HasPrefix(ident.Name, "Code") && !taken[ident] {
					problems = append(problems, fmt.Sprintf("%s: the package level %s %s is no code the scan takes; "+
						"declare a code as a constant of the type Code with a string literal", fset.Position(ident.Pos()),
						decl.Tok, ident.Name))
				}
			}
		}
	}
	return found, problems
}

func codeValue(typ, value ast.Expr) (code Code, isCode, literal bool) {
	isCode = isIdent(typ, "Code")
	if call, ok := value.(*ast.CallExpr); ok && isIdent(call.Fun, "Code") && len(call.Args) == 1 {
		isCode, value = true, call.Args[0]
	}
	lit, ok := value.(*ast.BasicLit)
	if !isCode || !ok || lit.Kind != token.STRING {
		return "", isCode, false
	}
	text, err := strconv.Unquote(lit.Value)
	if err != nil || text == "" {
		return "", true, false
	}
	return Code(text), true, true
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// sourceCodes parses the Go files of this package, tests left out, and
// gives every code by its value with the name of its constant.
func sourceCodes(t *testing.T) map[Code]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	codes := map[Code]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		found, problems := scanCodes(fset, file)
		for _, p := range problems {
			t.Error(p)
		}
		for _, c := range found {
			if other, twice := codes[c.value]; twice {
				t.Errorf("%s and %s both declare the code %q", other, c.name, c.value)
			}
			codes[c.value] = c.name
		}
	}
	return codes
}

// The scan finds a code in each form a constant can take and nothing else.
func TestSourceScanFindsEveryForm(t *testing.T) {
	const source = `package x

const (
	A Code = "a"
	B = Code("b")
	C, D Code = "c", "d"
	E = "e"
	F Code = "f"
	G
)

const H Code = "h"

func f() {
	const I Code = "i"
}

const J Code = "j" + "k"
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "x.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	found, problems := scanCodes(fset, file)
	want := []foundCode{{"A", "a"}, {"B", "b"}, {"C", "c"}, {"D", "d"}, {"F", "f"}, {"G", "f"}, {"H", "h"}, {"I", "i"}}
	if !slices.Equal(found, want) {
		t.Errorf("found %v, want %v", found, want)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "the code J") {
		t.Errorf("problems %q, want one for J", problems)
	}
}

// A package level constant or variable whose name starts with Code and
// that the scan does not take as a code gives a problem line; a code, a
// local constant and a name of another prefix do not.
func TestSourceScanFlagsCodeNames(t *testing.T) {
	const source = `package x

const (
	CodeA Code = "a"
	CodeX = CodeNameInvalid + "-x"
	Codec = 3
)

var CodeY Code = "y"

var (
	codeZ = "z"
	Other Code = "o"
)

func f() {
	const CodeLocal = 1
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "x.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	found, problems := scanCodes(fset, file)
	if !slices.Equal(found, []foundCode{{"CodeA", "a"}}) {
		t.Errorf("found %v, want CodeA only", found)
	}
	want := []string{"CodeX", "Codec", "CodeY"}
	if len(problems) != len(want) {
		t.Fatalf("problems %q, want one each for %q", problems, want)
	}
	for i, name := range want {
		if !strings.Contains(problems[i], " "+name+" ") {
			t.Errorf("problem %d is %q, want one for %s", i, problems[i], name)
		}
	}
}

// The declarations of the package give no problem line, and the scan takes
// codes from them.
func TestSourceScanOfThePackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	total := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		found, problems := scanCodes(fset, file)
		total += len(found)
		for _, p := range problems {
			t.Errorf("problem in the package: %s", p)
		}
	}
	if total != len(answers) {
		t.Errorf("the scan takes %d codes from the package, the table has %d rows", total, len(answers))
	}
}

// Every constant of the type Code has a row in the table of answers, and
// every row of the table names a code that a constant declares.
func TestEveryCodeHasARow(t *testing.T) {
	codes := sourceCodes(t)
	if codes[CodeNotFound] != "CodeNotFound" || codes[CodeTaskDisabled] != "CodeTaskDisabled" {
		t.Fatalf("the scan of the source misses codes it must find: %v", codes)
	}
	for code, name := range codes {
		if _, ok := answers[code]; !ok {
			t.Errorf("%s (%q) has no row in the table of answers", name, code)
		}
	}
	for code := range answers {
		if _, ok := codes[code]; !ok {
			t.Errorf("the table of answers has a row for %q, which no constant declares", code)
		}
	}
}

// Every row of the table matches a row of answers.json in status and body
// form. A row of the file without a code is one of the two rows of
// internal/auth, which the test names.
func TestTableMatchesAnswers(t *testing.T) {
	rows := answerRows(t)
	statuses := map[int]bool{}
	for _, r := range rows {
		if r.Row == "" || r.Form == "" || statuses[r.Status] {
			t.Errorf("answers.json: row %+v has no name, no form or a status of another row", r)
		}
		statuses[r.Status] = true
	}
	for status, name := range rowsWithoutCode {
		if !slices.ContainsFunc(rows, func(r answerRow) bool { return r.Status == status && r.Row == name }) {
			t.Errorf("answers.json has no row %d %q", status, name)
		}
	}
	matched := make([]int, len(rows))
	for code, a := range answers {
		i := slices.IndexFunc(rows, func(r answerRow) bool { return r.Status == a.status && r.Form == string(a.form) })
		if i < 0 {
			t.Errorf("%q answers %d in the form %q, which no row of answers.json has", code, a.status, a.form)
			continue
		}
		matched[i]++
	}
	for i, r := range rows {
		_, auth := rowsWithoutCode[r.Status]
		switch {
		case matched[i] == 0 && !auth:
			t.Errorf("row %d %q of answers.json has no code", r.Status, r.Row)
		case matched[i] > 0 && auth:
			t.Errorf("row %d %q belongs to internal/auth, yet %d codes map to it", r.Status, r.Row, matched[i])
		}
	}
}

// The row 405 is in answers.json with the note that the contract adds it.
func TestContractRow405(t *testing.T) {
	for _, r := range answerRows(t) {
		if r.Status == http.StatusMethodNotAllowed {
			if !strings.Contains(r.Note, "contract") || r.Form != "message" {
				t.Errorf("row 405: %+v", r)
			}
			if a := answers[CodeTaskDisabled]; a.status != http.StatusMethodNotAllowed {
				t.Errorf("%q answers %d, want 405", CodeTaskDisabled, a.status)
			}
			return
		}
	}
	t.Error("answers.json has no row 405")
}

func encoded(t *testing.T, body any) string {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Answer gives status and body as section Answers words them.
func TestAnswer(t *testing.T) {
	name := &Refusal{Code: CodeNameInvalid, Field: "name", Message: "the name x y is not allowed"}
	members := &Refusal{Code: CodeMemberMissing, Field: "group.memberNames", Message: "the repository demo-gone does not exist"}
	exists := &Refusal{Code: CodeNameExists, Message: "a repository named demo-raw exists"}
	cases := []struct {
		label  string
		err    error
		status int
		body   string
	}{
		{"field", name, 400, `[{"id":"name","message":"the name x y is not allowed"}]`},
		{"fields travel together", Refusals{name, members}, 400,
			`[{"id":"name","message":"the name x y is not allowed"},{"id":"group.memberNames","message":"the repository demo-gone does not exist"}]`},
		{"joined", errors.Join(name, members), 400,
			`[{"id":"name","message":"the name x y is not allowed"},{"id":"group.memberNames","message":"the repository demo-gone does not exist"}]`},
		{"wrapped", fmt.Errorf("save: %w", name), 400, `[{"id":"name","message":"the name x y is not allowed"}]`},
		{"no field", &Refusal{Code: CodeRoleReadOnly, Message: "the role nx-admin is built in"}, 400,
			`[{"id":"*","message":"the role nx-admin is built in"}]`},
		{"exists", exists, 409, `{"message":"a repository named demo-raw exists"}`},
		{"a refusal of another status wins", Refusals{name, exists, members}, 409, `{"message":"a repository named demo-raw exists"}`},
		{"the first of them", Refusals{&Refusal{Code: CodeNotFound, Message: "no store named demo"}, exists}, 404,
			`{"message":"no store named demo"}`},
		{"right", &Refusal{Code: CodeForbidden, Message: "the subject lacks the right"}, 403, `{"message":"the subject lacks the right"}`},
		{"request token", &Refusal{Code: CodeRequestToken}, 403, `{"message":"missing or wrong request token"}`},
		{"request token, own message", &Refusal{Code: CodeRequestToken, Message: "other"}, 403,
			`{"message":"missing or wrong request token"}`},
		{"not built", &Refusal{Code: CodeNotBuilt, Message: "the task list is not available", Area: "tasks"}, 501,
			`{"message":"the task list is not available","area":"tasks"}`},
		{"store kind", &Refusal{Code: CodeStoreKind, Message: "only file stores", Area: "blob-stores"}, 501,
			`{"message":"only file stores","area":"blob-stores"}`},
		{"unavailable", &Refusal{Code: CodeUnavailable, Message: "the server is read only"}, 503, `{"message":"the server is read only"}`},
		{"task not running", &Refusal{Code: CodeTaskNotRunning, Message: "the task demo-task is not running"}, 409,
			`{"message":"the task demo-task is not running"}`},
		{"task running", &Refusal{Code: CodeTaskRunning, Message: "the task demo-task runs already"}, 409,
			`{"message":"the task demo-task runs already"}`},
		{"task disabled", &Refusal{Code: CodeTaskDisabled, Message: "the task demo-task is disabled"}, 405,
			`{"message":"the task demo-task is disabled"}`},
		{"last administrator", &Refusal{Code: CodeLastAdministrator, Message: "m"}, 409, `{"message":"m"}`},
		{"no refusal", errors.New("disk full"), 500, `{"message":"internal error"}`},
		{"a refusal beside another error", errors.Join(name, errors.New("disk full")), 500, `{"message":"internal error"}`},
		{"a code without a row", &Refusal{Code: "made-up", Message: "m"}, 500, `{"message":"internal error"}`},
		{"a 501 without an area", Refusals{name, &Refusal{Code: CodeNotBuilt, Message: "m"}}, 500, `{"message":"internal error"}`},
		{"an empty list", Refusals{}, 500, `{"message":"internal error"}`},
		{"nil", nil, 500, `{"message":"internal error"}`},
	}
	for _, c := range cases {
		status, body := Answer(c.err)
		if status != c.status || encoded(t, body) != c.body {
			t.Errorf("%s: %d %s, want %d %s", c.label, status, encoded(t, body), c.status, c.body)
		}
	}
}

// A list of refusals reads as its messages, and errors.As finds each
// refusal in it.
func TestRefusalsAsError(t *testing.T) {
	a := &Refusal{Code: CodeNameInvalid, Field: "name", Message: "first"}
	b := &Refusal{Code: CodeNameExists, Message: "second"}
	var err error = Refusals{a, b}
	if err.Error() != "first; second" || a.Error() != "first" {
		t.Errorf("messages %q and %q", err.Error(), a.Error())
	}
	var found *Refusal
	if !errors.As(err, &found) || found != a {
		t.Errorf("errors.As found %v, want the first refusal", found)
	}
	if !errors.Is(err, b) {
		t.Error("errors.Is does not find the second refusal")
	}
	if (Refusals{}).err() != nil || (Refusals{a}).err() == nil {
		t.Error("err of a list: nil exactly when the list is empty")
	}
}
