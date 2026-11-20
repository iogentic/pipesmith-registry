package state

import (
	"reflect"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

func demoCapabilities() []model.Capability {
	return []model.Capability{
		{ID: "0c a0", TypeID: "demo.baseurl", Enabled: true, Notes: "invented", Origin: model.OriginImport,
			Properties: map[string]string{"url": "https://registry.example.com"}},
		{ID: "0c a1", TypeID: "demo.other", Origin: model.OriginLocal, Properties: map[string]string{}},
	}
}

// A put stores a capability with its properties, a remove takes it out,
// each change raises the generation, and Counts names the kind.
func TestCapabilitiesThroughUpdate(t *testing.T) {
	st := newStore(t)
	g := generationOf(t, st)
	for _, c := range []model.Capability{demoCapabilities()[1], demoCapabilities()[0]} {
		if err := st.PutCapability(c); err != nil {
			t.Fatal(err)
		}
	}
	if got := generationOf(t, st); got != g+2 {
		t.Fatalf("generation %d after two puts, want %d", got, g+2)
	}
	got, err := st.Capabilities()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, demoCapabilities()) {
		t.Fatalf("capabilities\n%+v\nwant\n%+v", got, demoCapabilities())
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snap.Capabilities, demoCapabilities()) || Counts(snap)["capabilities"] != 2 {
		t.Fatalf("Load gives %+v, Counts %v", snap.Capabilities, Counts(snap))
	}
	g = generationOf(t, st)
	if ok, err := st.RemoveCapability("0c a9"); ok || err != nil || generationOf(t, st) != g {
		t.Fatalf("remove of no capability: %v %v", ok, err)
	}
	if ok, err := st.RemoveCapability("0c a0"); !ok || err != nil || generationOf(t, st) != g+1 {
		t.Fatalf("remove: %v %v", ok, err)
	}
	if err := st.PutCapability(model.Capability{ID: "0c a2", Origin: ""}); err == nil {
		t.Fatal("a capability without an origin was stored")
	}
}
