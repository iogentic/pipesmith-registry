package server

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/node"
)

var nodeUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// GET /v1/system/node answers {"nodeId"} with the id of the file node-id,
// the same id the status checks name, and a second server on the same data
// directory answers it again.
func TestNodeAnswersTheIDOfTheDataDirectory(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	got := a.send("GET", "/service/rest/v1/system/node", "basic", asAdmin)
	status(t, got, 200)
	m := got.object(t)
	id, _ := m["nodeId"].(string)
	if len(m) != 1 || !nodeUUID.MatchString(id) {
		t.Fatalf("answer %#v, want one key nodeId with a UUID", m)
	}
	data, err := os.ReadFile(filepath.Join(a.cfg.DataDir, node.FileName))
	if err != nil || string(data) != id+"\n" {
		t.Fatalf("file %q %v, want %q", data, err, id)
	}
	if a.srv.nodeID() != id {
		t.Fatalf("status checks name %q, the node route %q", a.srv.nodeID(), id)
	}
	check := a.send("GET", "/service/rest/beta/status/check/"+id, "basic", asAdmin)
	status(t, check, 200)
	again, err := New(a.cfg, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.nodeID() != id {
		t.Fatalf("second start %q, want %q", again.nodeID(), id)
	}
}

// A data directory whose node-id holds no UUID does not start.
func TestMalformedNodeFileStopsTheStart(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	path := filepath.Join(a.cfg.DataDir, node.FileName)
	if err := os.WriteFile(path, []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if srv, err := New(a.cfg, nil, Options{}); err == nil {
		_ = srv.Close()
		t.Fatal("a server started over a malformed node-id")
	}
	if data, _ := os.ReadFile(path); string(data) != "broken\n" {
		t.Fatalf("the file became %q", data)
	}
}
