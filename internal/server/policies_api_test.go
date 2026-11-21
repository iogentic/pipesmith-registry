package server

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The cleanup policies of leg 6-api-b (contract S11, S14, S15, rulings CP1
// to CP16): the list, one policy, create, save, delete and the preview on a
// fake security service whose identity store holds the policies.

const policiesPath = "/api/cleanup-policies"

// policyApp is a server with security on whose snapshot holds two
// policies: demo-age of origin local, named by demo-raw, and the imported
// demo-mixed with a format outside the five and a criterion this server
// does not know (A9).
func policyApp(t *testing.T) (*secureApp, *fakeSecurity) {
	t.Helper()
	sec := newFakeSecurity()
	sec.snap.CleanupPolicies = []model.CleanupPolicy{
		{Name: "demo-mixed", Format: "ALL_FORMATS", Notes: "imported",
			Criteria: map[string]any{"lastDownloadedDays": float64(60), "regex": ".*-SNAPSHOT.*"}, Origin: model.OriginImport},
		{Name: "demo-age", Format: "raw", Criteria: map[string]any{"lastBlobUpdatedDays": float64(30)}, Origin: model.OriginLocal},
	}
	for i, r := range sec.snap.Repositories {
		if r.Name == "demo-raw" {
			attributes := map[string]any{"cleanup": map[string]any{"policyNames": []any{"demo-age"}}}
			for key, value := range r.Attributes {
				attributes[key] = value
			}
			sec.snap.Repositories[i].Attributes = attributes
		}
	}
	return newSecureApp(t, sec), sec
}

var (
	demoAgeView = map[string]any{"name": "demo-age", "format": "raw", "notes": "",
		"criteria": map[string]any{"lastBlobUpdatedDays": 30, "lastDownloadedDays": nil}, "usedBy": []string{"demo-raw"}}
	demoMixedView = map[string]any{"name": "demo-mixed", "format": "ALL_FORMATS", "notes": "imported",
		"criteria": map[string]any{"lastBlobUpdatedDays": nil, "lastDownloadedDays": 60, "regex": ".*-SNAPSHOT.*"}, "usedBy": []string{}}
)

// GET /api/cleanup-policies lists every policy sorted by name, each with
// both known criteria (null for an absent one), a stored unknown criterion
// as stored, and the repositories that name it (CP2, CP3, CP4, CP13).
func TestCleanupPoliciesAreListedByName(t *testing.T) {
	a, _ := policyApp(t)
	r := a.send("GET", policiesPath, "basic", asAdmin)
	status(t, r, 200)
	sameJSON(t, r.body, []any{demoAgeView, demoMixedView})
}

// GET /api/cleanup-policies/{name} answers the policy in the shape of the
// list, and 404 with a message for a name no policy has.
func TestACleanupPolicyIsReadByName(t *testing.T) {
	a, _ := policyApp(t)
	for name, want := range map[string]any{"demo-age": demoAgeView, "demo-mixed": demoMixedView} {
		r := a.send("GET", policiesPath+"/"+name, "basic", asAdmin)
		status(t, r, 200)
		sameJSON(t, r.body, want)
	}
	r := a.send("GET", policiesPath+"/demo-missing", "basic", asAdmin)
	status(t, r, 404)
	if m := r.object(t); m["message"] != "no cleanup policy named demo-missing" || len(m) != 1 {
		t.Fatalf("%s", r.body)
	}
}

// The reads need nexus:settings:read: a reader of content and a subject
// without rights get 403, a request without credentials 401.
func TestTheReadsOfCleanupPoliciesNeedTheSettingsRead(t *testing.T) {
	a, sec := policyApp(t)
	sec.addBasic("demo-settings:settings-pw", subject("demo-settings", "", settingsRead))
	for _, target := range []string{policiesPath, policiesPath + "/demo-age"} {
		for who, want := range map[string]int{asReader: 403, asNobody: 403, "demo-settings:settings-pw": 200} {
			status(t, a.send("GET", target, "basic", who), want)
		}
		status(t, a.send("GET", target), 401)
	}
}

func storedPolicy(sec *fakeSecurity, name string) (model.CleanupPolicy, bool) {
	list := sec.Snapshot().CleanupPolicies
	at := slices.IndexFunc(list, func(p model.CleanupPolicy) bool { return p.Name == name })
	if at < 0 {
		return model.CleanupPolicy{}, false
	}
	return list[at], true
}

// POST /api/cleanup-policies answers 201 with the view of the new policy.
// The known criteria that are set are stored and the null ones dropped; an
// absent notes is stored empty; the new policy is of origin local; usedBy
// in the body is ignored (CP2, CP5, CP12, CP13).
func TestACleanupPolicyIsCreated(t *testing.T) {
	a, sec := policyApp(t)
	r := sendTask(t, a, "POST", policiesPath, `{"name": "demo-new", "format": "npm", "notes": "Invented notes",
		"criteria": {"lastBlobUpdatedDays": 10, "lastDownloadedDays": null}, "usedBy": ["demo-npm"], "origin": "import"}`)
	status(t, r, 201)
	sameJSON(t, r.body, map[string]any{"name": "demo-new", "format": "npm", "notes": "Invented notes",
		"criteria": map[string]any{"lastBlobUpdatedDays": 10, "lastDownloadedDays": nil}, "usedBy": []string{}})
	p, found := storedPolicy(sec, "demo-new")
	if !found || p.Format != "npm" || p.Notes != "Invented notes" || p.Origin != model.OriginLocal ||
		!reflect.DeepEqual(p.Criteria, map[string]any{"lastBlobUpdatedDays": float64(10)}) {
		t.Fatalf("stored %+v %v", p, found)
	}
	status(t, sendTask(t, a, "POST", policiesPath, `{"name": "demo-plain", "format": "*", "criteria": {"lastDownloadedDays": 7}}`), 201)
	if p, _ := storedPolicy(sec, "demo-plain"); p.Notes != "" || p.Format != "*" {
		t.Fatalf("stored %+v", p)
	}
	r = a.send("GET", policiesPath, "basic", asAdmin)
	var names []string
	for _, item := range r.json(t).([]any) {
		names = append(names, item.(map[string]any)["name"].(string))
	}
	if !slices.Equal(names, []string{"demo-age", "demo-mixed", "demo-new", "demo-plain"}) {
		t.Fatalf("list %v", names)
	}
}

// A create of a name that a repository names already applies at once:
// usedBy of the 201 names the repository (CP10).
func TestACreatedPolicyAppliesToTheRepositoryThatNamesIt(t *testing.T) {
	sec := newFakeSecurity()
	for i, r := range sec.snap.Repositories {
		if r.Name == "demo-npm" {
			sec.snap.Repositories[i].Attributes = map[string]any{"cleanup": map[string]any{"policyNames": []any{"demo-later"}}}
		}
	}
	a := newSecureApp(t, sec)
	r := sendTask(t, a, "POST", policiesPath, `{"name": "demo-later", "format": "npm", "criteria": {"lastDownloadedDays": 30}}`)
	status(t, r, 201)
	if used := r.object(t)["usedBy"]; !reflect.DeepEqual(used, []any{"demo-npm"}) {
		t.Fatalf("usedBy %v", used)
	}
}

// The refusals of a create travel together in one 400 list; an existing
// name answers 409; a refused create writes nothing (R1, R2, R11, CP12).
func TestTheRefusalsOfACreate(t *testing.T) {
	a, sec := policyApp(t)
	long := strings.Repeat("n", 401)
	for _, c := range []struct {
		body string
		ids  []string
	}{
		{`{"name": "bad name", "format": "rpm", "notes": "` + long + `", "criteria": {"regex": "x"}}`, []string{"name", "format", "notes", "criteria.regex"}},
		{`{"name": "demo-x", "criteria": {"lastBlobUpdatedDays": 1}}`, []string{"format"}},
		{`{"name": "demo-x", "format": "raw"}`, []string{"criteria"}},
		{`{"name": "demo-x", "format": "raw", "criteria": null}`, []string{"criteria"}},
		{`{"name": "demo-x", "format": "raw", "criteria": "weekly"}`, []string{"criteria"}},
		{`{"name": "demo-x", "format": "raw", "criteria": {"lastBlobUpdatedDays": 0, "lastDownloadedDays": 1.5}}`,
			[]string{"criteria.lastBlobUpdatedDays", "criteria.lastDownloadedDays"}},
		{`{"name": "demo-x", "format": "raw", "criteria": {"lastBlobUpdatedDays": null}}`, []string{"criteria"}},
		{`{"name": "demo-x", "format": "raw", "notes": 7, "criteria": {"lastBlobUpdatedDays": 1}}`, []string{"notes"}},
		{`{"format": "raw", "criteria": {"lastBlobUpdatedDays": 1}}`, []string{"name"}},
		{`[]`, []string{"*"}},
	} {
		if ids := refusedFields(t, sendTask(t, a, "POST", policiesPath, c.body)); !slices.Equal(ids, c.ids) {
			t.Errorf("%s: ids %v, want %v", c.body, ids, c.ids)
		}
	}
	status(t, sendTask(t, a, "POST", policiesPath, `{"name": "demo-x", "format": "raw", "notes": "`+strings.Repeat("ü", 400)+`", "criteria": {"lastBlobUpdatedDays": 1}}`), 201)
	r := sendTask(t, a, "POST", policiesPath, `{"name": "demo-age", "format": "raw", "criteria": {"lastBlobUpdatedDays": 1}}`)
	status(t, r, 409)
	if m := r.object(t); m["message"] != "a cleanup policy named demo-age exists" || len(m) != 1 {
		t.Fatalf("%s", r.body)
	}
	if n := len(sec.Snapshot().CleanupPolicies); n != 3 {
		t.Fatalf("%d policies stored", n)
	}
}

// PUT /api/cleanup-policies/{name} answers 204 with no-store. The stored
// criteria are the known ones sent that are set; an absent format keeps the
// stored one; the saved policy is of origin local (CP2, CP5, CP12, CP13).
func TestACleanupPolicyIsSaved(t *testing.T) {
	a, sec := policyApp(t)
	r := sendTask(t, a, "PUT", policiesPath+"/demo-age", `{"name": "demo-age", "notes": "Saved",
		"criteria": {"lastBlobUpdatedDays": null, "lastDownloadedDays": 5}, "usedBy": []}`)
	status(t, r, 204)
	if r.header.Get("Cache-Control") != "no-store" || len(r.body) != 0 {
		t.Fatalf("%v %q", r.header, r.body)
	}
	p, _ := storedPolicy(sec, "demo-age")
	if p.Format != "raw" || p.Notes != "Saved" || p.Origin != model.OriginLocal ||
		!reflect.DeepEqual(p.Criteria, map[string]any{"lastDownloadedDays": float64(5)}) {
		t.Fatalf("stored %+v", p)
	}
	status(t, sendTask(t, a, "PUT", policiesPath+"/demo-age", `{"format": "*", "criteria": {"lastDownloadedDays": 5}}`), 204)
	if p, _ := storedPolicy(sec, "demo-age"); p.Format != "*" || p.Notes != "" {
		t.Fatalf("stored %+v", p)
	}
}

// A save of the imported policy that sends its stored values back keeps the
// format outside the five and the criterion this server does not know, and
// makes the policy of origin local; another value of that criterion is
// refused (R6, R11b, CP4, CP5).
func TestASaveKeepsTheStoredValuesOfAnImport(t *testing.T) {
	a, sec := policyApp(t)
	status(t, sendTask(t, a, "PUT", policiesPath+"/demo-mixed", `{"name": "demo-mixed", "format": "ALL_FORMATS", "notes": "imported",
		"criteria": {"lastBlobUpdatedDays": null, "lastDownloadedDays": 60, "regex": ".*-SNAPSHOT.*"}}`), 204)
	p, _ := storedPolicy(sec, "demo-mixed")
	if p.Format != "ALL_FORMATS" || p.Origin != model.OriginLocal ||
		!reflect.DeepEqual(p.Criteria, map[string]any{"lastDownloadedDays": float64(60), "regex": ".*-SNAPSHOT.*"}) {
		t.Fatalf("stored %+v", p)
	}
	ids := refusedFields(t, sendTask(t, a, "PUT", policiesPath+"/demo-mixed", `{"format": "ALL", "criteria": {"lastDownloadedDays": 60, "regex": ".*"}}`))
	if !slices.Equal(ids, []string{"format", "criteria.regex"}) {
		t.Fatalf("ids %v", ids)
	}
}

// A stored notes longer than 400 characters is never refused when it is
// sent back (R6); a new one of that length is.
func TestASaveKeepsALongStoredNotes(t *testing.T) {
	sec := newFakeSecurity()
	long := strings.Repeat("o", 500)
	sec.snap.CleanupPolicies = []model.CleanupPolicy{{Name: "demo-long", Format: "raw", Notes: long,
		Criteria: map[string]any{"lastBlobUpdatedDays": float64(3)}, Origin: model.OriginImport}}
	a := newSecureApp(t, sec)
	body := func(notes string) string {
		return `{"format": "raw", "notes": "` + notes + `", "criteria": {"lastBlobUpdatedDays": 3}}`
	}
	status(t, sendTask(t, a, "PUT", policiesPath+"/demo-long", body(long)), 204)
	if ids := refusedFields(t, sendTask(t, a, "PUT", policiesPath+"/demo-long", body(long+"o"))); !slices.Equal(ids, []string{"notes"}) {
		t.Fatalf("ids %v", ids)
	}
}

// A save to another format is refused at format while a repository of a
// different format names the policy; * clashes with nothing (R4). Another
// name in the body is refused at name, an unknown name answers 404 (R2,
// CP13).
func TestTheRefusalsOfASave(t *testing.T) {
	a, sec := policyApp(t)
	r := sendTask(t, a, "PUT", policiesPath+"/demo-age", `{"format": "maven2", "criteria": {"lastBlobUpdatedDays": 30}}`)
	if ids := refusedFields(t, r); !slices.Equal(ids, []string{"format"}) {
		t.Fatalf("ids %v", ids)
	}
	if !strings.Contains(string(r.body), "demo-raw (raw)") {
		t.Fatalf("%s", r.body)
	}
	if ids := refusedFields(t, sendTask(t, a, "PUT", policiesPath+"/demo-age", `{"name": "demo-other", "criteria": {"lastBlobUpdatedDays": 30}}`)); !slices.Equal(ids, []string{"name"}) {
		t.Fatalf("ids %v", ids)
	}
	if ids := refusedFields(t, sendTask(t, a, "PUT", policiesPath+"/demo-age", `{"criteria": {}}`)); !slices.Equal(ids, []string{"criteria"}) {
		t.Fatalf("ids %v", ids)
	}
	r = sendTask(t, a, "PUT", policiesPath+"/demo-missing", `{"format": "raw", "criteria": {"lastBlobUpdatedDays": 30}}`)
	status(t, r, 404)
	if m := r.object(t); m["message"] != "no cleanup policy named demo-missing" {
		t.Fatalf("%s", r.body)
	}
	if p, _ := storedPolicy(sec, "demo-age"); p.Format != "raw" || p.Origin != model.OriginLocal {
		t.Fatalf("a refused save wrote %+v", p)
	}
	status(t, sendTask(t, a, "PUT", policiesPath+"/demo-age", `{"format": "*", "criteria": {"lastBlobUpdatedDays": 30}}`), 204)
}

// DELETE /api/cleanup-policies/{name} answers 204 with no-store, 409 with
// the names while a repository names the policy, 404 for an unknown name
// (R4, CP13). An imported policy is deleted as a local one (CP5).
func TestACleanupPolicyIsDeleted(t *testing.T) {
	a, sec := policyApp(t)
	r := sendTask(t, a, "DELETE", policiesPath+"/demo-age", "")
	status(t, r, 409)
	if m := r.object(t); m["message"] != "the cleanup policy demo-age is in use by 1 repository: demo-raw" {
		t.Fatalf("%s", r.body)
	}
	if _, found := storedPolicy(sec, "demo-age"); !found {
		t.Fatal("a policy in use was deleted")
	}
	r = sendTask(t, a, "DELETE", policiesPath+"/demo-mixed", "")
	status(t, r, 204)
	if r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%v", r.header)
	}
	if _, found := storedPolicy(sec, "demo-mixed"); found {
		t.Fatal("the imported policy stays")
	}
	status(t, sendTask(t, a, "DELETE", policiesPath+"/demo-mixed", ""), 404)
}

