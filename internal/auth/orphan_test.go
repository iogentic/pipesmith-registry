package auth_test

import (
	"errors"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Review S1: the token of a local user that the view does not hold is
// revoked only when the view is of the generation of the store. A user added
// after the load of the view keeps a token issued meanwhile, and so does
// every user while the store does not answer.
func TestOrphanRevokeNeedsTheGenerationOfTheView(t *testing.T) {
	e, store := newStoreEnv(t, tokenSnapshot(t))
	rewrite(t, store, func(s *model.Snapshot) {
		s.Users = append(s.Users, model.User{ID: "demo-bob", FirstName: "Demo", LastName: "Bob", Email: "demo-bob@example.com",
			Status: "active", Source: auth.SourceLocal, Roles: []string{"demo-maven-reader"}})
	})
	text, row, err := auth.NewToken(auth.KindNpm, "demo-bob", auth.SourceLocal, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddToken(row, auth.MaxTokensPerKind); err != nil {
		t.Fatal(err)
	}
	held := func(when string) {
		t.Helper()
		if list, err := store.Tokens("demo-bob", auth.SourceLocal); err != nil || len(list) != 1 {
			t.Fatalf("%s: tokens of demo-bob %+v %v", when, list, err)
		}
	}
	refused := func(when string) {
		t.Helper()
		if s, err := subjectOf(e, bearer(text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
			t.Fatalf("%s: %+v %v", when, s, err)
		}
	}
	refused("within the reload interval")
	held("within the reload interval")
	store.fail.Store(true)
	e.clock.Add(auth.ReloadInterval)
	refused("while the store does not answer")
	held("while the store does not answer")
	store.fail.Store(false)
	e.clock.Add(auth.ReloadInterval)
	if s, err := subjectOf(e, bearer(text)); err != nil || s == nil || s.User.ID != "demo-bob" {
		t.Fatalf("after the reload: %+v %v", s, err)
	}
}
