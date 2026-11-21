package state_test

import (
	"crypto/sha256"
	"errors"
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

// Step 6 (ruling AP2 of leg 6-api): a token row has a name, the empty one
// for an npm login. A name that is not empty is unique per user, source
// and kind; another kind or another user may hold it.
func TestTokenNames(t *testing.T) {
	_, st := create(t)
	named := func(public, kind, user, name string) model.Token {
		tok := token(public, kind, user)
		tok.Name = name
		return tok
	}
	for _, tok := range []model.Token{
		named("AAAAAAAAAAA", "npm", "demo-admin", "laptop"),
		named("BBBBBBBBBBB", "user", "demo-admin", "laptop"),
		named("CCCCCCCCCCC", "npm", "demo-reader", "laptop"),
		named("DDDDDDDDDDD", "npm", "demo-admin", ""),
		named("EEEEEEEEEEE", "npm", "demo-admin", ""),
	} {
		if _, err := st.AddToken(tok, 20); err != nil {
			t.Fatalf("%s: %v", tok, err)
		}
	}
	before := generation(t, st)
	if _, err := st.AddToken(named("FFFFFFFFFFF", "npm", "demo-admin", "laptop"), 20); !errors.Is(err, model.ErrTokenName) {
		t.Fatalf("a second npm token named laptop: %v", err)
	}
	if after := generation(t, st); after != before {
		t.Fatalf("the refused name raised the generation from %d to %d", before, after)
	}
	if _, found, err := st.Token("FFFFFFFFFFF"); err != nil || found {
		t.Fatalf("the refused row is stored: %v %v", found, err)
	}
	list, err := st.Tokens("demo-admin", "default")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tok := range list {
		names = append(names, tok.Kind+":"+tok.Name)
	}
	if want := []string{"npm:laptop", "user:laptop", "npm:", "npm:"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names %v, want %v", names, want)
	}
	if got, _, _ := st.Token("AAAAAAAAAAA"); got.Name != "laptop" {
		t.Fatalf("Token gives the name %q", got.Name)
	}
}

// ImportedKeyRows gives the imported keys of a user with their digests, or
// every key for an empty user.
func TestImportedKeyRows(t *testing.T) {
	_, st := create(t)
	keys := []model.ImportedKey{
		{Kind: "npm", UserID: "demo-admin", Source: "default", Digest: sha256.Sum256([]byte("a")), Created: created},
		{Kind: "docker", UserID: "demo-reader", Source: "default", Digest: sha256.Sum256([]byte("b")), Created: created},
	}
	if err := st.WriteImport(nil, nil, keys); err != nil {
		t.Fatal(err)
	}
	mine, err := st.ImportedKeyRows("demo-admin", "default")
	if err != nil || len(mine) != 1 || mine[0].Digest != keys[0].Digest || mine[0].Kind != "npm" || !mine[0].Created.Equal(created) {
		t.Fatalf("keys of demo-admin: %v %v", mine, err)
	}
	all, err := st.ImportedKeyRows("", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("every key: %v %v", all, err)
	}
}

// The bound of keep revokes npm logins only, the rows with the empty name,
// and counts only those; a named row is never revoked by it. A named row
// beyond keep named rows is refused with model.ErrTokenLimit.
func TestTokenCapSparesNamedRows(t *testing.T) {
	_, st := create(t)
	named := func(public, name string) model.Token {
		row := token(public, "npm", "admin")
		row.Name = name
		return row
	}
	for _, row := range []model.Token{named("n1", "laptop"), token("p1", "npm", "admin"), named("n2", "ci"),
		token("p2", "npm", "admin"), token("p3", "npm", "admin")} {
		if gone, err := st.AddToken(row, 3); err != nil || len(gone) != 0 {
			t.Fatalf("%s: %q %v", row.Public, gone, err)
		}
	}
	var revoked []string
	for _, public := range []string{"p4", "p5"} {
		gone, err := st.AddToken(token(public, "npm", "admin"), 3)
		if err != nil {
			t.Fatal(err)
		}
		revoked = append(revoked, gone...)
	}
	if !reflect.DeepEqual(revoked, []string{"p1", "p2"}) {
		t.Fatalf("revoked %q, want the logins p1 and p2", revoked)
	}
	if gone, err := st.AddToken(named("n3", "desk"), 3); err != nil || len(gone) != 0 {
		t.Fatalf("the third named row: %q %v", gone, err)
	}
	if _, err := st.AddToken(named("n4", "spare"), 3); !errors.Is(err, model.ErrTokenLimit) {
		t.Fatalf("a fourth named row with keep 3: %v, want ErrTokenLimit", err)
	}
	list, err := st.Tokens("admin", "default")
	if err != nil {
		t.Fatal(err)
	}
	var publics []string
	for _, row := range list {
		publics = append(publics, row.Public)
	}
	if want := []string{"n1", "n2", "p3", "p4", "p5", "n3"}; !reflect.DeepEqual(publics, want) {
		t.Fatalf("rows %q, want %q", publics, want)
	}
}
