package importer_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/importer"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/routing"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The routing rules of the content set. Table and columns after the public
// source of the old product, tag release-3.87.2-01, read 2026-11-19:
// RoutingRuleDAO.xml creates repository_routing_rule with id, name, mode,
// description and matchers (JSON), and ConfigurationDAO.xml gives the table
// repository the column routing_rule_id, a UUID or null, without a foreign
// key. The fixture rules are invented (tests/import/FixtureDb.java).

const routingFile = "repository_routing_rule.jsonl"

func wantRouting() *model.Routing {
	return &model.Routing{
		Rules: []model.RoutingRule{
			{Name: "demo-allow-releases", Description: "Only the released library and the metadata", Mode: "ALLOW",
				Matchers: []string{"^/com/example/demo-lib/[0-9.]+/.*", `.*/maven-metadata\.xml`}},
			{Name: "demo-block-routed", Description: "Keeps two invented names off the remote", Mode: "BLOCK",
				Matchers: []string{"^/com/example/routing-blocked/.*", "^/@demo-routing-blocked/.*"}},
			{Name: "demo-java-only", Description: "A look ahead, which Java compiles and Go does not", Mode: "BLOCK",
				Matchers: []string{"^/com/example/(?!demo-).*"}},
		},
		Links: map[string]string{"demo-maven-proxy": "demo-block-routed"},
	}
}

func ruleRow(id, name, mode, description, matchers string) string {
	return `{"id":"` + id + `","name":"` + name + `","mode":"` + mode + `","description":"` + description +
		`","matchers":` + matchers + `,"_removed":[]}`
}

// The export of the fixture holds three rules and the link of the maven
// proxy that the importer of 0.1.1 dropped.
func TestReadContentExportRoutingRules(t *testing.T) {
	export, err := importer.ReadExport(fixtureContent)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(export.Routing, wantRouting()) {
		g, _ := json.MarshalIndent(export.Routing, "", " ")
		t.Fatalf("routing\n%s", g)
	}
	if len(export.RoutingWithoutRules) != 0 {
		t.Fatalf("%v", export.RoutingWithoutRules)
	}
	// the rules are no record kind of Snapshot, and the snapshot is the one of before
	if !slices.Equal(export.Kinds, []state.Kind{state.KindBlobStores, state.KindRepositories, state.KindCleanupPolicies,
		state.KindTasks, state.KindCapabilities}) {
		t.Fatalf("kinds %v", export.Kinds)
	}
	same(t, export.Snapshot, wantContent(t))
}

// An export of the content set from before the routing table holds no
// rules; a repository that names a rule is listed, as its rule cannot be
// known, and keeps a link to its rule id, which no rule has, so that the
// routing package blocks every path of it (fail closed, finding 5-3 of
// review 5).
func TestContentExportWithoutTheRoutingTable(t *testing.T) {
	dir := copyExport(t, fixtureContent)
	if err := os.Remove(filepath.Join(dir, routingFile)); err != nil {
		t.Fatal(err)
	}
	editManifest(t, dir, func(m map[string]any) {
		files := m["files"].([]any)
		m["files"] = slices.DeleteFunc(files, func(f any) bool { return f.(map[string]any)["file"] == routingFile })
	})
	export, err := importer.ReadExport(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := &model.Routing{Rules: []model.RoutingRule{}, Links: map[string]string{"demo-maven-proxy": "0a0e0000-0000-4000-8000-000000000001"}}
	if !reflect.DeepEqual(export.Routing, want) || !slices.Equal(export.RoutingWithoutRules, []string{"demo-maven-proxy"}) {
		t.Fatalf("%+v %v", export.Routing, export.RoutingWithoutRules)
	}
	rs := routing.Compile(*export.Routing)
	if rs.Allowed("demo-maven-proxy", "/com/example/demo-lib/1.2.0/demo-lib-1.2.0.jar") || !rs.Allowed("demo-npm-proxy", "/demo-pkg") {
		t.Fatal("the proxy that names an unknown rule is not blocked, or another proxy is")
	}
	same(t, export.Snapshot, wantContent(t))
	// the table is optional at its place only: a manifest without the repositories is still refused
	editManifest(t, dir, func(m map[string]any) { m["files"] = m["files"].([]any)[:1] })
	if _, err := importer.ReadExport(dir); err == nil {
		t.Fatal("a content export without its repositories was read")
	}
}

// A repository that names a rule id the export does not hold is refused,
// with the repository named: guessing would decide what the remote sees.
func TestRoutingRuleIDWithoutItsRule(t *testing.T) {
	dir := copyExport(t, fixtureContent)
	writeRows(t, dir, routingFile,
		ruleRow("0a0e0000-0000-4000-8000-000000000002", "demo-allow-releases", "ALLOW", "", `["^/a/.*"]`))
	_, err := importer.ReadExport(dir)
	if err == nil || !strings.Contains(err.Error(), `repository "demo-maven-proxy"`) ||
		!strings.Contains(err.Error(), "routing rule") {
		t.Fatalf("a link to a rule the export does not hold: %v", err)
	}
}

// Rows that are no routing rules are refused with the file and the row
// named, before anything is written.
func TestRoutingRuleRowRefusals(t *testing.T) {
	for name, rows := range map[string][]string{
		"matchers that are no list": {ruleRow("0a0e0000-0000-4000-8000-000000000001", "demo-block-routed", "BLOCK", "", `"^/a"`)},
		"a matcher that is no text": {ruleRow("0a0e0000-0000-4000-8000-000000000001", "demo-block-routed", "BLOCK", "", `["^/a", 7]`)},
		"a rule without a name":     {ruleRow("0a0e0000-0000-4000-8000-000000000001", "", "BLOCK", "", `[]`)},
		"a name given twice": {ruleRow("0a0e0000-0000-4000-8000-000000000001", "demo-block-routed", "BLOCK", "", `[]`),
			ruleRow("0a0e0000-0000-4000-8000-000000000009", "demo-block-routed", "ALLOW", "", `[]`)},
		"a rule without an id": {ruleRow("", "demo-block-routed", "BLOCK", "", `[]`)},
	} {
		t.Run(name, func(t *testing.T) {
			dir := copyExport(t, fixtureContent)
			writeRows(t, dir, routingFile, rows...)
			cfg := dataDir(t)
			_, err := importer.Import(dir, cfg)
			if err == nil || !strings.Contains(err.Error(), routingFile) {
				t.Fatalf("refusal %v", err)
			}
			if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
				t.Fatalf("a refused import touched the data directory: %v", err)
			}
		})
	}
}

// An import into a data directory of this binary keeps the routing rules
// and their links in the tables of schema step 3, beside everything else.
func TestImportKeepsTheRoutingRules(t *testing.T) {
	cfg := dataDir(t)
	result, err := importer.Import(fixtureContent, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !result.RoutingKept || !reflect.DeepEqual(result.Routing, wantRouting()) {
		t.Fatalf("kept %v, routing %+v", result.RoutingKept, result.Routing)
	}
	same(t, loadState(t, cfg), wantContent(t))
	st, err := state.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	routing, err := st.Routing()
	if err != nil || !reflect.DeepEqual(&routing, wantRouting()) {
		t.Fatalf("state.sqlite holds %+v %v, want %+v", routing, err, wantRouting())
	}
	// an identity import holds no routing rules and says nothing of them
	result, err = importer.Import(fixture, cfg)
	if err != nil || result.Routing != nil || result.RoutingKept {
		t.Fatalf("%+v %v", result, err)
	}
}
