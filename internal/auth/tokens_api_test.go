package auth_test

import (
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Leg 6-api, rulings AP1 to AP4: a signed in subject issues npm and user
// tokens for itself, the list holds no secret, a token is revoked by its
// id, and an imported key is listed by an id that is no digest.

func signedIn(t *testing.T, e *env, user string) *model.Subject {
	t.Helper()
	s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), user, password(t, user)))
	if err != nil || s == nil {
		t.Fatalf("sign in of %s: %+v %v", user, s, err)
	}
	return s
}

func TestIssueTokenForTheSubject(t *testing.T) {
	e, _ := newStoreEnv(t, tokenSnapshot(t))
	me := signedIn(t, e, "demo-reader")
	text, row, err := e.svc.IssueToken(me, auth.KindNpm, "laptop")
	if err != nil || !npmToken.MatchString(text) || row.Name != "laptop" || row.UserID != "demo-reader" || row.Source != auth.SourceLocal {
		t.Fatalf("issue: %d characters, %+v, %v", len(text), row, err)
	}
	if s, err := subjectOf(e, bearer(text)); err != nil || s == nil || s.User.ID != "demo-reader" {
		t.Fatalf("the issued token does not sign in: %+v %v", s, err)
	}
	user, _, err := e.svc.IssueToken(me, auth.KindUser, "laptop")
	if err != nil || !userToken.MatchString(user) {
		t.Fatalf("a user token of the same name: %v", err)
	}
	if _, _, err := e.svc.IssueToken(me, auth.KindNpm, "laptop"); !errors.Is(err, model.ErrTokenName) {
		t.Fatalf("a second npm token named laptop: %v", err)
	}
	list, err := e.svc.ListTokens("demo-reader", auth.SourceLocal)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}
	for _, entry := range list {
		_, secret := halves(t, map[string]string{auth.KindNpm: text, auth.KindUser: user}[entry.Kind])
		if entry.ID != entry.Public || entry.Name != "laptop" || entry.Imported || strings.Contains(entry.ID+entry.Public, secret) {
			t.Fatalf("entry %+v", entry)
		}
	}
	owner, source, found, err := e.svc.TokenOwner(row.Public)
	if err != nil || !found || owner != "demo-reader" || source != auth.SourceLocal {
		t.Fatalf("owner: %s %s %v %v", owner, source, found, err)
	}
	if removed, err := e.svc.RemoveTokenID(row.Public); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if s, err := subjectOf(e, bearer(text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("a removed token signs in: %+v %v", s, err)
	}
	if removed, err := e.svc.RemoveTokenID(row.Public); err != nil || removed {
		t.Fatalf("a second remove: %v %v", removed, err)
	}
	if _, _, found, _ := e.svc.TokenOwner(row.Public); found {
		t.Fatal("a removed token has an owner")
	}
}

func TestIssueTokenRefusesKinds(t *testing.T) {
	snap := tokenSnapshot(t)
	snap.Realms = slices.DeleteFunc(snap.Realms, func(r string) bool { return r == auth.RealmUserToken })
	snap.Realms = append(snap.Realms, auth.RealmDocker)
	e, _ := newStoreEnv(t, snap)
	me := signedIn(t, e, "demo-admin")
	for _, kind := range []string{auth.KindDocker, auth.KindUser, "nuget", ""} {
		var refusal *auth.TokenKindError
		if _, _, err := e.svc.IssueToken(me, kind, "x1"); !errors.As(err, &refusal) || refusal.Reason == "" {
			t.Errorf("kind %q: %v", kind, err)
		}
	}
	if list, err := e.svc.ListTokens("demo-admin", auth.SourceLocal); err != nil || len(list) != 0 {
		t.Fatalf("a refused kind left a row: %+v %v", list, err)
	}
	if _, _, err := e.svc.IssueToken(nil, auth.KindNpm, "x1"); !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("issue without a subject: %v", err)
	}
	if _, _, err := e.svc.IssueToken(me, auth.KindNpm, "x1"); err != nil {
		t.Fatalf("npm while its realm is on: %v", err)
	}
}

func TestImportedKeysInTheList(t *testing.T) {
	key := oldKey(t)
	e := importEnv(t, tokenSnapshot(t), map[string]model.ImportedKey{key: local(auth.KindNpm, "demo-reader")})
	list, err := e.svc.ListTokens("demo-reader", auth.SourceLocal)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	k := list[0]
	row, _ := e.row(t, auth.KindNpm, key)
	if !k.Imported || k.Kind != auth.KindNpm || k.Name != "" || k.Public != "" || k.ID != auth.ImportedKeyID(row.Digest) ||
		!strings.HasPrefix(k.ID, "imported-") || strings.Contains(k.ID, string(row.Digest[:])) || !k.Created.Equal(time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("imported entry %+v", k)
	}
	if owner, source, found, err := e.svc.TokenOwner(k.ID); err != nil || !found || owner != "demo-reader" || source != auth.SourceLocal {
		t.Fatalf("owner of the imported key: %s %s %v %v", owner, source, found, err)
	}
	if removed, err := e.svc.RemoveTokenID(k.ID); err != nil || !removed {
		t.Fatalf("remove of the imported key: %v %v", removed, err)
	}
	if _, found := e.row(t, auth.KindNpm, key); found {
		t.Fatal("the imported key is still stored")
	}
}

