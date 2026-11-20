package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/export"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/rest"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
	"github.com/iogentic/pipesmith-registry/internal/version"
)

const testVersion = "9.8.7"

// openAPIOf parses the document as the builder writes it for testVersion.
func openAPIOf(t *testing.T) map[string]any {
	t.Helper()
	data, err := buildOpenAPI(testVersion)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	return doc
}

func objectAt(t *testing.T, v any, where string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object: %v", where, v)
	}
	return m
}

// operations are the operations of the document by "METHOD path".
func operations(t *testing.T, doc map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for path, item := range objectAt(t, doc["paths"], "paths") {
		for method, op := range objectAt(t, item, path) {
			out[strings.ToUpper(method)+" "+path] = objectAt(t, op, method+" "+path)
		}
	}
	return out
}

var pathParameter = regexp.MustCompile(`\{([^/{}]+)\}`)

// sampled puts a sample value into every parameter of a path.
func sampled(path string) string { return pathParameter.ReplaceAllString(path, "sample") }

// shownPath is the path of a route in the document.
func shownPath(t *testing.T, r apiRoute) string {
	t.Helper()
	if r.refused {
		return r.template
	}
	w, ok := operationWords[r.key()]
	if !ok {
		t.Fatalf("%s has no words", r.key())
	}
	return w.path
}

func TestEveryRouteHasAnOperation(t *testing.T) {
	ops := operations(t, openAPIOf(t))
	for _, r := range docRoutes(t) {
		if !r.refused {
			if _, ok := operationWords[r.key()]; !ok {
				t.Errorf("%s has no entry in operationWords", r.key())
				continue
			}
		}
		if _, ok := ops[r.method+" "+shownPath(t, r)]; !ok {
			t.Errorf("%s is not in the document as %s %s", r.key(), r.method, shownPath(t, r))
		}
	}
}

func TestEveryOperationHasARoute(t *testing.T) {
	table := docRoutes(t)
	keys := map[string]bool{}
	for _, r := range table {
		if !r.refused {
			keys[r.key()] = true
		}
	}
	for key := range operationWords {
		if !keys[key] {
			t.Errorf("operationWords holds %s, which no route serves", key)
		}
	}
	ops := operations(t, openAPIOf(t))
	for op := range ops {
		method, path, _ := strings.Cut(op, " ")
		sample := sampled(path)
		found := false
		for _, r := range table {
			if r.method == method && r.pattern.MatchString(sample) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s of the document: no route lists %s and matches %s", op, method, sample)
		}
	}
	if len(ops) != len(table) {
		t.Errorf("the document holds %d operations, the route tables %d", len(ops), len(table))
	}
}

// refs collects every $ref below v.
func refs(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if k == "$ref" {
				if s, ok := e.(string); ok {
					*out = append(*out, s)
				}
			}
			refs(e, out)
		}
	case []any:
		for _, e := range x {
			refs(e, out)
		}
	}
}

// resolve follows a reference of the form #/a/b inside the document.
func resolve(doc map[string]any, ref string) bool {
	if !strings.HasPrefix(ref, "#/") {
		return false
	}
	var at any = doc
	for _, part := range strings.Split(ref[2:], "/") {
		m, ok := at.(map[string]any)
		if !ok {
			return false
		}
		if at, ok = m[part]; !ok {
			return false
		}
	}
	return true
}

