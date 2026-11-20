package server

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

const (
	rulesPath   = "/service/rest/v1/routing-rules"
	testPath    = "/api/routing-rules/test"
	previewPath = "/api/routing-rules/preview"
	asSettings  = "demo-settings:settings-pw"
)

// routingSecurity is the fake with routing rules, links and a generation
// that every routing write raises, as state.sqlite does.
type routingSecurity struct {
	*fakeSecurity
	rmu    sync.Mutex
	rules  model.Routing
	gen    int64
	writes int
	// failGeneration and failRouting make the reads of the generation and
	// of the rules fail, as a state file that cannot be read does
	failGeneration, failRouting bool
}

// errUnreadable is the error of a read the fake fails; it names a path, so
// a log line that holds it names the path.
var errUnreadable = errors.New("unable to open /srv/demo-data/state.sqlite")

func (r *routingSecurity) fail(generation, routing bool) {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	r.failGeneration, r.failRouting = generation, routing
}

func (r *routingSecurity) Routing() (model.Routing, error) {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	if r.failRouting {
		return model.Routing{}, errUnreadable
	}
	return model.Routing{Rules: slices.Clone(r.rules.Rules), Links: maps.Clone(r.rules.Links)}, nil
}

func (r *routingSecurity) UpdateRouting(fn func(*model.Routing) error) error {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	next := model.Routing{Rules: slices.Clone(r.rules.Rules), Links: maps.Clone(r.rules.Links)}
	if err := fn(&next); err != nil {
		return err
	}
	r.rules, r.gen, r.writes = next, r.gen+1, r.writes+1
	return nil
}

func (r *routingSecurity) Generation() (int64, error) {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	if r.failGeneration {
		return 0, errUnreadable
	}
	return r.gen, nil
}

func (r *routingSecurity) link(repository, rule string) {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	r.rules.Links[repository] = rule
	r.gen++
}

func (r *routingSecurity) writeCount() int {
	r.rmu.Lock()
	defer r.rmu.Unlock()
	return r.writes
}

func newRoutingApp(t *testing.T) (*app, *routingSecurity) {
	t.Helper()
	sec := &routingSecurity{fakeSecurity: newFakeSecurity(), rules: model.Routing{
		Rules: []model.RoutingRule{
			{Name: "demo-block", Description: "Invented", Mode: "BLOCK", Matchers: []string{"^/com/example/blocked/.*"}},
			{Name: "demo-free", Mode: "ALLOW", Matchers: []string{"/.*"}},
		},
		Links: map[string]string{"demo-maven": "demo-block"}}}
	sec.addBasic(asSettings, subject("demo-settings", "", "nexus:settings:read", "nexus:repository-view:maven2:*:browse"))
	return startSecured(t, sec), sec
}

// routeAllowed is the verdict of the live routing of the server; an error
// fails the test.
func routeAllowed(t *testing.T, a *app, repository, path string) bool {
	t.Helper()
	allowed, err := a.srv.routes.Allowed(repository, path)
	if err != nil {
		t.Fatal(err)
	}
	return allowed
}

func ruleSend(t *testing.T, a *app, method, target, body, who string) *answer {
	t.Helper()
	headers := []string{"Content-Type", jsonType}
	if who != "" {
		headers = append(headers, "basic", who)
	}
	return do(t, a.addr(), method, target, []byte(body), headers...)
}