// The writes need nexus:settings:update; a reader of the settings, a reader
// of content and a subject without rights get 403. A body above 65536 bytes
// answers 413.
func TestTheWritesOfCleanupPoliciesNeedTheSettingsUpdate(t *testing.T) {
	a, sec := policyApp(t)
	sec.addBasic("demo-settings:settings-pw", subject("demo-settings", "", settingsRead))
	body := `{"name": "demo-new", "format": "raw", "criteria": {"lastBlobUpdatedDays": 1}}`
	for _, c := range [][2]string{{"POST", policiesPath}, {"PUT", policiesPath + "/demo-mixed"}, {"DELETE", policiesPath + "/demo-mixed"}} {
		for _, who := range []string{asReader, asNobody, "demo-settings:settings-pw"} {
			status(t, sendTask(t, a, c[0], c[1], body, "basic", who), 403)
		}
	}
	if n := len(sec.Snapshot().CleanupPolicies); n != 2 {
		t.Fatalf("%d policies", n)
	}
	r := sendTask(t, a, "POST", policiesPath, `{"name": "demo-big", "notes": "`+strings.Repeat("x", 65536)+`"}`)
	status(t, r, 413)
	if m := r.object(t); m["message"] != "the body is larger than 65536 bytes" {
		t.Fatalf("%s", r.body)
	}
}

