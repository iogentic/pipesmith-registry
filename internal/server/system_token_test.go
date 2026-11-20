package server

import (
	"reflect"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Each of the seven writes of leg 6-system (create, save and delete of a
// routing rule, the test and the preview of a rule, the save and the delete
// of a capability) answers 403 to a session without the request token or
// with a wrong one and changes nothing; with the token it passes (A22).
func TestSystemWritesNeedTheRequestToken(t *testing.T) {
	a, sec := newRoutingApp(t)
	sec.fakeSecurity.mu.Lock()
	sec.fakeSecurity.snap.Capabilities = []model.Capability{
		{ID: "c-base", TypeID: "baseurl", Enabled: true, Properties: map[string]string{"url": "https://registry.example.com/"}, Origin: "import"},
		{ID: "c-hook", TypeID: "demo.webhook", Properties: map[string]string{}, Origin: "import"},
	}
	sec.fakeSecurity.mu.Unlock()
	writes := []struct {
		method, target, body string
		status               int
	}{
		{"POST", rulesPath, `{"name":"demo-token","mode":"BLOCK","matchers":["/a"]}`, 204},
		{"PUT", rulesPath + "/demo-token", `{"mode":"ALLOW","matchers":["/a"]}`, 204},
		{"DELETE", rulesPath + "/demo-token", ``, 204},
		{"POST", testPath, `{"mode":"BLOCK","matchers":["/a"],"path":"/a"}`, 200},
		{"POST", previewPath, `{"scope":"all","path":"/a"}`, 200},
		{"PUT", capabilitiesPath + "/c-base", `{"notes":"by a session"}`, 204},
		{"DELETE", capabilitiesPath + "/c-hook", ``, 204},
	}
	state := func() (model.Routing, []model.Capability, int, int) {
		rs, _ := sec.Routing()
		updates, _ := sec.fakeSecurity.writes()
		return rs, sec.Snapshot().Capabilities, sec.writeCount(), updates
	}
	for _, c := range writes {
		label := c.method + " " + c.target
		rules, caps, routingWrites, updates := state()
		for _, headers := range [][]string{{"Cookie", "fake-session=s1"}, {"Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1x"}} {
			r := do(t, a.addr(), c.method, c.target, []byte(c.body), append(headers, "Content-Type", jsonType)...)
			if r.status != 403 || r.object(t)["message"] != "missing or wrong request token" {
				t.Fatalf("%s %v: %d %s", label, headers, r.status, r.body)
			}
		}
		r2, c2, w2, u2 := state()
		if !reflect.DeepEqual(r2, rules) || !reflect.DeepEqual(c2, caps) || w2 != routingWrites || u2 != updates {
			t.Fatalf("%s: a write without the token changed something", label)
		}
		r := do(t, a.addr(), c.method, c.target, []byte(c.body), "Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1",
			"Content-Type", jsonType)
		if r.status != c.status {
			t.Fatalf("%s with the token: %d %s", label, r.status, r.body)
		}
	}
	if updates, _ := sec.fakeSecurity.writes(); updates != 2 || sec.writeCount() != 3 {
		t.Fatalf("%d capability writes, %d routing writes; want 2 and 3", updates, sec.writeCount())
	}
}