func TestOpenAPIDocumentIsValid(t *testing.T) {
	doc := openAPIOf(t)
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi %v, want 3.1.0", doc["openapi"])
	}
	if v := objectAt(t, doc["info"], "info")["version"]; v != testVersion {
		t.Errorf("info.version %v, want %s", v, testVersion)
	}
	tags := map[string]bool{}
	for _, tag := range doc["tags"].([]any) {
		tags[objectAt(t, tag, "tag")["name"].(string)] = true
	}
	ids := map[string]string{}
	ops := operations(t, doc)
	for name, op := range ops {
		id, _ := op["operationId"].(string)
		if id == "" {
			t.Errorf("%s has no operationId", name)
		} else if other, seen := ids[id]; seen {
			t.Errorf("%s and %s share the operationId %s", name, other, id)
		}
		ids[id] = name
		if s, _ := op["summary"].(string); s == "" {
			t.Errorf("%s has no summary", name)
		}
		list, _ := op["tags"].([]any)
		if len(list) == 0 {
			t.Errorf("%s has no tag", name)
		}
		for _, tag := range list {
			if !tags[tag.(string)] {
				t.Errorf("%s has the tag %v, which the tag list lacks", name, tag)
			}
		}
		if answers, _ := op["responses"].(map[string]any); len(answers) == 0 {
			t.Errorf("%s has no answer", name)
		}
		_, path, _ := strings.Cut(name, " ")
		declared := map[string]bool{}
		params, _ := op["parameters"].([]any)
		for _, p := range params {
			m := objectAt(t, p, name+" parameter")
			if m["in"] == "path" {
				declared[m["name"].(string)] = m["required"] == true
			}
		}
		for _, m := range pathParameter.FindAllStringSubmatch(path, -1) {
			if required, ok := declared[m[1]]; !ok || !required {
				t.Errorf("%s: the path parameter %s is not declared as required", name, m[1])
			}
			delete(declared, m[1])
		}
		for extra := range declared {
			t.Errorf("%s declares the path parameter %s, which its path lacks", name, extra)
		}
	}
	var all []string
	refs(doc, &all)
	if len(all) == 0 {
		t.Error("the document holds no $ref")
	}
	for _, ref := range all {
		if !resolve(doc, ref) {
			t.Errorf("$ref %s does not resolve", ref)
		}
	}
	shapes := map[string]string{}
	for path := range objectAt(t, doc["paths"], "paths") {
		shape := pathParameter.ReplaceAllString(path, "{}")
		if other, seen := shapes[shape]; seen {
			t.Errorf("the paths %s and %s differ only in the names of their parameters", path, other)
		}
		shapes[shape] = path
	}
	for _, r := range refused {
		op, ok := ops[r.Method+" "+r.Path]
		if !ok {
			t.Errorf("the refusal %s %s is not in the document", r.Method, r.Path)
			continue
		}
		answers := objectAt(t, op["responses"], r.Method+" "+r.Path)
		_, served := answers["501"]
		_, unsigned := answers["401"]
		_, throttled := answers["429"]
		if !served || !unsigned || !throttled || len(answers) != 3 {
			t.Errorf("%s %s answers %v, want 401, 429 and 501", r.Method, r.Path, answers)
		}
		if op["x-pipesmith-refused"] != true || op["x-pipesmith-area"] != r.Area {
			t.Errorf("%s %s: refused %v, area %v, want true and %s", r.Method, r.Path, op["x-pipesmith-refused"], op["x-pipesmith-area"], r.Area)
		}
	}
	// A refusal of writes off is in the document: as the served operation of
	// writes on with its 501 and the area, or as a refusal of its own.
	for _, r := range refusedOf(writeRefusals) {
		op, ok := ops[r.Method+" "+r.Path]
		if !ok {
			t.Errorf("the refusal of writes off %s %s is not in the document", r.Method, r.Path)
			continue
		}
		answer, _ := objectAt(t, op["responses"], r.Method+" "+r.Path)["501"].(map[string]any)
		text, _ := answer["description"].(string)
		if op["x-pipesmith-refused"] != true && (op["x-pipesmith-needs"] != "state.sqlite, writes" || !strings.Contains(text, "area "+r.Area)) {
			t.Errorf("%s %s: needs %v, 501 %q, want the needs of the writes and a 501 that names the area %s", r.Method, r.Path,
				op["x-pipesmith-needs"], text, r.Area)
		}
	}
}

func TestOpenAPIIsStable(t *testing.T) {
	first, err := buildOpenAPI(testVersion)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := buildOpenAPI(testVersion)
	if !bytes.Equal(first, second) {
		t.Fatal("two builds of the document differ")
	}
	if !bytes.HasSuffix(first, []byte("}\n")) || !bytes.HasPrefix(first, []byte("{\n  \"")) {
		t.Fatalf("the document is not indented by two spaces with a final newline: %q ... %q", first[:20], first[len(first)-20:])
	}
}

