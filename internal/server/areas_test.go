package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/rest"
)

type areasAnswer struct {
	ReadOnly struct {
		Frozen bool   `json:"frozen"`
		Reason string `json:"reason"`
	} `json:"readOnly"`
	Refused []struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Area   string `json:"area"`
	} `json:"refused"`
}

// contractRequests reads testdata/ui-contract-requests.txt: "METHOD path" of
// every request of docs/ui/API.md, the query removed.
func contractRequests(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile("testdata/ui-contract-requests.txt")
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	lines := bufio.NewScanner(bytes.NewReader(data))
	for lines.Scan() {
		if line := lines.Text(); line != "" && !strings.HasPrefix(line, "#") {
			set[line] = true
		}
	}
	return set
}

var placeholder = regexp.MustCompile(`\{[^/{}]+\}`)

// filled puts a value into every {name} of a path of the contract.
func filled(path string) string { return placeholder.ReplaceAllString(path, "x1") }

func areasOf(t *testing.T, a *secureApp, who string) areasAnswer {
	t.Helper()
	r := a.send("GET", "/api/areas", "basic", who)
	status(t, r, 200)
	var v areasAnswer
	dec := json.NewDecoder(bytes.NewReader(r.body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%v: %s", err, r.body)
	}
	return v
}

func TestAreasNeedsASubject(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	status(t, a.send("GET", "/api/areas"), 401)
}

func TestAreasForEverySignedInUser(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	admin, nobody := areasOf(t, a, asAdmin), areasOf(t, a, asNobody)
	data, err := rest.ReadOnly().JSON()
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		Frozen        bool   `json:"frozen"`
		SummaryReason string `json:"summaryReason"`
	}
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if !want.Frozen || !admin.ReadOnly.Frozen || admin.ReadOnly.Reason != want.SummaryReason || admin.ReadOnly.Reason == "" {
		t.Fatalf("readOnly %+v, want frozen with %q", admin.ReadOnly, want.SummaryReason)
	}
	a1, _ := json.Marshal(admin)
	n1, _ := json.Marshal(nobody)
	if !bytes.Equal(a1, n1) {
		t.Fatalf("the answer differs by user:\n%s\n%s", a1, n1)
	}
}

// writingSecureApp is newSecureApp with writes on.
func writingSecureApp(t *testing.T) *secureApp {
	t.Helper()
	sec := newFakeSecurity()
	return &secureApp{app: startWriting(t, sec, true), sec: sec}
}

func TestAreasNamesEveryRefusalOfTheContract(t *testing.T) {
	// with writes off 73 of the 104 requests of the contract at 1e6d884 are
	// refused, as the list of TestContractRefusals counts them, and the one
	// request of beyondContract; with writes on the 4 of writeRefusals are
	// served
	for _, c := range []struct {
		name  string
		app   *secureApp
		count int
	}{{"writes off", newSecureApp(t, newFakeSecurity()), 74}, {"writes on", writingSecureApp(t), 70}} {
		t.Run(c.name, func(t *testing.T) { refusalsOfTheContract(t, c.app, c.count) })
	}
}

// beyondContract are the refusals of requests the contract of the interface
// does not name: the field list of every format, a REST write of the old
// product that answers as the others with writes off
// (TestTheListOfUploadSpecsIsARefusalWithWritesOff).
var beyondContract = map[string]bool{"GET /service/rest/v1/formats/upload-specs": true}

func refusalsOfTheContract(t *testing.T, a *secureApp, count int) {
	v := areasOf(t, a, asAdmin)
	contract := contractRequests(t)
	seen := map[string]bool{}
	for _, e := range v.Refused {
		key := e.Method + " " + e.Path
		if seen[key] {
			t.Errorf("%s listed twice", key)
		}
		seen[key] = true
		if !contract[key] && !beyondContract[key] {
			t.Errorf("%s is not a request of the contract", key)
		}
		if strings.ContainsAny(e.Path, "()|?*+\\") {
			t.Errorf("%s is not in the spelling of the contract", key)
		}
		r := a.send(e.Method, filled(e.Path), "basic", asAdmin)
		if r.status != 501 {
			t.Errorf("%s answers %d, not 501", key, r.status)
			continue
		}
		if area := r.object(t)["area"]; area != e.Area {
			t.Errorf("%s: area %v in the 501, %q in the list", key, area, e.Area)
		}
	}
	if len(v.Refused) != count {
		t.Fatalf("%d refused, want %d", len(v.Refused), count)
	}
}

