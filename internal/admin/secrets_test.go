package admin

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const canary = "canary-5e1f0c"

func TestSecretVectors(t *testing.T) {
	var file struct {
		Family string `json:"family"`
		Note   string `json:"note"`
		Cases  []struct {
			Row     string         `json:"row"`
			Op      string         `json:"op"` // guard, redact, save
			Kind    Kind           `json:"kind"`
			Create  bool           `json:"create"`
			Created Kind           `json:"created"`
			Answer  any            `json:"answer"`
			Leaks   *[]string      `json:"leaks"`
			Record  map[string]any `json:"record"`
			Expect  map[string]any `json:"expect"`
			Body    map[string]any `json:"body"`
			Path    string         `json:"path"`
			Action  SecretSave     `json:"action"`
		} `json:"cases"`
	}
	readJSON(t, "secrets", &file)
	counts := tally{}
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s %s)", i, c.Row, c.Op, c.Kind)
		switch c.Op {
		case "guard":
			if c.Leaks == nil {
				t.Fatalf("%s: no key leaks", label)
			}
			got := Leaks(c.Answer, c.Created)
			if !slices.Equal(got, *c.Leaks) {
				t.Errorf("%s: leaks %q, want %q", label, got, *c.Leaks)
			}
			counts.add(c.Row, len(*c.Leaks) > 0)
		case "redact":
			before := fmt.Sprint(c.Record)
			got := Redact(c.Kind, c.Record, c.Create)
			if !reflect.DeepEqual(got, c.Expect) {
				t.Errorf("%s: %v, want %v", label, got, c.Expect)
			}
			created := Kind("")
			if c.Create {
				created = c.Kind
			}
			if leaks := Leaks(got, created); len(leaks) > 0 {
				t.Errorf("%s: the prepared record holds %q", label, leaks)
			}
			if fmt.Sprint(c.Record) != before {
				t.Errorf("%s: Redact changed the record it was given", label)
			}
			counts.add(c.Row, false)
		case "save":
			if got := SecretOnSave(c.Body, c.Path); got != c.Action {
				t.Errorf("%s: %q, want %q", label, got, c.Action)
			}
			counts.add(c.Row, c.Action == KeepSecret)
		default:
			t.Fatalf("%s: unknown op", label)
		}
	}
	counts.check(t, "secrets", "R8", "R9")
}

// nested is an object that holds value at the dot path.
func nested(path string, value any) map[string]any {
	keys := strings.Split(path, ".")
	out := map[string]any{keys[len(keys)-1]: value}
	for i := len(keys) - 2; i >= 0; i-- {
		out = map[string]any{keys[i]: out}
	}
	return out
}

// The guard finds a canary at each path of the table, alone and inside a
// list; the token secret passes only at the top of the answer of its
// create.
func TestGuardFindsEverySecretField(t *testing.T) {
	fields := SecretFields()
	if len(fields) != 9 {
		t.Fatalf("%d secret fields, want the 9 of the contract", len(fields))
	}
	for _, f := range fields {
		answer := nested(f.Path, canary)
		if got := Leaks(answer, ""); !slices.Equal(got, []string{f.Path}) {
			t.Errorf("%s %s: leaks %q", f.Kind, f.Path, got)
		}
		if got := Leaks([]any{"x", answer}, ""); !slices.Equal(got, []string{"[1]." + f.Path}) {
			t.Errorf("%s %s in a list: leaks %q", f.Kind, f.Path, got)
		}
		got := Leaks(answer, f.Kind)
		if f.Shown == ShownCreate && len(got) != 0 || f.Shown != ShownCreate && len(got) != 1 {
			t.Errorf("%s %s in the answer of its create: leaks %q", f.Kind, f.Path, got)
		}
	}
}