func TestPermissionTextFollowsTheRule(t *testing.T) {
	cases := []struct {
		kind   ruleKind
		method string
		path   string
		want   string
		secure bool
	}{
		{rulePublic, "GET", "/api/health", "public", false},
		{ruleSignedIn, "GET", "/api/repositories", "signed in", true},
		{ruleApp, "POST", "/api/scan", "* (every permission)", true},
		{ruleApp, "GET", "/service/rest/v1/security/user-sources", "users:read or roles:read", true},
		{ruleView, "GET", "/api/browse/{repository}", "view of the repository (browse)", true},
		{ruleAdmin, "GET", "/service/rest/v1/repositories/{segment}/{type}/{name}", "administration of the repository", true},
		{ruleAssetID, "GET", "/service/rest/v1/assets/{id}", "view of the repository of the asset (browse)", true},
		{ruleCreate, "POST", "/service/rest/v1/repositories/{segment}/{type}", "administration of any repository of the format of the path (add)", true},
		{ruleClient, "GET", "/repository/{repository}/{path}", "view of the repository (read below /repository/, browse below the browse route)", true},
		{ruleContent, "DELETE", "/service/rest/v1/assets/{id}", "view of the repository of the id (delete)", true},
		{ruleContent, "POST", "/service/rest/v1/components", "view of the repository of the query parameter repository (add)", true},
		{ruleListing, "GET", "/service/rest/v1/assets", "view of the repository of the query parameter repository (browse)", true},
		{ruleListing, "GET", "/service/rest/v1/components/{id}", "view of the repository of the id (browse)", true},
	}
	ops := operations(t, openAPIOf(t))
	tested := map[ruleKind]bool{}
	for _, c := range cases {
		tested[c.kind] = true
		op, ok := ops[c.method+" "+c.path]
		if !ok {
			t.Errorf("%s %s is not in the document", c.method, c.path)
			continue
		}
		if got := op["x-pipesmith-permission"]; got != c.want {
			t.Errorf("%s %s: x-pipesmith-permission %q, want %q", c.method, c.path, got, c.want)
		}
		if _, has := op["security"]; has != c.secure {
			t.Errorf("%s %s: security present %v, want %v", c.method, c.path, has, c.secure)
		}
	}
	var missing []string
	for _, r := range docRoutes(t) {
		if !tested[r.rule.kind] {
			missing = append(missing, r.key())
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("routes of a rule kind no case covers: %v", slices.Compact(missing))
	}
}

// forbiddenNames are the name of the old product and of its maker, built
// from parts so that this file does not hold them as words.
var forbiddenNames = []string{"nex" + "us", "sona" + "type"}

func TestOpenAPIHoldsNoOldProductName(t *testing.T) {
	data, err := buildOpenAPI(testVersion)
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string][]byte{"the document": data}
	for _, file := range []string{"../../docs/openapi.json", "../../docs/api.html", "../../docs/api.js"} {
		if texts[file], err = os.ReadFile(file); err != nil {
			t.Fatal(err)
		}
	}
	for what, text := range texts {
		lower := bytes.ToLower(text)
		for _, name := range forbiddenNames {
			if n := bytes.Count(lower, []byte(name)); n > 0 {
				t.Errorf("%s holds a forbidden name %d times", what, n)
			}
		}
	}
}

// firstRoute is the index of the route the router runs for a method and a
// path: the first whose pattern matches and whose methods list the method.
func firstRoute(table []route, method, path string) int {
	for i, rt := range table {
		if rt.Pattern.MatchString(path) && slices.Contains(rt.Methods, method) {
			return i
		}
	}
	return -1
}

func sameRoute(rt route, r apiRoute) bool {
	return rt.Pattern.String() == r.pattern.String() && slices.Contains(rt.Methods, r.method)
}