// The routing rules of contract S16 and S17: the list and one rule in the
// shape of RoutingRuleXO, create, save and delete with 204, the refusals of
// SY2 and of a rule in use in the shapes of section Errors; no refusal
// writes.
func TestRoutingRules(t *testing.T) {
	a, sec := newRoutingApp(t)
	r := ruleSend(t, a, "GET", rulesPath, "", asAdmin)
	status(t, r, 200)
	var list []map[string]any
	if err := json.Unmarshal(r.body, &list); err != nil || len(list) != 2 {
		t.Fatalf("%v %s", err, r.body)
	}
	for _, item := range list {
		if len(item) != 4 || item["name"] == nil || item["description"] == nil || item["mode"] == nil || item["matchers"] == nil {
			t.Fatalf("not a RoutingRuleXO: %v", item)
		}
	}
	if one := ruleSend(t, a, "GET", rulesPath+"/demo-block", "", asAdmin); one.status != 200 || one.object(t)["mode"] != "BLOCK" {
		t.Fatalf("%d %s", one.status, one.body)
	}
	status(t, ruleSend(t, a, "GET", rulesPath+"/demo-none", "", asAdmin), 404)

	for _, c := range []struct {
		method, target, body, who string
		status                    int
		field                     string
	}{
		{"POST", rulesPath, `{"name":"demo new","mode":"BLOCK","matchers":["/a"]}`, asAdmin, 400, "name"},
		{"POST", rulesPath, `{"name":"demo-new","mode":"BLOCK","matchers":["/a","("]}`, asAdmin, 400, "matchers[1]"},
		{"POST", rulesPath, `{"name":"demo-new","mode":"BLOCK","matchers":["/a"],"description":"` + strings.Repeat("d", 70000) + `"}`, asAdmin, 400, "*"},
		{"POST", rulesPath, `{"name":"demo-block","mode":"BLOCK","matchers":["/a"]}`, asAdmin, 409, ""},
		{"PUT", rulesPath + "/demo-block", `{"name":"demo-other","mode":"BLOCK","matchers":["/a"]}`, asAdmin, 400, "name"},
		{"PUT", rulesPath + "/demo-none", `{"mode":"BLOCK","matchers":["/a"]}`, asAdmin, 404, ""},
		{"DELETE", rulesPath + "/demo-block", ``, asAdmin, 409, ""},
		{"DELETE", rulesPath + "/demo-none", ``, asAdmin, 404, ""},
		{"POST", rulesPath, `{"name":"demo-new","mode":"BLOCK","matchers":["/a"]}`, asSettings, 403, ""},
		{"PUT", rulesPath + "/demo-free", `{"mode":"BLOCK","matchers":["/a"]}`, asReader, 403, ""},
		{"DELETE", rulesPath + "/demo-free", ``, asNobody, 403, ""},
		{"DELETE", rulesPath + "/demo-free", ``, "", 401, ""},
	} {
		label := c.method + " " + c.target
		got := ruleSend(t, a, c.method, c.target, c.body, c.who)
		if got.status != c.status {
			t.Fatalf("%s: %d %.200s", label, got.status, got.body)
		}
		if c.status == 400 {
			var fields []map[string]string
			if err := json.Unmarshal(got.body, &fields); err != nil || len(fields) != 1 || fields[0]["id"] != c.field {
				t.Fatalf("%s: %.300s, want the field %s", label, got.body, c.field)
			}
		}
		if c.status == 409 && got.object(t)["message"] == "" {
			t.Fatalf("%s: %s", label, got.body)
		}
		if n := sec.writeCount(); n != 0 {
			t.Fatalf("%s: a refusal wrote", label)
		}
	}
	for _, who := range []string{asReader, asNobody} {
		status(t, ruleSend(t, a, "GET", rulesPath, "", who), 403)
	}
	status(t, ruleSend(t, a, "GET", rulesPath, "", asSettings), 200)

	for i, c := range []struct{ method, target, body string }{
		{"POST", rulesPath, `{"name":"demo-new","description":"New","mode":"ALLOW","matchers":["/com/example/.*"]}`},
		{"PUT", rulesPath + "/demo-new", `{"name":"demo-new","description":"Saved","mode":"BLOCK","matchers":["/x/.*"]}`},
		{"DELETE", rulesPath + "/demo-new", ``},
		{"DELETE", rulesPath + "/demo-free", ``},
	} {
		got := ruleSend(t, a, c.method, c.target, c.body, asAdmin)
		if got.status != 204 || len(got.body) != 0 {
			t.Fatalf("%s %s: %d %q", c.method, c.target, got.status, got.body)
		}
		if sec.writeCount() != i+1 {
			t.Fatalf("%s %s: %d writes", c.method, c.target, sec.writeCount())
		}
		if i == 1 {
			saved := ruleSend(t, a, "GET", rulesPath+"/demo-new", "", asAdmin).object(t)
			if saved["description"] != "Saved" || saved["mode"] != "BLOCK" {
				t.Fatalf("saved %v", saved)
			}
		}
	}
	if rs, _ := sec.Routing(); len(rs.Rules) != 1 || rs.Rules[0].Name != "demo-block" {
		t.Fatalf("%+v", rs.Rules)
	}
}

