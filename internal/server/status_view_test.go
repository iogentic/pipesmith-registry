package server

import (
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

func TestStatusShowsTheHostToItsAdministratorOnly(t *testing.T) {
	sec := newFakeSecurity()
	sec.addBasic("demo-pusher:pusher-pw", subject("demo-pusher", "", "nexus:repository-view:docker:demo-docker:browse"))
	a := newSecureApp(t, sec)
	full := a.send("GET", "/api/status", "basic", "demo-admin:admin-pw").object(t)
	if full["data_dir"] == nil || full["bind"] == nil || len(full["docker_ports"].([]any)) != 1 {
		t.Fatalf("administrator: %v", full)
	}
	if store := full["stores"].([]any)[0].(map[string]any); store["root"] == nil {
		t.Fatalf("administrator store: %v", store)
	}
	for pair, ports := range map[string]int{"demo-reader:reader-pw": 0, "demo-pusher:pusher-pw": 1, asNobody: 0} {
		r := a.send("GET", "/api/status", "basic", pair)
		seen := r.object(t)
		if len(seen) != len(full) {
			t.Errorf("%s: keys %v", pair, seen)
		}
		if seen["data_dir"] != nil || seen["bind"] != nil {
			t.Errorf("%s: data_dir %v, bind %v", pair, seen["data_dir"], seen["bind"])
		}
		if got := len(seen["docker_ports"].([]any)); got != ports {
			t.Errorf("%s: %d Docker ports, want %d", pair, got, ports)
		}
		store := seen["stores"].([]any)[0].(map[string]any)
		if store["root"] != nil || store["repos"] != nil || store["manifest"] != nil || store["name"] != "store-a" || store["files"] == nil {
			t.Errorf("%s: store %v", pair, store)
		}
		if root := full["stores"].([]any)[0].(map[string]any)["root"].(string); strings.Contains(string(r.body), root) {
			t.Errorf("%s reads the root of a store: %s", pair, r.body)
		}
	}
}

func TestStatusViewHidesTheScanError(t *testing.T) {
	sec := newFakeSecurity()
	a := newSecureApp(t, sec)
	outcome := pyjson.NewObject().Set("store", "store-a").Set("error", "[Errno 13] Permission denied: '/srv/host/store-a'")
	cancelled := pyjson.NewObject().Set("store", "store-b").Set("error", "cancelled")
	status := func() *pyjson.Object {
		return pyjson.NewObject().Set("data_dir", "/srv/host/data").Set("bind", "127.0.0.1").Set("docker_ports", []any{}).
			Set("stores", []any{}).Set("scan", pyjson.NewObject().Set("running", false).
			Set("last", pyjson.NewObject().Set("store-a", outcome).Set("store-b", cancelled)))
	}
	req := &request{subject: sec.basic["demo-reader:reader-pw"], resolved: true}
	seen := a.srv.statusView(req, status())
	text := dumped(t, seen)
	if strings.Contains(text, "/srv/host") || !strings.Contains(text, `"error": "`+hiddenScanError+`"`) || !strings.Contains(text, `"error": "cancelled"`) {
		t.Errorf("reader: %s", text)
	}
	if e, _ := outcome.Get("error"); !strings.Contains(e.(string), "/srv/host") {
		t.Errorf("the outcome of the job changed: %v", e)
	}
	admin := &request{subject: sec.basic["demo-admin:admin-pw"], resolved: true}
	if text := dumped(t, a.srv.statusView(admin, status())); !strings.Contains(text, "/srv/host/store-a") {
		t.Errorf("administrator: %s", text)
	}
}

func dumped(t *testing.T, v *pyjson.Object) string {
	t.Helper()
	data, err := pyjson.Dumps(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