func TestEveryOperationIsTheFirstMatch(t *testing.T) {
	for _, c := range []struct {
		name   string
		table  []route
		off    bool // security off
		writes bool // writes on
	}{{"security on", securedRoutes, false, false}, {"security off", routes, true, false}, {"writes on", writingRoutes, false, true}} {
		seen := map[string]string{}
		for _, r := range docRoutes(t) {
			if c.off && r.secured {
				continue
			}
			path := shownPath(t, r)
			i := firstRoute(c.table, r.method, sampled(path))
			if r.writes && !c.writes {
				// writes off: the sample reaches the refusal that the words name, or no route
				switch {
				case r.offPattern == nil && i >= 0:
					t.Errorf("%s: %s %s reaches %s, want no route as its words say", c.name, r.method, path, c.table[i].Pattern)
				case r.offPattern != nil && (i < 0 || c.table[i].Pattern.String() != r.offPattern.String()):
					t.Errorf("%s: %s %s does not reach the refusal %s of its words", c.name, r.method, path, r.offPattern)
				}
				continue
			}
			if i < 0 {
				t.Errorf("%s: %s %s reaches no route", c.name, r.method, path)
				continue
			}
			if shadowed[r.method+" "+path] {
				continue // TestShadowedRefusalsAnswerAlike checks these
			}
			if !sameRoute(c.table[i], r) {
				t.Errorf("%s: %s %s reaches %s first, not its own route %s", c.name, r.method, path, c.table[i].Pattern, r.pattern)
				continue
			}
			key := fmt.Sprint(i, " ", r.method)
			if other, twice := seen[key]; twice && !alternatives(other, path) {
				t.Errorf("%s: %s and %s resolve to the same route and method", c.name, other, path)
			}
			seen[key] = path
		}
	}
}

// shadowed are refusals that an earlier refusal of the same area reaches
// first, so that the answer is the same 501. Pinned, so that a new case
// fails.
var shadowed = map[string]bool{}

func TestShadowedRefusalsAnswerAlike(t *testing.T) {
	found := map[string]bool{}
	for _, r := range docRoutes(t) {
		if r.writes {
			continue // TestEveryOperationIsTheFirstMatch checks what these reach with writes off
		}
		path := shownPath(t, r)
		i := firstRoute(securedRoutes, r.method, sampled(path))
		if i < 0 || sameRoute(securedRoutes[i], r) {
			continue
		}
		found[r.method+" "+path] = true
		got := do(t, newSecureApp(t, newFakeSecurity()).addr(), r.method, sampled(path), nil, "basic", asAdmin)
		if got.status != 501 || !strings.Contains(string(got.body), `"area":"`+r.area+`"`) {
			t.Errorf("%s %s answers %d %s, want 501 with the area %s", r.method, path, got.status, got.body, r.area)
		}
	}
	for key := range shadowed {
		if !found[key] {
			t.Errorf("%s is listed as shadowed but reaches its own route", key)
		}
	}
}

// alternatives are two paths of one refusal template, which differ in the
// words of its alternative part alone.
func alternatives(a, b string) bool {
	for _, d := range refusalTable {
		paths := d.paths()
		if len(paths) > 1 && slices.Contains(paths, a) && slices.Contains(paths, b) {
			return true
		}
	}
	return false
}

// queryReads are the forms in which the handlers read a query parameter.
var queryReads = []*regexp.Regexp{
	regexp.MustCompile(`params\(\)\.get\("([^"]+)"\)`),
	regexp.MustCompile(`\bq\.get\("([^"]+)"\)`),
	regexp.MustCompile(`\bq\.Get\("([^"]+)"\)`),
	regexp.MustCompile(`\bq\["([^"]+)"\]`),
	regexp.MustCompile(`intParam\([^,]+, "([^"]+)"`),
}

// searchNames are the keys of the name tables of the search, which reads
// every parameter of the query by name.
var (
	searchTable = regexp.MustCompile(`(?s)var (served|deferred) = map\[string\]bool\{(.*?)\n\}`)
	searchKey   = regexp.MustCompile(`"([^"]+)": true`)
)

