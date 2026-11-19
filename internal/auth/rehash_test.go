package auth_test

import (
	"encoding/base64"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Task 3 of the phase 3 plan: a password held in an unsalted form is stored
// again in the salted default form of the old product at the next good sign
// in (lab research 05-identity.md, section Rehash on login: a legacy hex
// hash is replaced after a successful sign in, a $shiro1$ hash of the
// default form never). Every other stored form stays as it is.

// saltedForm checks the salted default form: Shiro 1, SHA-512, 1024 rounds,
// 16 bytes of salt and a digest of 64 bytes, both in standard Base64.
func saltedForm(t *testing.T, stored string) {
	t.Helper()
	fields := strings.Split(stored, "$")
	if len(fields) != 6 || fields[1] != "shiro1" || fields[2] != "SHA-512" || fields[3] != "1024" {
		t.Fatalf("not the salted default form: %d fields", len(fields))
	}
	salt, err1 := base64.StdEncoding.DecodeString(fields[4])
	digest, err2 := base64.StdEncoding.DecodeString(fields[5])
	if err1 != nil || err2 != nil || len(salt) != 16 || len(digest) != 64 {
		t.Fatalf("salt of %d bytes and digest of %d bytes, want 16 and 64: %v %v", len(salt), len(digest), err1, err2)
	}
}

func TestHashPassword(t *testing.T) {
	const pass = "demo-new-pass-ä-1"
	first, err := auth.HashPassword(pass)
	if err != nil {
		t.Fatal(err)
	}
	saltedForm(t, first)
	if !auth.VerifyPassword(pass, first) || auth.VerifyPassword(pass+"x", first) {
		t.Fatal("the new form does not verify its password alone")
	}
	second, err := auth.HashPassword(pass)
	if err != nil || second == first {
		t.Fatalf("a second hash of the same password repeats the first: %v", err)
	}
	if _, err := auth.HashPassword(strings.Repeat("a", auth.MaxPasswordBytes+1)); err == nil {
		t.Fatal("a password longer than MaxPasswordBytes got a form that never verifies")
	}
	if _, err := auth.HashPassword(""); err == nil {
		t.Fatal("an empty password got a form")
	}
}

func usersOf(t *testing.T, st auth.Store) map[string]model.User {
	t.Helper()
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]model.User{}
	for _, u := range snap.Users {
		out[u.ID] = u
	}
	return out
}

func TestRehashAtTheNextGoodSignIn(t *testing.T) {
	e, store := newStoreEnv(t, fixture(t))
	before := usersOf(t, store)
	gen := generation(t, store)

	// a wrong password changes nothing
	if rec := e.signIn("demo-legacy-sha1", "demo-wrong-pass-1", "192.0.2.60"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	if got := usersOf(t, store); !reflect.DeepEqual(got, before) {
		t.Fatal("a refused sign in changed a row")
	}

	// the interface sign in of the SHA-1 user and a Basic request of the MD5
	// user each store the salted form
	e.mustSignIn("demo-legacy-sha1", password(t, "demo-legacy-sha1"))
	if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-legacy-md5", password(t, "demo-legacy-md5"))); err != nil || s == nil {
		t.Fatalf("Basic of the MD5 user: %v", err)
	}
	after := usersOf(t, store)
	for _, id := range []string{"demo-legacy-sha1", "demo-legacy-md5"} {
		saltedForm(t, after[id].PasswordHash)
		if !auth.VerifyPassword(password(t, id), after[id].PasswordHash) {
			t.Fatalf("the new form of %s does not verify its password", id)
		}
		row, old := after[id], before[id]
		row.PasswordHash, old.PasswordHash = "", ""
		if !reflect.DeepEqual(row, old) {
			t.Fatalf("the rehash of %s changed more than the form: %+v", id, row)
		}
	}
	if got := generation(t, store); got != gen+2 {
		t.Fatalf("generation %d after two rehashes, want %d", got, gen+2)
	}

	// the salted rows stay byte equal: Shiro 1 of SHA-512 and of SHA-256,
	// both PBKDF2 forms; so does the row of a user who may not sign in
	for _, id := range []string{"demo-admin", "demo-reader", "demo-pbkdf2", "demo-pbkdf2-sha1", "demo-unicode"} {
		e.mustSignIn(id, password(t, id))
	}
	if rec := e.signIn("demo-off", password(t, "demo-off"), "192.0.2.61"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled user: %d", rec.Code)
	}
	final := usersOf(t, store)
	for id, u := range before {
		if id == "demo-legacy-sha1" || id == "demo-legacy-md5" {
			continue
		}
		if final[id].PasswordHash != u.PasswordHash || !reflect.DeepEqual(final[id], u) {
			t.Errorf("the row of %s changed", id)
		}
	}
	if got := generation(t, store); got != gen+2 {
		t.Fatalf("generation %d after sign ins of salted forms, want %d", got, gen+2)
	}

	// the same password still signs in, before and after the reload that
	// takes the new form
	e.mustSignIn("demo-legacy-sha1", password(t, "demo-legacy-sha1"))
	e.clock.Add(auth.ReloadInterval)
	e.mustSignIn("demo-legacy-sha1", password(t, "demo-legacy-sha1"))
	if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-legacy-md5", password(t, "demo-legacy-md5"))); err != nil || s == nil {
		t.Fatalf("Basic of the MD5 user after the reload: %v", err)
	}
	if got := usersOf(t, store); !reflect.DeepEqual(got, final) {
		t.Fatal("a sign in with the salted form wrote the row again")
	}
	if got := generation(t, store); got != gen+2 {
		t.Fatalf("generation %d at the end, want %d", got, gen+2)
	}
	if strings.Contains(e.logs.String(), password(t, "demo-legacy-sha1")) || strings.Contains(e.logs.String(), "$shiro1$") {
		t.Fatal("the log holds a password or a form")
	}
	if !strings.Contains(e.logs.String(), "salted form") {
		t.Errorf("no log line for the rehash: %q", e.logs)
	}
}

// A form that another process changed since the snapshot is not replaced.
func TestRehashLeavesAFormChangedMeanwhile(t *testing.T) {
	e, store := newStoreEnv(t, fixture(t))
	const other = "$shiro1$SHA-512$1024$b3RoZXItc2FsdC0xMjM0NQ==$" +
		"b3RoZXItZGlnZXN0LW9mLXRoZS10ZXN0LW9mLWEtY2hhbmdlZC1mb3JtLW9mLXRoZS1zYW1lLXVzZXIhIQ=="
	changed, err := store.SetPasswordHash("demo-legacy-sha1", usersOf(t, store)["demo-legacy-sha1"].PasswordHash, other)
	if err != nil || !changed {
		t.Fatalf("set up: %v %v", changed, err)
	}
	gen := generation(t, store)
	// the service still holds the snapshot with the SHA-1 form
	e.mustSignIn("demo-legacy-sha1", password(t, "demo-legacy-sha1"))
	if got := usersOf(t, store)["demo-legacy-sha1"].PasswordHash; got != other {
		t.Fatal("the rehash replaced a form another process wrote")
	}
	if g := generation(t, store); g != gen {
		t.Fatalf("generation %d, want %d", g, gen)
	}
}

func generation(t *testing.T, st auth.Store) int64 {
	t.Helper()
	g, err := st.Generation()
	if err != nil {
		t.Fatal(err)
	}
	return g
}
