package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/server"
)

// routes is a hidden command: it prints the route table of a server with
// security on, with the rule and the examples of each entry, for the walk of
// tests/identity (task 19 of the phase 3 plan). The help does not name it and
// it is no choice of the command line, so the commands of the reference stay
// as they are.

func TestRoutesCommand(t *testing.T) {
	dataDir(t)
	code, out, errOut := run(t, "routes", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("routes --json: %d %q", code, errOut)
	}
	var doc struct {
		Placeholders map[string]string  `json:"placeholders"`
		Rules        map[string]string  `json:"rules"`
		Routes       []server.RouteInfo `json:"routes"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("routes --json is no JSON: %v", err)
	}
	if len(doc.Routes) != len(server.Routes(false)) || len(doc.Placeholders) == 0 || len(doc.Rules) != 8 {
		t.Fatalf("%d routes, %d placeholders, %d rules", len(doc.Routes), len(doc.Placeholders), len(doc.Rules))
	}
	code, out, _ = run(t, "routes")
	if code != 0 || !strings.HasPrefix(out, "TABLE     RULE         METHODS") || strings.Count(out, "\n") != len(doc.Routes)+1 {
		t.Fatalf("routes: %d %q", code, out)
	}
	// --writes lists the table of a server with writes on
	code, out, errOut = run(t, "routes", "--json", "--writes")
	if code != 0 || errOut != "" || json.Unmarshal([]byte(out), &doc) != nil || len(doc.Routes) != len(server.Routes(true)) {
		t.Fatalf("routes --json --writes: %d %q %d routes", code, errOut, len(doc.Routes))
	}
	if code, out, _ := run(t, "routes", "--help"); code != 0 || out != "usage: pipesmith-registry routes [-h] [--json] [--writes]\n\n"+
		"options:\n  -h, --help  show this help message and exit\n  --json\n  --writes\n" {
		t.Fatalf("routes --help: %d %q", code, out)
	}
	if code, _, errOut := run(t, "routes", "--yaml"); code != 2 || !strings.Contains(errOut, "unrecognized arguments: --yaml") {
		t.Fatalf("routes --yaml: %d %q", code, errOut)
	}
	_, help, _ := run(t, "help")
	if strings.Contains(help, "routes") {
		t.Fatal("the help names the hidden command routes")
	}
	if _, _, errOut := run(t, "frobnicate"); strings.Contains(errOut, "routes") {
		t.Fatal("the choices of the command line name the hidden command routes")
	}
}
