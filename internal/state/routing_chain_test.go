package state_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/importer"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/routing"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The chain of task 4: the committed export of the fixture database
// (testdata/export/fixture-content), its mapping by the importer, a state
// file of schema 3, and the answers of
// routing for the fixture proxy, which names the rule demo-block-routed.
func TestExportImportStateRoutingChain(t *testing.T) {
	export, err := importer.ReadExport("../../testdata/export/fixture-content")
	if err != nil {
		t.Fatal(err)
	}
	if export.Routing == nil || len(export.Routing.Rules) != 3 || export.Routing.Links["demo-maven-proxy"] != "demo-block-routed" {
		t.Fatalf("the export: %+v", export.Routing)
	}
	st, err := state.Create(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Write(export.Kinds, export.Snapshot); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteRouting(*export.Routing); err != nil {
		t.Fatal(err)
	}
	stored, err := st.Routing()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, *export.Routing) {
		t.Fatalf("state holds\n%+v\nthe export\n%+v", stored, *export.Routing)
	}

	rs := routing.Compile(stored)
	for _, c := range []struct {
		repository, path string
		allowed          bool
	}{
		{"demo-maven-proxy", "/com/example/routing-blocked/demo/1.0/demo-1.0.jar", false},
		{"demo-maven-proxy", "/@demo-routing-blocked/tool/-/tool-1.0.0.tgz", false},
		{"demo-maven-proxy", "com/example/routing-blocked/demo/maven-metadata.xml", false},
		{"demo-maven-proxy", "/com/example/demo-lib/1.2.0/demo-lib-1.2.0.jar", true},
		{"demo-maven-proxy", "/com/example/routing-blocked", true},
		{"demo-npm-proxy", "/@demo-routing-blocked/tool", true},
		{"demo-maven-hosted", "/com/example/routing-blocked/demo/1.0/demo-1.0.jar", true},
	} {
		if got := rs.Allowed(c.repository, c.path); got != c.allowed {
			t.Errorf("Allowed(%q, %q) = %v, want %v", c.repository, c.path, got, c.allowed)
		}
	}
	// the rule with the look ahead is named for the start log, and no repository names it
	if p := rs.Problems(); len(p) != 1 || !strings.HasPrefix(p[0], `routing rule "demo-java-only" blocks every path: matcher 1 does not compile`) {
		t.Fatalf("problems %v", p)
	}

	// the repositories of the same file get the name of their rule in their proxy settings
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	var defined []repos.Repo
	for _, r := range snap.Repositories {
		defined = append(defined, repos.Repo{Name: r.Name, Format: r.Format, Type: r.Type, Defined: true})
	}
	rp := repos.New(defined, nil, nil).WithAttributes(snap.Repositories).WithRoutingRules(stored.Links)
	proxy, _ := rp.Get("demo-maven-proxy")
	settings, err := proxy.ProxySettings()
	if err != nil || settings.RoutingRule != "demo-block-routed" || settings.RemoteURL != "https://repo.example.com/maven2/" {
		t.Fatalf("%+v %v", settings, err)
	}
	npm, _ := rp.Get("demo-npm-proxy")
	if settings, err := npm.ProxySettings(); err != nil || settings.RoutingRule != "" {
		t.Fatalf("%+v %v", settings, err)
	}
}