// While the server is frozen every write of a policy answers 503 with the
// reason and writes nothing; the reads are served (AP6).
func TestTheWritesOfCleanupPoliciesWaitForTheRelease(t *testing.T) {
	a, sec := policyApp(t)
	status(t, sendTask(t, a, "POST", "/service/rest/v1/read-only/freeze", ""), 204)
	for _, c := range [][3]string{
		{"POST", policiesPath, `{"name": "demo-new", "format": "raw", "criteria": {"lastBlobUpdatedDays": 1}}`},
		{"PUT", policiesPath + "/demo-mixed", `{"criteria": {"lastDownloadedDays": 60, "regex": ".*-SNAPSHOT.*"}}`},
		{"DELETE", policiesPath + "/demo-mixed", ""},
	} {
		status(t, sendTask(t, a, c[0], c[1], c[2]), 503)
	}
	status(t, a.send("GET", policiesPath, "basic", asAdmin), 200)
	if updates, _ := sec.writes(); updates != 0 {
		t.Fatalf("%d writes while frozen", updates)
	}
}

// A save keeps a stored criterion this server does not know when the body
// does not send it, as a save from the interface sends only the two known
// keys: the policy keeps the criterion, stays not runnable, and its
// preview is still refused at that criterion (ruling C1, A9). A known
// criterion the body omits is dropped.
func TestASaveKeepsAStoredUnknownCriterionTheBodyOmits(t *testing.T) {
	a, sec := policyApp(t)
	status(t, sendTask(t, a, "PUT", policiesPath+"/demo-mixed", `{"name": "demo-mixed", "notes": "imported",
		"criteria": {"lastBlobUpdatedDays": null, "lastDownloadedDays": 60}}`), 204)
	p, _ := storedPolicy(sec, "demo-mixed")
	if !reflect.DeepEqual(p.Criteria, map[string]any{"lastDownloadedDays": float64(60), "regex": ".*-SNAPSHOT.*"}) {
		t.Fatalf("stored %+v", p)
	}
	r := a.send("GET", policiesPath+"/demo-mixed", "basic", asAdmin)
	status(t, r, 200)
	sameJSON(t, r.body, demoMixedView)
	shown, err := json.Marshal(r.object(t)["criteria"])
	if err != nil {
		t.Fatal(err)
	}
	ids := refusedFields(t, sendTask(t, a, "POST", policiesPath+"/preview", previewBody("raw", string(shown), "demo-raw")))
	if !slices.Equal(ids, []string{"policy.criteria.regex"}) {
		t.Fatalf("preview ids %v", ids)
	}
	status(t, sendTask(t, a, "PUT", policiesPath+"/demo-mixed", `{"criteria": {"lastBlobUpdatedDays": 9}}`), 204)
	if p, _ := storedPolicy(sec, "demo-mixed"); !reflect.DeepEqual(p.Criteria, map[string]any{"lastBlobUpdatedDays": float64(9), "regex": ".*-SNAPSHOT.*"}) {
		t.Fatalf("stored %+v", p)
	}
}