func TestChangeOwnPassword(t *testing.T) {
	e, store := newStoreEnv(t, tokenSnapshot(t))
	me := signedIn(t, e, "demo-reader")
	old := password(t, "demo-reader")
	hash, err := auth.HashPassword("example-new-password-1")
	if err != nil {
		t.Fatal(err)
	}
	r := request(http.MethodPost, "/api/account/password", nil)
	start := time.Now()
	if err := e.svc.ChangeOwnPassword(r, me, "wrong-password", hash); !errors.Is(err, auth.ErrWrongPassword) {
		t.Fatalf("a wrong password: %v", err)
	}
	if took := time.Since(start); took < auth.RefusalFloor {
		t.Fatalf("a wrong password took %v, less than the floor %v", took, auth.RefusalFloor)
	}
	if err := e.svc.ChangeOwnPassword(r, me, password(t, "demo-admin"), hash); !errors.Is(err, auth.ErrWrongPassword) {
		t.Fatalf("the password of another user: %v", err)
	}
	if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-reader", old)); err != nil || s == nil {
		t.Fatalf("a refused change changed the password: %+v %v", s, err)
	}
	if err := e.svc.ChangeOwnPassword(r, me, old, hash); err != nil {
		t.Fatalf("the right password: %v", err)
	}
	snap, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if u := userOf(snap, "demo-reader"); u.PasswordHash != hash {
		t.Fatalf("stored form %q, want the new one", u.PasswordHash)
	}
	// at once: the old password signs in no more, the new one does
	if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-reader", old)); err == nil && s != nil {
		t.Fatal("the old password still signs in")
	}
	if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-reader", "example-new-password-1")); err != nil || s == nil {
		t.Fatalf("the new password does not sign in: %v", err)
	}
	// the form the check verified is gone: a change with the old password is refused
	if err := e.svc.ChangeOwnPassword(r, me, old, hash); !errors.Is(err, auth.ErrWrongPassword) {
		t.Fatalf("a second change with the old password: %v", err)
	}
}

// The compare and set: a form that another change stored after the view
// was loaded keeps the row, and the change is refused.
func TestChangeOwnPasswordKeepsAFormStoredMeanwhile(t *testing.T) {
	e, store := newStoreEnv(t, tokenSnapshot(t))
	me := signedIn(t, e, "demo-reader")
	snap, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	other, err := auth.HashPassword("example-other-password-2")
	if err != nil {
		t.Fatal(err)
	}
	// a write beside the service: the view keeps the old form until it loads again
	if ok, err := store.SetPasswordHash("demo-reader", userOf(snap, "demo-reader").PasswordHash, other); err != nil || !ok {
		t.Fatalf("set: %v %v", ok, err)
	}
	mine, err := auth.HashPassword("example-new-password-1")
	if err != nil {
		t.Fatal(err)
	}
	err = e.svc.ChangeOwnPassword(request(http.MethodPost, "/api/account/password", nil), me, password(t, "demo-reader"), mine)
	if !errors.Is(err, auth.ErrWrongPassword) {
		t.Fatalf("a change over a form stored meanwhile: %v", err)
	}
	if snap, err = store.Load(); err != nil || userOf(snap, "demo-reader").PasswordHash != other {
		t.Fatalf("the form stored meanwhile was replaced: %v", err)
	}
}

// A user with an unsalted form: the check stores the salted form first,
// and the change replaces that one.
func TestChangeOwnPasswordAfterTheRehash(t *testing.T) {
	e, store := newStoreEnv(t, tokenSnapshot(t))
	me := &model.Subject{User: model.User{ID: "demo-legacy-sha1", Source: auth.SourceLocal}}
	hash, err := auth.HashPassword("example-new-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ChangeOwnPassword(request(http.MethodPost, "/api/account/password", nil), me, password(t, "demo-legacy-sha1"), hash); err != nil {
		t.Fatalf("the change of a user with an unsalted form: %v", err)
	}
	snap, err := store.Load()
	if err != nil || userOf(snap, "demo-legacy-sha1").PasswordHash != hash {
		t.Fatalf("stored form after the change: %v", err)
	}
}

