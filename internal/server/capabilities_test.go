package server

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

const (
	capabilitiesPath = "/service/rest/v1/capabilities"
	supportPath      = "/api/capabilities/support"
	hookSecret       = "example-hook-secret-7"
)

// capabilitySecurity is the fake with the capabilities of the content
// fixture under invented ids, and a subject that may change capabilities
// and holds nothing else.
func capabilitySecurity() *fakeSecurity {
	sec := newFakeSecurity()
	sec.snap.Capabilities = []model.Capability{
		{ID: "c-base", TypeID: "baseurl", Enabled: true, Properties: map[string]string{"url": "https://registry.example.com/"}, Origin: "import"},
		{ID: "c-hook", TypeID: "demo.webhook", Notes: "Invented hook", Properties: map[string]string{"url": "https://hooks.example.com/demo"}, Origin: "import"},
		{ID: "c-role", TypeID: "defaultrole", Properties: map[string]string{"role": "demo-administrators"}, Origin: "import"},
	}
	sec.addBasic("demo-capman:capman-pw", subject("demo-capman", "", "nexus:capabilities:*"))
	return sec
}

func capabilityWrite(t *testing.T, a *secureApp, method, target, body, who string) *answer {
	t.Helper()
	return do(t, a.addr(), method, target, []byte(body), "basic", who, "Content-Type", jsonType)
}

// The capabilities of contract S35 and S36: the list in the shape of
// CapabilityDTO, the support answer from kinds.json, a save and a delete
// with 204, and the refusals of SY4, SY5 and SY8 in the shapes of section
// Errors, none of which writes.
func TestCapabilities(t *testing.T) {
	sec := capabilitySecurity()
	a := newSecureApp(t, sec)
	list := a.send("GET", capabilitiesPath, "basic", asAdmin)
	status(t, list, 200)
	var items []map[string]any
	if err := json.Unmarshal(list.body, &items); err != nil || len(items) != 3 {
		t.Fatalf("%v %s", err, list.body)
	}
	for _, item := range items {
		keys := slices.Sorted(func(yield func(string) bool) {
			for k := range item {
				if !yield(k) {
					return
				}
			}
		})
		if !slices.Equal(keys, []string{"enabled", "id", "notes", "properties", "type"}) {
			t.Fatalf("keys %v", keys)
		}
	}
	support := a.send("GET", supportPath, "basic", asAdmin)
	status(t, support, 200)
	var kinds map[string]struct {
		Supported *bool   `json:"supported"`
		Reason    *string `json:"reason"`
	}
	if err := json.Unmarshal(support.body, &kinds); err != nil || len(kinds) != 3 {
		t.Fatalf("%v %s", err, support.body)
	}
	if k := kinds["defaultrole"]; k.Supported == nil || !*k.Supported || k.Reason == nil || *k.Reason == "" {
		t.Fatalf("defaultrole %s", support.body)
	}
	if k := kinds["demo.webhook"]; k.Supported == nil || *k.Supported {
		t.Fatalf("demo.webhook %s", support.body)
	}

	// refusals, none writes
	for _, c := range []struct {
		method, target, body, who string
		status                    int
		field                     string
	}{
		{"PUT", capabilitiesPath + "/c-hook", `{"enabled":true}`, asAdmin, 400, "enabled"},
		{"PUT", capabilitiesPath + "/c-hook", `{"properties":{"url":"https://hooks.example.com/demo","secret":"` + hookSecret + `"}}`, asAdmin, 400, "properties.secret"},
		{"PUT", capabilitiesPath + "/c-base", `{"type":"defaultrole"}`, asAdmin, 400, "type"},
		{"PUT", capabilitiesPath + "/c-base", `{"origin":"local"}`, asAdmin, 400, "*"},
		{"PUT", capabilitiesPath + "/c-none", `{"notes":"x"}`, asAdmin, 404, ""},
		{"DELETE", capabilitiesPath + "/c-none", ``, asAdmin, 404, ""},
		{"PUT", capabilitiesPath + "/c-role", `{"enabled":true}`, "demo-capman:capman-pw", 403, ""},
		{"PUT", capabilitiesPath + "/c-base", `{"notes":"x"}`, asReader, 403, ""},
		{"DELETE", capabilitiesPath + "/c-base", ``, asReader, 403, ""},
		{"PUT", capabilitiesPath + "/c-base", `{"notes":"x"}`, asNobody, 403, ""},
		{"PUT", capabilitiesPath + "/c-base", `{"notes":"x"}`, "", 401, ""},
	} {
		label := c.method + " " + c.target + " " + c.body
		var r *answer
		if c.who == "" {
			r = do(t, a.addr(), c.method, c.target, []byte(c.body), "Content-Type", jsonType)
		} else {
			r = capabilityWrite(t, a, c.method, c.target, c.body, c.who)
		}
		if r.status != c.status {
			t.Fatalf("%s: %d %s", label, r.status, r.body)
		}
		if strings.Contains(string(r.body), hookSecret) {
			t.Fatalf("%s: the answer holds the secret: %s", label, r.body)
		}
		if c.status == 400 {
			var fields []map[string]string
			if err := json.Unmarshal(r.body, &fields); err != nil || len(fields) != 1 || fields[0]["id"] != c.field {
				t.Fatalf("%s: %s, want the field %s", label, r.body, c.field)
			}
		}
		if updates, _ := sec.writes(); updates != 0 {
			t.Fatalf("%s: a refusal wrote", label)
		}
	}
	if sec.Snapshot().DefaultRole != "" {
		t.Fatalf("a refused save set the default role %q", sec.Snapshot().DefaultRole)
	}
	for _, target := range []string{capabilitiesPath, supportPath} {
		status(t, a.send("GET", target, "basic", asNobody), 403)
		status(t, a.send("GET", target), 401)
	}

	// the save and the delete
	r := capabilityWrite(t, a, "PUT", capabilitiesPath+"/c-base",
		`{"id":"c-base","type":"baseurl","notes":"saved","enabled":false,"properties":{"url":"https://other.example.com/"}}`, asAdmin)
	if r.status != 204 || len(r.body) != 0 {
		t.Fatalf("save: %d %q", r.status, r.body)
	}
	saved := sec.Snapshot().Capabilities[0]
	if saved.Notes != "saved" || saved.Enabled || saved.Properties["url"] != "https://other.example.com/" || saved.Origin != "import" {
		t.Fatalf("saved %+v", saved)
	}
	r = capabilityWrite(t, a, "PUT", capabilitiesPath+"/c-role", `{"enabled":true}`, asAdmin)
	if r.status != 204 || sec.Snapshot().DefaultRole != "demo-administrators" {
		t.Fatalf("the default role: %d %q, %q", r.status, r.body, sec.Snapshot().DefaultRole)
	}
	r = capabilityWrite(t, a, "DELETE", capabilitiesPath+"/c-hook", ``, asAdmin)
	if r.status != 204 || len(r.body) != 0 {
		t.Fatalf("delete: %d %q", r.status, r.body)
	}
	if n := len(sec.Snapshot().Capabilities); n != 2 {
		t.Fatalf("%d capabilities after the delete", n)
	}
	if updates, _ := sec.writes(); updates != 3 {
		t.Fatalf("%d writes, want 3", updates)
	}
	status(t, a.send("GET", capabilitiesPath, "basic", "demo-capman:capman-pw"), 200)
}
