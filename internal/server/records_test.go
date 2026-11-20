package server

import (
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
	idstate "github.com/iogentic/pipesmith-registry/internal/state"
)

// A repository that exists only as a record of state.sqlite is served and
// written to like one of repositories.json (ruling ST3), from the request
// after the change of the state on.
func TestARecordWithoutADefinitionIsServed(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	sec := w.opts.security.(*fakeSecurity)
	if r := do(t, w.addr(), "GET", "/repository/demo-made/docs/a.txt", nil, "basic", asAdmin); r.status != 404 {
		t.Fatalf("before the record: %d %q", r.status, r.body)
	}
	err := sec.Update([]idstate.Kind{idstate.KindRepositories}, func(s *model.Snapshot) error {
		s.Repositories = append(s.Repositories,
			model.Repository{Name: "demo-made", Format: "raw", Type: "hosted", Online: true, Attributes: storage("ALLOW")},
			model.Repository{Name: "demo-made-group", Format: "raw", Type: "group", Online: true,
				Attributes: map[string]any{"group": map[string]any{"memberNames": []any{"demo-made"}}}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	put := do(t, w.addr(), "PUT", "/repository/demo-made/docs/a.txt", []byte("made"), "basic", asAdmin)
	if put.status != 201 {
		t.Fatalf("PUT: %d %q", put.status, put.body)
	}
	for _, target := range []string{"/repository/demo-made/docs/a.txt", "/repository/demo-made-group/docs/a.txt"} {
		if r := do(t, w.addr(), "GET", target, nil, "basic", asAdmin); r.status != 200 || string(r.body) != "made" {
			t.Fatalf("GET %s: %d %q", target, r.status, r.body)
		}
	}
	row, err := w.srv.rp.index.Lookup("demo-made", "docs/a.txt")
	if err != nil || row == nil || row.Store != "local-write" {
		t.Fatalf("%+v %v", row, err)
	}
	repo := w.repo("demo-made-group")
	if repo.Type != "group" || repo.Format != "raw" || len(repo.Members) != 1 || repo.Defined {
		t.Fatalf("%+v", repo)
	}
}