// A save that sends back what GET answered never refuses the stored
// criteria, also for a policy with no known criterion set: one with only a
// criterion of another name, and one with only a known value the check
// refuses. The null keys of the view are compared as absent (R6, R11b,
// review F1). Dropping the stored known value is still refused.
func TestASaveSendsBackTheViewOfAPolicyWithNoKnownCriterionSet(t *testing.T) {
	for name, stored := range map[string]map[string]any{
		"demo-pattern": {"regex": ".*"},
		"demo-zero":    {"lastBlobUpdatedDays": float64(0)},
	} {
		sec := newFakeSecurity()
		sec.snap.CleanupPolicies = []model.CleanupPolicy{{Name: name, Format: "raw", Criteria: stored, Origin: model.OriginImport}}
		a := newSecureApp(t, sec)
		r := a.send("GET", policiesPath+"/"+name, "basic", asAdmin)
		status(t, r, 200)
		view := r.object(t)
		body, err := json.Marshal(map[string]any{"name": name, "format": "raw", "notes": "", "criteria": view["criteria"]})
		if err != nil {
			t.Fatal(err)
		}
		status(t, sendTask(t, a, "PUT", policiesPath+"/"+name, string(body)), 204)
		if p, _ := storedPolicy(sec, name); !reflect.DeepEqual(p.Criteria, stored) || p.Origin != model.OriginLocal {
			t.Fatalf("%s: stored %+v", name, p)
		}
		known := `{"criteria": {"lastBlobUpdatedDays": null, "lastDownloadedDays": null}}`
		if name == "demo-pattern" {
			status(t, sendTask(t, a, "PUT", policiesPath+"/"+name, known), 204)
			if p, _ := storedPolicy(sec, name); !reflect.DeepEqual(p.Criteria, stored) {
				t.Fatalf("%s: stored %+v", name, p)
			}
		} else if ids := refusedFields(t, sendTask(t, a, "PUT", policiesPath+"/"+name, known)); !slices.Equal(ids, []string{"criteria"}) {
			t.Fatalf("%s: ids %v", name, ids)
		}
	}
}

