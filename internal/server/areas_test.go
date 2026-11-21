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
	"time"

	adminrules "github.com/iogentic/pipesmith-registry/internal/admin"
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
	data, err := rest.ReadOnly(adminrules.NewReadOnlyMode(time.Now).State(), true).JSON()
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
	// ruling SB2: a server with writes off starts released, its reason
	// says that its content writes stay refused
	if want.Frozen || admin.ReadOnly.Frozen || admin.ReadOnly.Reason != want.SummaryReason || !strings.Contains(want.SummaryReason, "writes off") {
		t.Fatalf("readOnly %+v, want released with %q", admin.ReadOnly, want.SummaryReason)
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
	// with writes off 3 of the 104 requests of the contract at 1e6d884 are
	// refused, as the list of TestContractRefusals counts them, and the one
	// request of beyondContract; with writes on the 4 of writeRefusals are
	// served; POST .../invalidate-cache (leg 5c of phase 5), the nine writes
	// of the area security, the five writes of the LDAP connections and the
	// three checks of an LDAP form (leg 6-identity), and the four writes of
	// the blob stores and the four of the repositories (leg 6-stores), and
	// the system information, the four requests of the capabilities, the
	// seven of the routing rules, the four of the tasks and the three
	// changes of the read only mode (leg 6-system), the two figures per day
	// of S43 (task 7.23 of phase 7), the six requests of the log files
	// and logger levels (task 7.24), the node id (F2), the three of the
	// tokens, the two of the own account and the five of the task types and
	// settings (leg 6-api-a), and the six requests of the cleanup policies
	// (leg 6-api-b) are served in both
	for _, c := range []struct {
		name  string
		app   *secureApp
		count int
	}{{"writes off", newSecureApp(t, newFakeSecurity()), 4}, {"writes on", writingSecureApp(t), 0}} {
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
// entry more and without POST .../invalidate-cache, which leg 5c of phase 5
// serves: the three requests of writeRefusals that 0.1.1 lists first, in its
// order, then the field list of every format (beyondContract), then the
// table of 0.1.1.
func TestAreasWithWritesOffAsBefore(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	r := a.send("GET", "/api/areas", "basic", asAdmin)
	data, err := rest.Areas(adminrules.NewReadOnlyMode(time.Now).State(), true, refusedOf(slices.Concat(writeRefusals, refusalTable))).JSON()
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
	// 101 requests outside the list with writes off, 104 with writes on
	for _, c := range []struct {
		name   string
		app    *secureApp
		served int
	}{{"writes off", newSecureApp(t, newFakeSecurity()), 101}, {"writes on", writingSecureApp(t), 104}} {
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
		target := filled(path)
		if key == "POST /service/rest/v1/repositories/{segment}/{type}" {
			// x1 names no kind of A5, which the create answers with 501 by design
			target = "/service/rest/v1/repositories/raw/hosted"
		}
		if r := a.send(method, target, "basic", asAdmin); r.status == 501 {
			t.Errorf("%s answers 501 but is not in the list", key)
		}
		served++
	}
	if served != want {
		t.Fatalf("%d requests outside the list, want %d", served, want)
	}
}

func TestAPlaceholderIsOneSegment(t *testing.T) {
	// with writes off the templates of writeRefusals are refusals too: 4
	// templates with a placeholder of one segment give 2 requests; with
	// writes on they give none (the tokens and the task settings left the
	// refusals in leg 6-api-a, one cleanup policy in 6-api-b)
	for _, c := range []struct {
		name  string
		app   *secureApp
		table []refusalDecl
		count int
	}{
		{"writes off", newSecureApp(t, newFakeSecurity()), slices.Concat(writeRefusals, refusalTable), 2},
		{"writes on", writingSecureApp(t), refusalTable, 0},
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