func TestEveryQueryParameterIsDocumented(t *testing.T) {
	read := map[string]string{} // name -> file:line of the first read
	for _, dir := range []string{".", "../rest"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			note := func(name string, offset int) {
				if _, ok := read[name]; !ok {
					read[name] = fmt.Sprintf("%s:%d", file, strings.Count(text[:offset], "\n")+1)
				}
			}
			for _, form := range queryReads {
				for _, m := range form.FindAllStringSubmatchIndex(text, -1) {
					note(text[m[2]:m[3]], m[0])
				}
			}
			for _, block := range searchTable.FindAllStringSubmatchIndex(text, -1) {
				for _, m := range searchKey.FindAllStringSubmatchIndex(text[block[4]:block[5]], -1) {
					note(text[block[4]+m[2]:block[4]+m[3]], block[4]+m[0])
				}
			}
		}
	}
	if len(read) == 0 {
		t.Fatal("the scan found no query read")
	}
	documented := map[string]string{}
	for key, w := range operationWords {
		for _, p := range w.params {
			if p.in == "query" {
				documented[p.name] = key
			}
		}
	}
	for name, where := range read {
		if _, ok := documented[name]; !ok {
			t.Errorf("%s reads the query parameter %s, which no operation documents", where, name)
		}
	}
	for name, key := range documented {
		if _, ok := read[name]; !ok {
			t.Errorf("%s documents the query parameter %s, which no handler reads", key, name)
		}
	}
}

// kitSampled puts into every parameter of a path a name that the stores and
// the snapshot of startSecured and startWriting hold, so that an operation
// with a path parameter reaches its object. A refused operation answers
// before it reads a parameter: its other parameters get "sample".
func kitSampled(t *testing.T, path string, refused bool) string {
	t.Helper()
	component, ok := rest.ComponentID("demo-raw", "raw", "docs/readme.txt")
	if !ok {
		t.Fatal("no component id for demo-raw docs/readme.txt")
	}
	values := map[string]string{"repository": "demo-raw", "path": "docs/readme.txt", "user": "demo-admin", "userId": "demo-admin", "token": "sample",
		"format": "raw", "segment": "raw", "type": "hosted", "name": "demo-raw", "id": rest.AssetID("demo-raw", "docs/readme.txt"), "nodeId": "0000"}
	switch {
	case strings.Contains(path, "/-/"):
		values["repository"] = "demo-npm"
	case strings.HasSuffix(path, "/v2/token"):
		values["repository"] = "demo-docker"
	case strings.HasPrefix(path, "/service/rest/repository/browse/"):
		values["path"] = "docs/"
	case strings.HasPrefix(path, "/service/rest/v1/blobstores/"):
		values["name"] = "store-a"
	case strings.HasPrefix(path, "/service/rest/v1/security/ldap/"):
		values["name"] = "corp"
	case strings.HasPrefix(path, "/service/rest/v1/security/privileges/"):
		values["name"] = "demo-all"
	case strings.HasPrefix(path, "/service/rest/v1/security/roles/"):
		values["id"] = "demo-administrators"
	case strings.HasPrefix(path, "/service/rest/v1/components/"):
		values["id"] = component
	}
	return pathParameter.ReplaceAllStringFunc(path, func(m string) string {
		value, known := values[m[1:len(m)-1]]
		if !known && refused {
			return "sample"
		}
		if !known {
			t.Fatalf("%s: no sample for the parameter %s", path, m)
		}
		return value
	})
}

// statusSecurity is the fake security with the session handler of
// auth.Service, so that /api/session answers as in the binary.
type statusSecurity struct {
	*fakeSecurity
	session http.Handler
}

func (s statusSecurity) SessionHandler() http.Handler { return s.session }

// documentedStatuses are the statuses of every operation by "METHOD path".
func documentedStatuses(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for name, op := range operations(t, openAPIOf(t)) {
		out[name] = map[string]bool{}
		for status := range objectAt(t, op["responses"], name) {
			out[name][status] = true
		}
	}
	return out
}