// A policy named preview is a policy like any other while the server is
// frozen: its save and its delete answer 503 and write nothing, while the
// preview itself, POST of the same path, is served (review F2, SB2).
func TestAPolicyNamedPreviewWaitsForTheRelease(t *testing.T) {
	a, sec := policyApp(t)
	status(t, sendTask(t, a, "POST", policiesPath, `{"name": "preview", "format": "raw", "criteria": {"lastBlobUpdatedDays": 4}}`), 201)
	status(t, sendTask(t, a, "POST", "/service/rest/v1/read-only/freeze", ""), 204)
	updatesBefore, resetsBefore := sec.writes()
	for _, c := range [][3]string{
		{"PUT", policiesPath + "/preview", `{"criteria": {"lastBlobUpdatedDays": 8}}`},
		{"DELETE", policiesPath + "/preview", ""},
	} {
		r := sendTask(t, a, c[0], c[1], c[2])
		status(t, r, 503)
		if _, ok := r.object(t)["message"]; !ok {
			t.Fatalf("%s %s: %s", c[0], c[1], r.body)
		}
	}
	if updates, resets := sec.writes(); updates != updatesBefore || resets != resetsBefore {
		t.Fatalf("writes while frozen: %d updates, %d resets", updates-updatesBefore, resets-resetsBefore)
	}
	p, found := storedPolicy(sec, "preview")
	if !found || !reflect.DeepEqual(p.Criteria, map[string]any{"lastBlobUpdatedDays": float64(4)}) {
		t.Fatalf("stored %+v %v", p, found)
	}
	r := sendTask(t, a, "POST", policyPreviewPath, previewBody("raw", `{"lastBlobUpdatedDays": 3}`, "demo-raw"))
	status(t, r, 200)
	sameJSON(t, r.body, map[string]any{"items": []any{}, "total": 0, "truncated": false})
}

