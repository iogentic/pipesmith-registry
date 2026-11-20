package state_test

import (
	"crypto/sha256"
	"reflect"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// Schema step 2 holds the tokens this server issues and the generation, a
// value every identity write raises (decision E3 of the execution plan; the
// Records table of docs/plans/phases.md, row Tokens).

var created = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

func token(public, kind, user string) model.Token {
	return model.Token{Public: public, Kind: kind, UserID: user, Source: "default",
		Digest: sha256.Sum256([]byte("secret of " + public)), Created: created}
}

func generation(t *testing.T, st *state.Store) int64 {
	t.Helper()
	g, err := st.Generation()
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// Every identity write raises the generation once, in its own transaction:
// a Write of any kind, a token issued or revoked, a password form replaced.
// The last use of a token is no identity write, and a write that fails or
// changes nothing leaves the value.
func TestGenerationRisesWithEveryIdentityWrite(t *testing.T) {
	_, st := create(t)
	want := int64(0)
	check := func(step string) {
		t.Helper()
		if got := generation(t, st); got != want {
			t.Fatalf("after %s: generation %d, want %d", step, got, want)
		}
	}
	check("create")
	if err := st.Write(state.Kinds, sample()); err != nil {
		t.Fatal(err)
	}
	want++
	check("a Write of every kind")
	if err := st.Write([]state.Kind{state.KindRepositories}, sample()); err != nil {
		t.Fatal(err)
	}
	want++
	check("a Write of the repositories")
	broken := sample()
	broken.Users[1].ID = broken.Users[0].ID
	if err := st.Write([]state.Kind{state.KindUsers}, broken); err == nil {
		t.Fatal("a snapshot with two users of one id was written")
	}
	check("a failed Write")
	if _, err := st.AddToken(token("pub-a", "npm", "admin"), 20); err != nil {
		t.Fatal(err)
	}
	want++
	check("a token issued")
	if err := st.TouchToken("pub-a", created.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	check("the last use of a token")
	if gone, err := st.RemoveToken("pub-a"); err != nil || !gone {
		t.Fatalf("revoke: %v %v", gone, err)
	}
	want++
	check("a token revoked")
	if gone, err := st.RemoveToken("pub-a"); err != nil || gone {
		t.Fatalf("revoke of a token that is gone: %v %v", gone, err)
	}
	check("a revoke of no token")
	const salted = "$shiro1$SHA-512$1024$bmV3LXNhbHQtb2YtdGVzdA==$bmV3LWhhc2gtb2YtdGhlLXRlc3Q="
	if changed, err := st.SetPasswordHash("admin", testHash, salted); err != nil || !changed {
		t.Fatalf("new form: %v %v", changed, err)
	}
	want++
	check("a password form replaced")
	if changed, err := st.SetPasswordHash("admin", testHash, salted); err != nil || changed {
		t.Fatalf("a second replace of the old form: %v %v", changed, err)
	}
	check("a replace whose old form is gone")
	if got := load(t, st).Users[0].PasswordHash; got != salted {
		t.Fatalf("stored form %q", got)
	}
}

// Another connection of the same file, as another process has it, sees the
// generation that a write raised.
func TestGenerationSeenByAnotherConnection(t *testing.T) {
	dir, st := create(t)
	other, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	before := generation(t, other)
	if err := st.Write([]state.Kind{state.KindRealms}, sample()); err != nil {
		t.Fatal(err)
	}
	if got := generation(t, other); got != before+1 {
		t.Fatalf("the other connection reads generation %d, want %d", got, before+1)
	}
}

func TestTokenRows(t *testing.T) {
	_, st := create(t)
	a := token("pub-a", "npm", "admin")
	b := token("pub-b", "user", "admin")
	c := token("pub-c", "npm", "ldap.person")
	c.Source = "LDAP"
	c.Created = created.Add(time.Second)
	for _, row := range []model.Token{a, b, c} {
		if revoked, err := st.AddToken(row, 20); err != nil || len(revoked) != 0 {
			t.Fatalf("add %s: %v %v", row.Public, revoked, err)
		}
	}
	got, found, err := st.Token("pub-c")
	if err != nil || !found || !reflect.DeepEqual(got, c) {
		t.Fatalf("Token: %+v %v %v, want %+v", got, found, err, c)
	}
	if _, found, err := st.Token("pub-none"); err != nil || found {
		t.Fatalf("Token of an unknown public half: %v %v", found, err)
	}
	if _, err := st.AddToken(a, 20); err == nil {
		t.Fatal("a second row with the same public half was written")
	}
	used := created.Add(90 * time.Second)
	if err := st.TouchToken("pub-a", used); err != nil {
		t.Fatal(err)
	}
	list, err := st.Tokens("admin", "default")
	if err != nil {
		t.Fatal(err)
	}
	a.LastUsed = used
	if !reflect.DeepEqual(list, []model.Token{a, b}) {
		t.Fatalf("Tokens of admin: %+v", list)
	}
	if list, err := st.Tokens("admin", "LDAP"); err != nil || len(list) != 0 {
		t.Fatalf("Tokens of admin of source LDAP: %+v %v", list, err)
	}
	// a row without a public half, a kind, a user or a source is refused
	for _, bad := range []model.Token{{Kind: "npm", UserID: "admin", Source: "default"}, {Public: "p", UserID: "admin", Source: "default"},
		{Public: "p", Kind: "npm", Source: "default"}, {Public: "p", Kind: "npm", UserID: "admin"}} {
		if _, err := st.AddToken(bad, 20); err == nil {
			t.Fatalf("a row without a field was written: %+v", bad)
		}
	}
}

// A user holds at most keep tokens per kind: the oldest go first, in the
// order they were issued, also when they carry the same time (decision N10
// of the phase 3 plan).
func TestTokenCap(t *testing.T) {
	_, st := create(t)
	var revoked []string
	for _, public := range []string{"p1", "p2", "p3", "p4", "p5"} {
		gone, err := st.AddToken(token(public, "npm", "admin"), 3)
		if err != nil {
			t.Fatal(err)
		}
		revoked = append(revoked, gone...)
	}
	if !reflect.DeepEqual(revoked, []string{"p1", "p2"}) {
		t.Fatalf("revoked %q, want p1 and p2", revoked)
	}
	// another kind and another user count apart
	for _, row := range []model.Token{token("u1", "user", "admin"), token("o1", "npm", "viewer")} {
		if gone, err := st.AddToken(row, 3); err != nil || len(gone) != 0 {
			t.Fatalf("%s: %q %v", row.Public, gone, err)
		}
	}
	list, err := st.Tokens("admin", "default")
	if err != nil {
		t.Fatal(err)
	}
	var publics []string
	for _, row := range list {
		publics = append(publics, row.Public)
	}
	if !reflect.DeepEqual(publics, []string{"p3", "p4", "p5", "u1"}) {
		t.Fatalf("rows of admin %q", publics)
	}
}

// Review S4: the token rows of a local user id leave with the id in the
// write that replaces the users, and an id that arrives starts without the
// rows an earlier user of that id left, so a user of the same name added
// later never inherits a token. Rows of another source stay.
func TestTokenRowsLeaveWithTheirUser(t *testing.T) {
	_, st := create(t)
	users := func(ids ...string) *model.Snapshot {
		snap := &model.Snapshot{}
		for _, id := range ids {
			snap.Users = append(snap.Users, model.User{ID: id, FirstName: "Demo", LastName: id, Email: id + "@example.com",
				Status: "active", Source: "default"})
		}
		return snap
	}
	if err := st.Write([]state.Kind{state.KindUsers}, users("demo-alice", "demo-bob")); err != nil {
		t.Fatal(err)
	}
	ldap := token("ldap-bob", "npm", "demo-bob")
	ldap.Source = "LDAP"
	for _, tok := range []model.Token{token("alice-1", "npm", "demo-alice"), token("bob-1", "npm", "demo-bob"),
		token("bob-2", "user", "demo-bob"), ldap, token("carol-1", "npm", "demo-carol")} {
		if _, err := st.AddToken(tok, 20); err != nil {
			t.Fatal(err)
		}
	}
	held := func(want ...string) {
		t.Helper()
		var got []string
		for _, id := range []string{"demo-alice", "demo-bob", "demo-carol"} {
			for _, source := range []string{"default", "LDAP"} {
				rows, err := st.Tokens(id, source)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range rows {
					got = append(got, r.Public)
				}
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("token rows %q, want %q", got, want)
		}
	}
	// demo-bob leaves: its local rows go, its row of the source LDAP stays
	if err := st.Write([]state.Kind{state.KindUsers}, users("demo-alice")); err != nil {
		t.Fatal(err)
	}
	held("alice-1", "ldap-bob", "carol-1")
	// demo-carol arrives where a row of an earlier demo-carol was left
	if err := st.Write([]state.Kind{state.KindUsers}, users("demo-alice", "demo-carol")); err != nil {
		t.Fatal(err)
	}
	held("alice-1", "ldap-bob")
	// a write of the same users keeps every row
	if err := st.Write(state.Kinds, users("demo-alice", "demo-carol")); err != nil {
		t.Fatal(err)
	}
	held("alice-1", "ldap-bob")
}

// Review 3-11: the cap follows the order of issue, not the clock of the
// writer. After the clock steps back, a user at the cap keeps the token that
// was just issued and loses the oldest one.
func TestTokenCapKeepsTheNewTokenAfterTheClockStepsBack(t *testing.T) {
	_, st := create(t)
	for i, public := range []string{"c1", "c2", "c3"} {
		row := token(public, "npm", "admin")
		row.Created = created.Add(time.Duration(i) * time.Hour)
		if _, err := st.AddToken(row, 3); err != nil {
			t.Fatal(err)
		}
	}
	back := token("c4", "npm", "admin")
	back.Created = created.Add(-24 * time.Hour)
	gone, err := st.AddToken(back, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gone, []string{"c1"}) {
		t.Fatalf("revoked %q, want c1", gone)
	}
	if _, ok, err := st.Token("c4"); err != nil || !ok {
		t.Fatalf("the token issued after the clock stepped back is gone: %v %v", ok, err)
	}
}