func TestChangeOwnPasswordRefusesADirectoryUser(t *testing.T) {
	e, store := newStoreEnv(t, tokenSnapshot(t))
	me := &model.Subject{User: model.User{ID: "demo-reader", Source: auth.SourceLDAP}}
	hash, err := auth.HashPassword("example-new-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ChangeOwnPassword(request(http.MethodPost, "/api/account/password", nil), me, password(t, "demo-reader"), hash); err == nil {
		t.Fatal("a directory user changed the password of the local user of its id")
	}
	if snap, err := store.Load(); err != nil || userOf(snap, "demo-reader").PasswordHash == hash {
		t.Fatalf("the local row changed: %v", err)
	}
}

// Finding F1 of the review of leg 6-api: a named token is never revoked by
// the bound of 20 per kind. The 21st named token of a kind is refused with
// model.ErrTokenLimit and the list keeps 20; 21 npm logins revoke only the
// oldest login and leave every named npm token in place.
func TestNamedTokensOutliveTheBound(t *testing.T) {
	e, _ := newStoreEnv(t, tokenSnapshot(t))
	me := signedIn(t, e, "demo-reader")
	var named []string
	for i := range auth.MaxTokensPerKind {
		text, _, err := e.svc.IssueToken(me, auth.KindNpm, "named-"+string(rune('a'+i)))
		if err != nil {
			t.Fatalf("named token %d: %v", i+1, err)
		}
		named = append(named, text)
	}
	if _, _, err := e.svc.IssueToken(me, auth.KindNpm, "named-extra"); !errors.Is(err, model.ErrTokenLimit) {
		t.Fatalf("the 21st named npm token: %v, want ErrTokenLimit", err)
	}
	if _, _, err := e.svc.IssueToken(me, auth.KindUser, "named-extra"); err != nil {
		t.Fatalf("a named user token beside 20 named npm tokens: %v", err)
	}
	var logins []string
	for range auth.MaxTokensPerKind + 1 {
		logins = append(logins, npmLogin(t, e, "demo-reader", password(t, "demo-reader")).Text)
	}
	for i, text := range named {
		if s, err := subjectOf(e, bearer(text)); err != nil || s == nil {
			t.Fatalf("named npm token %d after 21 logins: %v", i+1, err)
		}
	}
	if s, err := subjectOf(e, bearer(logins[0])); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("the oldest login after the 21st: %+v %v", s, err)
	}
	if s, err := subjectOf(e, bearer(logins[1])); err != nil || s == nil {
		t.Fatalf("the second login after the 21st: %v", err)
	}
	list, err := e.svc.ListTokens("demo-reader", auth.SourceLocal)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, entry := range list {
		counts[entry.Kind+" "+map[bool]string{true: "named", false: "login"}[entry.Name != ""]]++
	}
	if want := map[string]int{"npm named": 20, "npm login": 20, "user named": 1}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("list counts %v, want %v", counts, want)
	}
}

// Finding F2 of the review: the subject tells a sign in with a token, a
// bearer token or a user token as the Basic password, from one with a
// session or the password, also when the Basic check comes from the cache.
func TestSubjectKnowsATokenSignIn(t *testing.T) {
	e, _ := newStoreEnv(t, tokenSnapshot(t))
	me := signedIn(t, e, "demo-reader")
	if me.ByToken {
		t.Fatalf("a sign in with the password: %+v", me)
	}
	npm, _, err := e.svc.IssueToken(me, auth.KindNpm, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	user, _, err := e.svc.IssueToken(me, auth.KindUser, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if s, err := subjectOf(e, bearer(npm)); err != nil || s == nil || !s.ByToken {
			t.Fatalf("an npm bearer token: %+v %v", s, err)
		}
		if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-reader", user)); err != nil || s == nil || !s.ByToken {
			t.Fatalf("a user token as the Basic password: %+v %v", s, err)
		}
		if s := signedIn(t, e, "demo-reader"); s.ByToken {
			t.Fatalf("the password after a token: %+v", s)
		}
	}
	cookie, _ := e.mustSignIn("demo-reader", password(t, "demo-reader"))
	r := request(http.MethodGet, "/", nil)
	r.AddCookie(cookie)
	if s, err := subjectOf(e, r); err != nil || s == nil || s.ByToken || s.Session == "" {
		t.Fatalf("a session: %+v %v", s, err)
	}
}

// Finding F3 of the review: a user token revoked by its id signs in no
// more at once, also when a Basic check with it is in the cache.
func TestRevokedUserTokenFailsAtOnce(t *testing.T) {
	e, _ := newStoreEnv(t, tokenSnapshot(t))
	me := signedIn(t, e, "demo-reader")
	text, row, err := e.svc.IssueToken(me, auth.KindUser, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-reader", text)); err != nil || s == nil {
			t.Fatalf("the user token before the revoke: %+v %v", s, err)
		}
	}
	if removed, err := e.svc.RemoveTokenID(row.Public); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-reader", text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("the revoked user token signs in: %+v %v", s, err)
	}
}
