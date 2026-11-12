package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/rest"
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

func TestAreasNamesEveryRefusalOfTheContract(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	v := areasOf(t, a, asAdmin)
	contract := contractRequests(t)
	seen := map[string]bool{}
	for _, e := range v.Refused {
		key := e.Method + " " + e.Path
		if seen[key] {
			t.Errorf("%s listed twice", key)
		}
		seen[key] = true
		if !contract[key] {
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
	// 73 of the 104 requests of the contract at 1e6d884 are refused, as the
	// list of TestContractRefusals counts them
	if len(v.Refused) != 73 {
		t.Fatalf("%d refused, want 73", len(v.Refused))
	}
}

func TestNoOtherRequestOfTheContractAnswers501(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
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
	if served != 31 {
		t.Fatalf("%d requests outside the list, want 31", served)
	}
}

func TestAPlaceholderIsOneSegment(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	wide := 0
	for _, d := range refusalTable {
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
	// 21 templates with a placeholder of one segment give 34 requests
	if wide != 34 {
		t.Fatalf("%d requests checked", wide)
	}
}