func TestEveryAnsweredStatusIsDocumented(t *testing.T) {
	service, err := auth.New(auth.Options{Snapshot: secureSnapshot()})
	if err != nil {
		t.Fatal(err)
	}
	security := statusSecurity{fakeSecurity: newFakeSecurity(), session: service.SessionHandler()}
	a := startSecured(t, security)
	modes := []struct {
		name string
		app  *app
		off  bool // security off: the operations that need state.sqlite are no route
	}{
		{"security off", startSecured(t, nil), true},
		{"security on", a, false},
		{"writes on", startWriting(t, security, true), false},
	}
	documented := documentedStatuses(t)
	callers := map[string][]string{"no credentials": nil, "no permission": {"basic", asNobody}, "administrator": {"basic", asAdmin},
		"throttled": {"basic", "demo-throttled:any"}}
	checked := 0
	for _, mode := range modes {
		for _, r := range docRoutes(t) {
			if mode.off && r.secured {
				continue
			}
			path := shownPath(t, r)
			for who, headers := range callers {
				got := do(t, mode.app.addr(), r.method, kitSampled(t, path, r.refused), nil, headers...)
				checked++
				if !documented[r.method+" "+path][strconv.Itoa(got.status)] {
					t.Errorf("%s: %s %s as %s answered %d, which the document does not list: %q", mode.name, r.method, path, who, got.status, got.body)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no operation was sent")
	}
	// Answers that need a body or a value the loop above does not send.
	admin := []string{"basic", asAdmin}
	scan := append([]string{"X-Artifact-Repository", "1", "Host", "localhost:" + a.port()}, admin...)
	type bodyCase struct {
		method, path, target string
		body                 []byte
		headers              []string
		want                 int
	}
	check := func(addr string, cases []bodyCase) {
		t.Helper()
		for _, c := range cases {
			got := do(t, addr, c.method, c.target, c.body, c.headers...)
			if got.status != c.want {
				t.Errorf("%s %s answered %d, want %d: %q", c.method, c.target, got.status, c.want, got.body)
			}
			if !documented[c.method+" "+c.path][strconv.Itoa(got.status)] {
				t.Errorf("%s %s answered %d, which the document does not list: %q", c.method, c.target, got.status, got.body)
			}
		}
	}
	check(a.addr(), []bodyCase{
		{"POST", "/api/scan", "/api/scan", bytes.Repeat([]byte("a"), maxScanBody+1), scan, 413},
		{"GET", "/service/rest/v1/search", "/service/rest/v1/search?q=%FF", nil, admin, 400},
		{"GET", "/service/rest/v1/search", "/service/rest/v1/search?name=a%00b", nil, admin, 400},
	})
	// Writes on, over hosted repositories with a write policy: a redeploy
	// that the policy refuses, a body past the limit, refusals of an upload
	// and of a delete, and npm login bodies.
	attributes := restAttributes()
	attributes["demo-raw"] = map[string]any{"storage": map[string]any{"writePolicy": "ALLOW_ONCE"}, "max_body_bytes": float64(8)}
	w := startREST(t, attributes)
	const (
		file     = "/repository/{repository}/{path}"
		upload   = "/service/rest/v1/components"
		asset    = "/service/rest/v1/assets/{id}"
		npmLogin = "/repository/{repository}/-/user/org.couchdb.user:{user}"
	)
	missing := base64.RawURLEncoding.EncodeToString([]byte("demo-raw:docs/none.txt"))
	login := "/repository/demo-npm/-/user/org.couchdb.user:demo"
	check(w.addr(), []bodyCase{
		{"PUT", file, "/repository/demo-raw/docs/once.txt", []byte("payload"), admin, 201},
		{"PUT", file, "/repository/demo-raw/docs/once.txt", []byte("payload"), admin, 400},
		{"PUT", file, "/repository/demo-raw/docs/large.txt", []byte("too large"), admin, 413},
		{"PUT", file, "/repository/demo-group/a.jar", []byte("payload"), admin, 405},
		{"POST", upload, upload + "?repository=demo-npm", []byte("x"), append([]string{"Content-Type", "text/plain"}, admin...), 415},
		{"POST", upload, upload, []byte("x"), admin, 422},
		{"POST", upload, upload + "?repository=demo-group", []byte("x"), admin, 400},
		{"POST", upload, upload + "?repository=nothing", []byte("x"), admin, 404},
		{"DELETE", asset, "/service/rest/v1/assets/" + missing, nil, admin, 404},
		{"DELETE", asset, "/service/rest/v1/assets/%21", nil, admin, 422},
		{"PUT", npmLogin, login, bytes.Repeat([]byte(" "), 64<<10+1), nil, 413},
		{"PUT", npmLogin, login, []byte(`{"name":"other","password":"x"}`), nil, 400},
	})
	// The export of a folder: its refusals, a limit, a stored path no
	// archive may carry, and the archive and the dry answer.
	const exportPath = "/api/export/{repository}"
	plain := startExport(t, exportOptions{})
	check(plain.addr, []bodyCase{
		{"GET", exportPath, exportTarget("demo-raw", "path", "docs"), nil, nil, 200},
		{"GET", exportPath, exportTarget("demo-raw", "path", "docs", "dry", "1"), nil, nil, 200},
		{"GET", exportPath, exportTarget("demo-raw", "dry", "yes"), nil, nil, 400},
		{"GET", exportPath, exportTarget("demo-raw", "form", "tar"), nil, nil, 400},
		{"GET", exportPath, exportTarget("demo-raw", "path", "../etc"), nil, nil, 400},
		{"GET", exportPath, exportTarget("demo-raw", "path", "nothing/here"), nil, nil, 404},
		{"GET", exportPath, exportTarget("demo-docker", "path", "docs"), nil, nil, 404},
	})
	limited := startExport(t, exportOptions{limits: export.Limits{MaxFiles: 1, MaxBytes: 10}})
	check(limited.addr, []bodyCase{{"GET", exportPath, exportTarget("demo-raw", "path", "docs"), nil, nil, 413}})
	unfit := startExport(t, exportOptions{blobs: []testkit.BlobSpec{spec("demo-raw", `docs/a\b.txt`, []byte("bad"), "text/plain")},
		defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}})
	check(unfit.addr, []bodyCase{{"GET", exportPath, exportTarget("demo-raw", "path", "docs"), nil, nil, 409}})
}

var updateOpenAPI = flag.Bool("update", false, "rewrite docs/openapi.json from the route tables")

const openAPIFile = "../../docs/openapi.json"

// TestOpenAPIFile compares docs/openapi.json with the document of the
// version in VERSION, which a release build carries.
func TestOpenAPIFile(t *testing.T) {
	v, err := os.ReadFile("../../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	want, err := buildOpenAPI(strings.TrimSpace(string(v)))
	if err != nil {
		t.Fatal(err)
	}
	if *updateOpenAPI {
		if err := os.WriteFile(openAPIFile, want, 0o644); err != nil { // #nosec G306 -- a public file of the repository
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(openAPIFile)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("docs/openapi.json differs from the route tables (%v); rewrite it with"+
			" tools/go.sh run 'go test -count=1 -run TestOpenAPIFile ./internal/server -update'"+
			" and then python3 tools/sync_manual.py", err)
	}
}

func TestOpenAPIIsServedInBothModes(t *testing.T) {
	want, err := buildOpenAPI(version.Version)
	if err != nil {
		t.Fatal(err)
	}
	for name, security := range map[string]*fakeSecurity{"off": nil, "on": newFakeSecurity()} {
		a := newSecureApp(t, security)
		got := a.send("GET", "/api/openapi.json")
		if got.status != 200 || !bytes.Equal(got.body, want) {
			t.Fatalf("security %s: GET /api/openapi.json without credentials: %d, %d bytes, want 200 and the document", name, got.status, len(got.body))
		}
		for header, value := range map[string]string{"Content-Type": "application/json", "X-Content-Type-Options": "nosniff", "Cache-Control": "no-cache"} {
			if got.header.Get(header) != value {
				t.Errorf("security %s: %s %q, want %q", name, header, got.header.Get(header), value)
			}
		}
		etag := got.header.Get("ETag")
		if etag == "" {
			t.Fatalf("security %s: no ETag", name)
		}
		if again := a.send("GET", "/api/openapi.json", "If-None-Match", etag); again.status != 304 || len(again.body) != 0 {
			t.Errorf("security %s: GET with If-None-Match: %d, %d bytes, want 304 and no body", name, again.status, len(again.body))
		}
		if other := a.send("GET", "/api/openapi.json", "If-None-Match", `"other"`); other.status != 200 {
			t.Errorf("security %s: GET with another tag: %d, want 200", name, other.status)
		}
		if head := a.send("HEAD", "/api/openapi.json"); head.status != 405 {
			t.Errorf("security %s: HEAD answered %d, want 405 as for the other GET routes", name, head.status)
		}
		if gate := a.send("GET", "/api/openapi.json", "Host", "other.example"); gate.status != 403 {
			t.Errorf("security %s: GET with another Host: %d, want 403", name, gate.status)
		}
	}
}

// The cookie and header names of the document are the ones of internal/auth.
func TestOpenAPINamesTheCookieAndHeaderOfAuth(t *testing.T) {
	if sessionCookie != auth.CookieName {
		t.Errorf("sessionCookie is %q, auth.CookieName is %q", sessionCookie, auth.CookieName)
	}
	if csrfHeader != auth.CSRFHeader {
		t.Errorf("csrfHeader is %q, auth.CSRFHeader is %q", csrfHeader, auth.CSRFHeader)
	}
}

// docRoutes is apiRoutes for a test.
func docRoutes(t *testing.T) []apiRoute {
	t.Helper()
	table, err := apiRoutes()
	if err != nil {
		t.Fatal(err)
	}
	return table
}

// tableModes are the servers that (*Server).table() tells apart: security
// off, security on with writes off, and writes on.
var tableModes = []struct {
	name string
	srv  *Server
}{
	{"security off", &Server{writes: &writePath{}}},
	{"security on", &Server{sec: newFakeSecurity(), writes: &writePath{}}},
	{"writes on", &Server{sec: newFakeSecurity(), writes: &writePath{writer: &blobwrite.Writer{}, clients: true}}},
}

// The route reader sees every route of every table the server routes by:
// each table that table() returns in one of its modes. A route or method
// that apiRoutes does not return fails, so a new slice of routes cannot stay
// out of the document unseen. A refusal of writeRefusals is read with the
// served operation that takes its place while writes are on.
func TestRouteReaderSeesEveryRoute(t *testing.T) {
	read := map[string]bool{}
	for _, r := range docRoutes(t) {
		read[r.method+" "+r.pattern.String()] = true
		if r.offPattern != nil {
			read[r.method+" "+r.offPattern.String()] = true
		}
	}
	tables := map[string]bool{}
	for _, mode := range tableModes {
		table := mode.srv.table()
		tables[fmt.Sprintf("%p", table)] = true
		for _, rt := range table.Entries() {
			for _, method := range rt.Methods {
				if !read[method+" "+rt.Pattern.String()] {
					t.Errorf("%s: the table holds %s %s, which apiRoutes does not read", mode.name, method, rt.Pattern)
				}
			}
		}
	}
	if len(tables) != len(tableModes) {
		t.Errorf("the %d modes give %d tables, want one each", len(tableModes), len(tables))
	}
}

// genericTemplate knows the three group forms of the route patterns and
// refuses any other, so that a new form cannot pass into the document as
// regular expression text.
func TestGenericTemplateKnowsEachGroupForm(t *testing.T) {
	for pattern, want := range map[string]string{
		`^/a/([^/]+)/b$`:    "/a/{}/b",
		`^/a/(.*)\n?$`:      "/a/{*}",
		`^/a/x:(.+)$`:       "/a/x:{+}",
		`^/a/openapi\.json`: "/a/openapi.json",
	} {
		got, err := genericTemplate(regexp.MustCompile(pattern))
		if err != nil || got != want {
			t.Errorf("genericTemplate(%s) = %q, %v, want %q", pattern, got, err, want)
		}
	}
	for _, pattern := range []string{`^/a/(\d+)$`, `^/a/([a-z]+)$`, `^/a/(?:x|y)$`, `^/a/b?$`, `^/a/[^/]+$`} {
		if got, err := genericTemplate(regexp.MustCompile(pattern)); err == nil {
			t.Errorf("genericTemplate(%s) = %q without an error, want an error for a form it does not know", pattern, got)
		}
	}
}