// A record prepared for an answer holds no secret and keeps every other
// field; the record given stays as it was.
func TestPreparedRecordHoldsNoSecret(t *testing.T) {
	kinds := map[Kind]bool{}
	for _, f := range SecretFields() {
		kinds[f.Kind] = true
	}
	for kind := range kinds {
		record := map[string]any{"name": "demo-" + string(kind), "connection": map[string]any{"host": "ldap.example.com"},
			"httpClient": map[string]any{"authentication": map[string]any{"type": "username", "username": "demo"}}}
		for _, f := range SecretFields() {
			if f.Kind != kind {
				continue
			}
			parent, key := parentOf(record, f.Path)
			if parent == nil {
				parent = record
			}
			parent[key] = canary
		}
		prepared := Redact(kind, record, false)
		if leaks := Leaks(prepared, ""); len(leaks) > 0 {
			t.Errorf("%s: the prepared record holds %q", kind, leaks)
		}
		if prepared["name"] != record["name"] || !strings.Contains(fmt.Sprint(prepared), "ldap.example.com") {
			t.Errorf("%s: the prepared record lost a field: %v", kind, prepared)
		}
		if !strings.Contains(fmt.Sprint(record), canary) {
			t.Errorf("%s: Redact changed the record it was given", kind)
		}
		for _, f := range SecretFields() {
			if f.Kind != kind {
				continue
			}
			parent, key := parentOf(prepared, f.Path)
			value, present := parent[key]
			if f.Shown == ShownNull && (!present || value != nil) || f.Shown != ShownNull && present {
				t.Errorf("%s %s: present %v with %v, want %s", kind, f.Path, present, value, f.Shown)
			}
		}
	}
	created := Redact(KindToken, map[string]any{"id": "demo-token", "secret": canary}, true)
	if created["secret"] != canary || len(Leaks(created, KindToken)) != 0 || len(Leaks(created, "")) != 1 {
		t.Errorf("the answer of a token create: %v", created)
	}
}

// The secret words are the ones of internal/importer, read from its source.
func TestSecretWordsOfTheImport(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../importer/importer.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var words []string
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "secretWords" || len(spec.Values) != 1 {
			return true
		}
		if list, ok := spec.Values[0].(*ast.CompositeLit); ok {
			for _, elt := range list.Elts {
				if lit, ok := elt.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					word, _ := strconv.Unquote(lit.Value)
					words = append(words, word)
				}
			}
		}
		return false
	})
	if len(words) == 0 || !slices.Equal(words, secretWords) {
		t.Errorf("internal/importer marks %q, this package %q", words, secretWords)
	}
}

// The guard walks the JSON form of any value: a canary inside a list of
// typed objects, a map of strings and a struct with JSON tags is found, and
// the same containers without a secret are clean.
func TestGuardWalksAnyContainer(t *testing.T) {
	type authentication struct {
		Type        string `json:"type"`
		BearerToken string `json:"bearerToken"`
	}
	type client struct {
		Authentication authentication `json:"authentication"`
	}
	type repository struct {
		Name       string `json:"name"`
		HTTPClient client `json:"httpClient"`
	}
	cases := []struct {
		label  string
		answer any
		leaks  []string
	}{
		{"list of objects", []map[string]any{{"name": "demo-ldap"}, {"name": "demo-b", "authPassword": canary}}, []string{"[1].authPassword"}},
		{"map of strings", map[string]string{"name": "demo-ldap", "authPassword": canary}, []string{"authPassword"}},
		{"struct", repository{Name: "demo-proxy", HTTPClient: client{authentication{Type: "bearerToken", BearerToken: canary}}},
			[]string{"httpClient.authentication.bearerToken"}},
		{"pointer to struct", &repository{HTTPClient: client{authentication{BearerToken: canary}}},
			[]string{"httpClient.authentication.bearerToken"}},
		{"clean list of objects", []map[string]any{{"name": "demo-ldap", "authPassword": nil}}, nil},
		{"clean map of strings", map[string]string{"name": "demo-ldap", "authPassword": ""}, nil},
		{"clean struct", repository{Name: "demo-proxy"}, nil},
		{"nil", nil, nil},
	}
	for _, c := range cases {
		if got := Leaks(c.answer, ""); !slices.Equal(got, c.leaks) {
			t.Errorf("%s: leaks %q, want %q", c.label, got, c.leaks)
		}
	}
}

// A value without a JSON form is never clean: the guard reports it.
func TestGuardReportsAValueWithoutJSONForm(t *testing.T) {
	for _, v := range []any{make(chan int), func() {}, map[string]any{"name": "demo", "x": math.NaN()}, complex(1, 2)} {
		got := Leaks(v, "")
		if len(got) != 1 || !strings.Contains(got[0], "no JSON form") {
			t.Errorf("%T: leaks %q, want one entry that says the value has no JSON form", v, got)
		}
	}
}