// A saved rule takes effect at the next request, with no restart (SY3).
func TestASavedRuleIsLive(t *testing.T) {
	a, sec := newRoutingApp(t)
	sec.link("demo-px", "demo-live")
	if routeAllowed(t, a, "demo-px", "/a/b") {
		t.Fatal("a link to a rule that does not exist allowed a path")
	}
	if r := ruleSend(t, a, "POST", rulesPath, `{"name":"demo-live","mode":"ALLOW","matchers":["/a/.*"]}`, asAdmin); r.status != 204 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if !routeAllowed(t, a, "demo-px", "/a/b") || routeAllowed(t, a, "demo-px", "/c") {
		t.Fatal("the created rule is not live")
	}
	if r := ruleSend(t, a, "PUT", rulesPath+"/demo-live", `{"mode":"BLOCK","matchers":["/a/.*"]}`, asAdmin); r.status != 204 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if routeAllowed(t, a, "demo-px", "/a/b") || !routeAllowed(t, a, "demo-px", "/c") {
		t.Fatal("the saved rule is not live")
	}
	settings := ruleSend(t, a, "GET", "/service/rest/v1/repositorySettings", "", asAdmin)
	status(t, settings, 200)
}

// The test of a rule answers with the engine of the server (contract S17).
func TestRoutingRuleTest(t *testing.T) {
	a, sec := newRoutingApp(t)
	for _, c := range []struct {
		body, who string
		status    int
		want      string
	}{
		{`{"mode":"BLOCK","matchers":["/a/.*","/b/.*"],"path":"b/x"}`, asAdmin, 200, `{"allowed":false,"matcher":1}`},
		{`{"mode":"BLOCK","matchers":["/a/.*"],"path":"/c"}`, asSettings, 200, `{"allowed":true,"matcher":null}`},
		{`{"mode":"BLOCK","matchers":["/a/.*","("],"path":"/c"}`, asAdmin, 400, `matchers[1]`},
		{`{"mode":"BLOCK","matchers":["/a/.*"],"path":"/` + strings.Repeat("p", 4096) + `"}`, asAdmin, 400, `"id":"path"`},
		{`{"mode":"BLOCK","matchers":["/a/.*"],"path":"/c"}`, asReader, 403, ``},
		{`{"mode":"BLOCK","matchers":["/a/.*"],"path":"/c"}`, "", 401, ``},
	} {
		r := ruleSend(t, a, "POST", testPath, c.body, c.who)
		if r.status != c.status || !strings.Contains(string(r.body), c.want) {
			t.Fatalf("%.80s: %d %s, want %d %s", c.body, r.status, r.body, c.status, c.want)
		}
	}
	if sec.writeCount() != 0 {
		t.Fatal("a test wrote")
	}
}

// The preview lists the verdict of every repository in scope that the
// subject may browse, and reads no content (contract S18, SY2).
func TestRoutingPreview(t *testing.T) {
	a, sec := newRoutingApp(t)
	items := func(scope, who string) []map[string]any {
		t.Helper()
		r := ruleSend(t, a, "POST", previewPath, `{"scope":"`+scope+`","path":"/com/example/blocked/x.jar"}`, who)
		status(t, r, 200)
		var v struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(r.body, &v); err != nil {
			t.Fatal(err)
		}
		return v.Items
	}
	names := func(list []map[string]any) []string {
		var out []string
		for _, item := range list {
			if len(item) != 5 {
				t.Fatalf("not an item of the contract: %v", item)
			}
			out = append(out, item["repository"].(string))
		}
		return out
	}
	all := items("all", asAdmin)
	if got := names(all); !slices.Equal(got, []string{"demo-docker", "demo-group", "demo-maven", "demo-npm", "demo-raw"}) {
		t.Fatalf("%v", got)
	}
	for _, item := range all {
		if item["rule"] != nil || item["allowed"] != true {
			t.Fatalf("a hosted repository or a group with a rule or a verdict: %v", item)
		}
	}
	if got := names(items("group", asAdmin)); !slices.Equal(got, []string{"demo-group"}) {
		t.Fatalf("group: %v", got)
	}
	if got := names(items("proxy", asAdmin)); len(got) != 0 {
		t.Fatalf("proxy: %v", got)
	}
	if got := names(items("all", asSettings)); !slices.Equal(got, []string{"demo-group", "demo-maven"}) {
		t.Fatalf("a subject that browses maven2 only sees %v", got)
	}
	r := ruleSend(t, a, "POST", previewPath, `{"scope":"hosted","path":"/a"}`, asAdmin)
	if r.status != 400 || !strings.Contains(string(r.body), `"id":"scope"`) {
		t.Fatalf("%d %s", r.status, r.body)
	}
	status(t, ruleSend(t, a, "POST", previewPath, `{"scope":"all","path":"/a"}`, asReader), 403)
	if sec.writeCount() != 0 {
		t.Fatal("a preview wrote")
	}
}
