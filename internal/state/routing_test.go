package state

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// routingStore opens the state file of dir: with setUp it creates the
// folder and the file first (Create), else it opens a file that exists.
func routingStore(t *testing.T, dir string, setUp bool) *Store {
	t.Helper()
	open := Open
	if setUp {
		open = Create
	}
	st, err := open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func demoRouting() model.Routing {
	return model.Routing{
		Rules: []model.RoutingRule{
			{Name: "demo-block-internal", Description: "Keeps the internal names off the remote", Mode: "BLOCK",
				Matchers: []string{"^/com/example/internal/.*", "^/@demo-internal/.*"}},
			{Name: "demo-allow-releases", Mode: "ALLOW", Matchers: []string{"/com/example/demo-lib/[0-9.]+/.*"}},
			{Name: "demo-empty", Mode: "BLOCK", Matchers: []string{}},
		},
		Links: map[string]string{"demo-maven-proxy": "demo-block-internal", "demo-other-proxy": "demo-gone"},
	}
}

// sortedRouting is demoRouting as Routing gives it: the rules by name.
func sortedRouting() model.Routing {
	r := demoRouting()
	slices.SortFunc(r.Rules, func(a, b model.RoutingRule) int { return strings.Compare(a.Name, b.Name) })
	return r
}

// Step 3 is the third step of the numbered list and makes the two tables
// of the routing rules.
func TestStep3IsInTheList(t *testing.T) {
	if len(steps) < 3 || steps[2].Version != 3 {
		t.Fatalf("SchemaVersion %d with %d steps", SchemaVersion, len(steps))
	}
	dir := filepath.Join(t.TempDir(), "data")
	st := routingStore(t, dir, true)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	schema := dump(t, Path(dir), false)
	for _, want := range []string{"table routing_rules on routing_rules: CREATE TABLE routing_rules",
		"table routing_links on routing_links: CREATE TABLE routing_links"} {
		if !strings.Contains(schema, want) {
			t.Fatalf("a fresh file has no %q:\n%s", want, schema)
		}
	}
}

// Rules and links come back as they were written: the rules by name, the
// matchers in their order, a link to a rule that does not exist kept (the
// routing package blocks such a repository).
func TestRoutingRoundTrip(t *testing.T) {
	st := routingStore(t, filepath.Join(t.TempDir(), "data"), true)
	if err := st.WriteRouting(demoRouting()); err != nil {
		t.Fatal(err)
	}
	got, err := st.Routing()
	if err != nil {
		t.Fatal(err)
	}
	if want := sortedRouting(); !reflect.DeepEqual(got, want) {
		t.Fatalf("routing\n%+v\nwant\n%+v", got, want)
	}
	// a second write replaces every rule and every link
	next := model.Routing{Rules: []model.RoutingRule{{Name: "demo-only", Mode: "ALLOW", Matchers: nil}},
		Links: map[string]string{"demo-npm-proxy": "demo-only"}}
	if err := st.WriteRouting(next); err != nil {
		t.Fatal(err)
	}
	got, err = st.Routing()
	if err != nil {
		t.Fatal(err)
	}
	want := model.Routing{Rules: []model.RoutingRule{{Name: "demo-only", Mode: "ALLOW", Matchers: []string{}}},
		Links: map[string]string{"demo-npm-proxy": "demo-only"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after the second write\n%+v\nwant\n%+v", got, want)
	}
	// the records of the other kinds stay where they are
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Repositories) != 0 || len(snap.Users) != 0 {
		t.Fatalf("%+v", snap)
	}
}

// A rule without a name, a name given twice and a link without a repository
// or a rule are refused, and nothing of the write is kept.
func TestWriteRoutingRefuses(t *testing.T) {
	st := routingStore(t, filepath.Join(t.TempDir(), "data"), true)
	if err := st.WriteRouting(demoRouting()); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]model.Routing{
		"no name":       {Rules: []model.RoutingRule{{Mode: "BLOCK"}}},
		"a name twice":  {Rules: []model.RoutingRule{{Name: "a", Mode: "BLOCK"}, {Name: "a", Mode: "ALLOW"}}},
		"no repository": {Links: map[string]string{"": "a"}},
		"no rule":       {Links: map[string]string{"demo-proxy": ""}},
	} {
		if err := st.WriteRouting(r); err == nil {
			t.Errorf("%s: written", name)
		}
		got, err := st.Routing()
		if err != nil || !reflect.DeepEqual(got, sortedRouting()) {
			t.Fatalf("%s: a refused write changed the rules: %+v %v", name, got, err)
		}
	}
}

// A state file of release 0.1.1 reaches schema 3: the copy of schema 1 lies
// beside it, its records load as before, it has no rules yet and takes them.
func TestReleasedFileGetsTheRoutingTables(t *testing.T) {
	dir := dataDirOf(t, "v1.sqlite")
	st := routingStore(t, dir, false)
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	sameRecords(t, got, recordsV1())
	if _, err := os.Stat(Path(dir) + ".v1"); err != nil {
		t.Fatalf("no copy of schema 1: %v", err)
	}
	routing, err := st.Routing()
	if err != nil || len(routing.Rules) != 0 || len(routing.Links) != 0 || routing.Rules == nil || routing.Links == nil {
		t.Fatalf("%+v %v", routing, err)
	}
	if err := st.WriteRouting(demoRouting()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// the migrated file has the schema of a fresh file
	fresh := filepath.Join(t.TempDir(), "fresh")
	f := routingStore(t, fresh, true)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := dump(t, Path(dir), false), dump(t, Path(fresh), false); got != want {
		t.Fatalf("the migrated fixture differs from a fresh file\nmigrated\n%s\nfresh\n%s", got, want)
	}
}
