package server

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	idstate "github.com/iogentic/pipesmith-registry/internal/state"
)

// A capability with a stored secret property (A11, SY5): the secret file
// the import writes, and the same value in a record that holds the key. No
// answer of the area holds the value, and a save and a delete leave the
// secret file as it is.
func TestCapabilityAnswersHoldNoStoredSecret(t *testing.T) {
	const secret = "example-stored-hook-secret-9e4"
	sec := capabilitySecurity()
	sec.dataDir = t.TempDir()
	sec.snap.Capabilities[1].Properties["secret"] = secret
	path := idstate.PropertySecretPath(sec.dataDir, idstate.SecretKindCapability, "c-hook", "secret")
	if err := idstate.WriteSecret(path, []byte(secret)); err != nil {
		t.Fatal(err)
	}
	a := newSecureApp(t, sec)
	answers := []*answer{
		a.send("GET", capabilitiesPath, "basic", asAdmin),
		a.send("GET", supportPath, "basic", asAdmin),
		capabilityWrite(t, a, "PUT", capabilitiesPath+"/c-hook", `{"enabled":true}`, asAdmin),
		capabilityWrite(t, a, "PUT", capabilitiesPath+"/c-hook", `{"notes":"saved"}`, asAdmin),
		a.send("GET", capabilitiesPath, "basic", asAdmin),
		capabilityWrite(t, a, "DELETE", capabilitiesPath+"/c-hook", ``, asAdmin),
		a.send("GET", capabilitiesPath, "basic", asAdmin),
	}
	if answers[0].status != 200 || !strings.Contains(string(answers[0].body), "https://hooks.example.com/demo") {
		t.Fatalf("the list: %d %s", answers[0].status, answers[0].body)
	}
	for i, r := range answers {
		if strings.Contains(string(r.body), secret) {
			t.Fatalf("answer %d holds the stored secret: %s", i, r.body)
		}
	}
	if answers[3].status != 204 || answers[5].status != 204 {
		t.Fatalf("save %d, delete %d", answers[3].status, answers[5].status)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, []byte(secret)) {
		t.Fatalf("the secret file after the save and the delete: %q %v", data, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("%d files in the secrets folder", len(entries))
	}
}
