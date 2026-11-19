package state_test

import (
	"errors"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The writes of the identity commands (task 17 of the phase 3 plan): Update
// reads the records and writes the kinds named in one transaction, so that a
// write of another process between the read and the write cannot be lost.

func TestUpdateReadsAndWritesInOneTransaction(t *testing.T) {
	_, st := create(t)
	if err := st.Write(state.Kinds, sample()); err != nil {
		t.Fatal(err)
	}
	before := generation(t, st)
	err := st.Update([]state.Kind{state.KindUsers}, func(snap *model.Snapshot) error {
		if len(snap.Users) != 2 || snap.Users[0].PasswordHash != testHash || len(snap.Repositories) != 2 {
			t.Fatalf("Update hands over %d users and %d repositories", len(snap.Users), len(snap.Repositories))
		}
		snap.Users[1].Status = "active"
		// a kind that is not named keeps its rows whatever fn does with them
		snap.Repositories = nil
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := sorted()
	want.Users[1].Status = "active"
	same(t, load(t, st), want)
	if got := generation(t, st); got != before+1 {
		t.Fatalf("generation %d after Update, want %d", got, before+1)
	}
}

func TestUpdateKeepsEverythingWhenFnFails(t *testing.T) {
	_, st := create(t)
	if err := st.Write(state.Kinds, sample()); err != nil {
		t.Fatal(err)
	}
	before := generation(t, st)
	refused := errors.New("refused by the command")
	err := st.Update([]state.Kind{state.KindUsers}, func(snap *model.Snapshot) error {
		snap.Users = nil
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("Update returns %v, want the error of fn", err)
	}
	same(t, load(t, st), sorted())
	if got := generation(t, st); got != before {
		t.Fatalf("generation %d after a refused Update, want %d", got, before)
	}
	for _, kinds := range [][]state.Kind{nil, {"nonsense"}} {
		if err := st.Update(kinds, func(*model.Snapshot) error { return nil }); err == nil {
			t.Errorf("Update of %v passes", kinds)
		}
	}
}

// The default role is the settings row default_role, a kind of its own: a
// write of the realms, as an import makes it, keeps it.
func TestDefaultRoleSetting(t *testing.T) {
	_, st := create(t)
	if got := load(t, st).DefaultRole; got != "" {
		t.Fatalf("a new state file has the default role %q", got)
	}
	snap := sample()
	snap.DefaultRole = "demo-viewer"
	if err := st.Write([]state.Kind{state.KindDefaultRole}, snap); err != nil {
		t.Fatal(err)
	}
	if got := load(t, st).DefaultRole; got != "demo-viewer" {
		t.Fatalf("default role %q", got)
	}
	snap.DefaultRole = ""
	if err := st.Write([]state.Kind{state.KindRealms, state.KindUsers}, snap); err != nil {
		t.Fatal(err)
	}
	if got := load(t, st).DefaultRole; got != "demo-viewer" {
		t.Fatalf("a write of the realms changed the default role to %q", got)
	}
	if err := st.Write([]state.Kind{state.KindDefaultRole}, snap); err != nil {
		t.Fatal(err)
	}
	if got := load(t, st).DefaultRole; got != "" {
		t.Fatalf("default role %q after it was cleared", got)
	}
}

func TestRaiseGeneration(t *testing.T) {
	_, st := create(t)
	if err := st.Write(state.Kinds, sample()); err != nil {
		t.Fatal(err)
	}
	before := generation(t, st)
	if err := st.RaiseGeneration(); err != nil {
		t.Fatal(err)
	}
	if got := generation(t, st); got != before+1 {
		t.Fatalf("generation %d, want %d", got, before+1)
	}
	same(t, load(t, st), sorted())
}