// With writes off the list of GET /api/areas is the one of 0.1.1 with one
// entry more: the three requests of writeRefusals that 0.1.1 lists first, in
// its order, then the field list of every format (beyondContract), then the
// table of 0.1.1.
func TestAreasWithWritesOffAsBefore(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	r := a.send("GET", "/api/areas", "basic", asAdmin)
	data, err := rest.Areas(refusedOf(slices.Concat(writeRefusals, refusalTable))).JSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(r.body) != string(data) || !strings.HasPrefix(string(r.body), `{"readOnly":`) {
		t.Fatalf("%s", r.body)
	}
	var v areasAnswer
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"DELETE /service/rest/v1/assets/{id}", "GET /service/rest/v1/formats/{format}/upload-specs",
		"POST /service/rest/v1/components", "GET /service/rest/v1/formats/upload-specs"} {
		if got := v.Refused[i].Method + " " + v.Refused[i].Path; got != want {
			t.Fatalf("refused %d is %s, want %s", i, got, want)
		}
	}
}

func TestNoOtherRequestOfTheContractAnswers501(t *testing.T) {
	// 31 requests outside the list with writes off, 34 with writes on
	for _, c := range []struct {
		name   string
		app    *secureApp
		served int
	}{{"writes off", newSecureApp(t, newFakeSecurity()), 31}, {"writes on", writingSecureApp(t), 34}} {
		t.Run(c.name, func(t *testing.T) { servedOfTheContract(t, c.app, c.served) })
	}
}

func servedOfTheContract(t *testing.T, a *secureApp, want int) {
	v := areasOf(t, a, asAdmin)
	refused := map[string]bool{}
	for _, e := range v.Refused {
		refused[e.Method+" "+e.Path] = true
	}
	served := 0
	for key := range contractRequests(t) {
		if refused[key] {
			continue
		}
		method, path, _ := strings.Cut(key, " ")
		if r := a.send(method, filled(path), "basic", asAdmin); r.status == 501 {
			t.Errorf("%s answers 501 but is not in the list", key)
		}
		served++
	}
	if served != want {
		t.Fatalf("%d requests outside the list, want %d", served, want)
	}
}

func TestAPlaceholderIsOneSegment(t *testing.T) {
	// with writes off the templates of writeRefusals are refusals too: 21
	// templates with a placeholder of one segment give 34 requests; with
	// writes on 19 give 32
	for _, c := range []struct {
		name  string
		app   *secureApp
		table []refusalDecl
		count int
	}{
		{"writes off", newSecureApp(t, newFakeSecurity()), slices.Concat(writeRefusals, refusalTable), 34},
		{"writes on", writingSecureApp(t), refusalTable, 32},
	} {
		t.Run(c.name, func(t *testing.T) { placeholdersAreOneSegment(t, c.app, c.table, c.count) })
	}
}

func placeholdersAreOneSegment(t *testing.T, a *secureApp, table []refusalDecl, count int) {
	wide := 0
	for _, d := range table {
		if strings.Contains(d.template, "+}") || !strings.Contains(d.template, "{") {
			continue
		}
		for _, method := range d.methods {
			for _, path := range d.paths() {
				target := placeholder.ReplaceAllString(path, "a/b")
				if r := a.send(method, target, "basic", asAdmin); r.status == 501 {
					t.Errorf("%s %s answers 501: {name} of %s is one segment", method, target, d.template)
				}
				wide++
			}
		}
	}
	if wide != count {
		t.Fatalf("%d requests checked, want %d", wide, count)
	}
}