// A subject that holds only nexus:settings:update creates, saves and
// deletes a policy: 201, 204 and 204 (review F3), beside the denials of
// TestTheWritesOfCleanupPoliciesNeedTheSettingsUpdate.
func TestTheSettingsUpdateAloneWritesCleanupPolicies(t *testing.T) {
	a, sec := policyApp(t)
	sec.addBasic("demo-writer:writer-pw", subject("demo-writer", "", settingsUpdate))
	as := []string{"basic", "demo-writer:writer-pw"}
	r := sendTask(t, a, "POST", policiesPath, `{"name": "demo-written", "format": "raw", "criteria": {"lastBlobUpdatedDays": 2}}`, as...)
	status(t, r, 201)
	status(t, sendTask(t, a, "PUT", policiesPath+"/demo-written", `{"criteria": {"lastDownloadedDays": 3}}`, as...), 204)
	if p, _ := storedPolicy(sec, "demo-written"); !reflect.DeepEqual(p.Criteria, map[string]any{"lastDownloadedDays": float64(3)}) {
		t.Fatalf("stored %+v", p)
	}
	status(t, sendTask(t, a, "DELETE", policiesPath+"/demo-written", "", as...), 204)
	if _, found := storedPolicy(sec, "demo-written"); found {
		t.Fatal("the policy stays")
	}
}
