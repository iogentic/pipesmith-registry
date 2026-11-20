package state

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// newStore is a new state file at the last schema.
func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := Create(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// generationOf is the generation of st.
func generationOf(t *testing.T, st *Store) int64 {
	t.Helper()
	g, err := st.Generation()
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func demoPolicies() []model.CleanupPolicy {
	return []model.CleanupPolicy{
		{Name: "demo-age", Format: "maven2", Notes: "invented", Origin: model.OriginImport,
			Criteria: map[string]any{"lastBlobUpdated": json.Number("30"), "demoUnknown": "x"}},
		{Name: "demo-local", Format: "ALL_FORMATS", Origin: model.OriginLocal, Criteria: map[string]any{}},
	}
}

// A put stores a policy with every criterion as given, known or not, and a
// remove takes it out; each change raises the generation by one, a remove
// of no policy writes nothing. Load, Counts and the accessor agree.
func TestCleanupPoliciesThroughUpdate(t *testing.T) {
	st := newStore(t)
	g := generationOf(t, st)
	for i, p := range []model.CleanupPolicy{demoPolicies()[1], demoPolicies()[0]} {
		if err := st.PutCleanupPolicy(p); err != nil {
			t.Fatal(err)
		}
		if got := generationOf(t, st); got != g+int64(i)+1 {
			t.Fatalf("generation %d after put %d, want %d", got, i+1, g+int64(i)+1)
		}
	}
	got, err := st.CleanupPolicies()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, demoPolicies()) {
		t.Fatalf("policies\n%+v\nwant\n%+v", got, demoPolicies())
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snap.CleanupPolicies, demoPolicies()) || Counts(snap)["cleanup_policies"] != 2 {
		t.Fatalf("Load gives %+v, Counts %v", snap.CleanupPolicies, Counts(snap))
	}
	changed := demoPolicies()[0]
	changed.Notes = "changed"
	if err := st.PutCleanupPolicy(changed); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.CleanupPolicies(); len(got) != 2 || got[0].Notes != "changed" {
		t.Fatalf("after a put of the same name: %+v", got)
	}
	g = generationOf(t, st)
	if ok, err := st.RemoveCleanupPolicy("demo-none"); ok || err != nil || generationOf(t, st) != g {
		t.Fatalf("remove of no policy: %v %v, generation %d, want %d", ok, err, generationOf(t, st), g)
	}
	if ok, err := st.RemoveCleanupPolicy("demo-age"); !ok || err != nil || generationOf(t, st) != g+1 {
		t.Fatalf("remove: %v %v, generation %d, want %d", ok, err, generationOf(t, st), g+1)
	}
	if got, _ := st.CleanupPolicies(); len(got) != 1 || got[0].Name != "demo-local" {
		t.Fatalf("after the remove: %+v", got)
	}
}

// A policy without a name or with another origin is refused, and nothing
// changes.
func TestCleanupPolicyRefusals(t *testing.T) {
	st := newStore(t)
	if err := st.PutCleanupPolicy(demoPolicies()[1]); err != nil {
		t.Fatal(err)
	}
	g := generationOf(t, st)
	for _, p := range []model.CleanupPolicy{{Name: "demo-x", Origin: "bootstrap"}, {Origin: model.OriginLocal}} {
		if err := st.PutCleanupPolicy(p); err == nil {
			t.Errorf("%+v was stored", p)
		}
	}
	if got, _ := st.CleanupPolicies(); generationOf(t, st) != g || len(got) != 1 || got[0].Name != "demo-local" {
		t.Fatalf("a refusal changed the file: generation %d, want %d; %+v", generationOf(t, st), g, got)
	}
}

// An Update of another kind keeps the rows of step 5.
func TestUpdateOfAnotherKindKeepsStep5Rows(t *testing.T) {
	st := newStore(t)
	if err := st.PutCleanupPolicy(demoPolicies()[0]); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTask(demoTasks()[0]); err != nil {
		t.Fatal(err)
	}
	if err := st.PutCapability(demoCapabilities()[0]); err != nil {
		t.Fatal(err)
	}
	if err := st.Update([]Kind{KindRealms}, func(snap *model.Snapshot) error {
		snap.Realms = []string{"demo-realm"}
		snap.CleanupPolicies, snap.Tasks, snap.Capabilities = nil, nil, nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	counts := Counts(snap)
	if counts["cleanup_policies"] != 1 || counts["tasks"] != 1 || counts["capabilities"] != 1 || counts["realms"] != 1 {
		t.Fatalf("counts after the update of the realms: %v", counts)
	}
}
