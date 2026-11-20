package state

import (
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// UpdateRouting reads the rules and links, lets fn change them and writes
// them in one transaction that raises the generation once (ruling SY3 of leg
// 6-system); an error of fn and a refused result change nothing and leave
// the generation.
func TestUpdateRouting(t *testing.T) {
	st := routingStore(t, filepath.Join(t.TempDir(), "data"), true)
	if err := st.WriteRouting(demoRouting()); err != nil {
		t.Fatal(err)
	}
	g := generationOf(t, st)
	err := st.UpdateRouting(func(r *model.Routing) error {
		if !reflect.DeepEqual(*r, mustRouting(t, st)) {
			t.Errorf("fn reads %+v", *r)
		}
		r.Rules = append(r.Rules, model.RoutingRule{Name: "demo-new", Mode: "BLOCK", Matchers: []string{"/x"}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := generationOf(t, st); got != g+1 {
		t.Fatalf("generation %d, want %d", got, g+1)
	}
	if r := mustRouting(t, st); len(r.Rules) != 4 || r.Links["demo-maven-proxy"] != "demo-block-internal" {
		t.Fatalf("%+v", r)
	}
	before := mustRouting(t, st)
	refused := errors.New("refused")
	if err := st.UpdateRouting(func(r *model.Routing) error { r.Rules = nil; return refused }); !errors.Is(err, refused) {
		t.Fatalf("%v", err)
	}
	if err := st.UpdateRouting(func(r *model.Routing) error {
		r.Rules = append(r.Rules, r.Rules[0])
		return nil
	}); err == nil {
		t.Fatal("a rule name given twice was written")
	}
	if got := generationOf(t, st); got != g+1 || !reflect.DeepEqual(mustRouting(t, st), before) {
		t.Fatalf("a refused update changed the file: generation %d", got)
	}
}

// Updates of several goroutines lose none of them.
func TestUpdateRoutingInParallel(t *testing.T) {
	st := routingStore(t, filepath.Join(t.TempDir(), "data"), true)
	g := generationOf(t, st)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.UpdateRouting(func(r *model.Routing) error {
				r.Rules = append(r.Rules, model.RoutingRule{Name: "demo-" + string(rune('a'+i)), Mode: "BLOCK", Matchers: []string{"/x"}})
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if r := mustRouting(t, st); len(r.Rules) != 8 || generationOf(t, st) != g+8 {
		t.Fatalf("%d rules, generation %d", len(r.Rules), generationOf(t, st))
	}
}

func mustRouting(t *testing.T, st *Store) model.Routing {
	t.Helper()
	r, err := st.Routing()
	if err != nil {
		t.Fatal(err)
	}
	return r
}
