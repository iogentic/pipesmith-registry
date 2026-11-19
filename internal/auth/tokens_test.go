package auth_test

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// Task 8 of the phase 3 plan: tokens are issued new by this server, in the
// wire form <kind>.<public half>.<secret half> of 8 and 32 random bytes
// (N9); the tokens table holds only a SHA-256 of the secret half; a user
// holds 20 tokens per kind (N10); Docker bearer tokens live in memory for
// 300 s (N11).

var (
	npmToken    = regexp.MustCompile(`^NpmToken\.[A-Za-z0-9_-]{11}\.[A-Za-z0-9_-]{43}$`)
	dockerToken = regexp.MustCompile(`^DockerToken\.[A-Za-z0-9_-]{11}\.[A-Za-z0-9_-]{43}$`)
	userToken   = regexp.MustCompile(`^UserToken\.[A-Za-z0-9_-]{11}\.[A-Za-z0-9_-]{43}$`)
)

// tokenSnapshot is the fixture with the realms of the three token kinds in
// the stored order.
func tokenSnapshot(t testing.TB) *model.Snapshot {
	s := fixture(t)
	s.Realms = append(s.Realms, auth.RealmNpm, auth.RealmUserToken)
	return s
}

func bearer(token string) *http.Request {
	r := request(http.MethodGet, "/repository/demo-maven/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func npmLogin(t testing.TB, e *env, user, pass string) model.IssuedToken {
	t.Helper()
	tok, err := e.svc.NpmLogin(request(http.MethodPut, "/repository/demo-npm/-/user/org.couchdb.user:"+user, nil), user, pass)
	if err != nil {
		t.Fatalf("npm login of %s: %v", user, err)
	}
	return tok
}

func halves(t testing.TB, text string) (public, secret string) {
	t.Helper()
	parts := strings.Split(text, ".")
	if len(parts) != 3 {
		t.Fatalf("a token of %d parts", len(parts))
	}
	return parts[1], parts[2]
}

func TestNpmTokenIssueUseListRevoke(t *testing.T) {
	e, store := newStoreEnv(t, tokenSnapshot(t))
	tok := npmLogin(t, e, "demo-reader", password(t, "demo-reader"))
	if !npmToken.MatchString(tok.Text) || tok.ExpiresIn != 0 || !tok.IssuedAt.Equal(e.clock.Now()) {
		t.Fatalf("issued %d characters, expires in %d, at %v", len(tok.Text), tok.ExpiresIn, tok.IssuedAt)
	}
	public, secret := halves(t, tok.Text)

	s, err := subjectOf(e, bearer(tok.Text))
	if err != nil || s == nil || s.User.ID != "demo-reader" || s.Realm != auth.RealmLocal || s.Session != "" ||
		!e.svc.Permits(s, "nexus:repository-view:maven2:demo-maven:read") || e.svc.Permits(s, "nexus:settings:read") {
		t.Fatalf("subject of the token: %+v %v", s, err)
	}
	// the request token of a session is not asked of a bearer token
	if !e.svc.CSRF(request(http.MethodPut, "/", nil), s) {
		t.Fatal("a bearer token needs a request token")
	}

	list, err := e.svc.Tokens("demo-reader", auth.SourceLocal)
	if err != nil || len(list) != 1 || list[0].Public != public || list[0].Kind != auth.KindNpm ||
		!list[0].Created.Equal(e.clock.Now()) || !list[0].LastUsed.Equal(e.clock.Now()) {
		t.Fatalf("tokens of demo-reader: %+v %v", list, err)
	}
	// the last use is written at most once a minute and raises no generation
	gen := generation(t, store)
	e.clock.Add(30 * time.Second)
	if _, err := subjectOf(e, bearer(tok.Text)); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.svc.Tokens("demo-reader", auth.SourceLocal); !list[0].LastUsed.Equal(e.clock.Now().Add(-30 * time.Second)) {
		t.Fatalf("last use after 30 s: %v", list[0].LastUsed)
	}
	e.clock.Add(auth.TouchInterval)
	if _, err := subjectOf(e, bearer(tok.Text)); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.svc.Tokens("demo-reader", auth.SourceLocal); !list[0].LastUsed.Equal(e.clock.Now()) {
		t.Fatalf("last use after the touch interval: %v", list[0].LastUsed)
	}
	if g := generation(t, store); g != gen {
		t.Fatalf("a use raised the generation from %d to %d", gen, g)
	}

	// another secret with the same public half, another kind, a cut text
	for _, forged := range []string{
		"NpmToken." + public + "." + strings.Repeat("A", 43),
		"UserToken." + public + "." + secret,
		"NpmToken." + public + "." + secret[:42],
		"NpmToken." + public + "." + secret + "A",
		"NpmToken." + public + "." + secret + ".x",
		"NpmToken.." + secret,
		"npmtoken." + public + "." + secret,
	} {
		if s, err := subjectOf(e, bearer(forged)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
			t.Errorf("forged token %d characters: %+v %v", len(forged), s, err)
		}
	}

	// revoke: someone else may not, the owner may, once
	other, _ := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-admin", password(t, "demo-admin")))
	if err := e.svc.RevokeToken(other, tok.Text); !errors.Is(err, model.ErrNoToken) {
		t.Fatalf("revoke by another user: %v", err)
	}
	if err := e.svc.RevokeToken(s, "NpmToken."+public+"."+strings.Repeat("A", 43)); !errors.Is(err, model.ErrNoToken) {
		t.Fatalf("revoke with another secret: %v", err)
	}
	if err := e.svc.RevokeToken(s, tok.Text); err != nil {
		t.Fatalf("revoke by the owner: %v", err)
	}
	if s, err := subjectOf(e, bearer(tok.Text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("revoked token: %+v %v", s, err)
	}
	if err := e.svc.RevokeToken(s, tok.Text); !errors.Is(err, model.ErrNoToken) {
		t.Fatalf("a second revoke: %v", err)
	}
	if list, err := e.svc.Tokens("demo-reader", auth.SourceLocal); err != nil || len(list) != 0 {
		t.Fatalf("tokens after the revoke: %+v %v", list, err)
	}
}

// A user token, issued as the token command of leg S3 issues it, signs in
// with Bearer while the realm of user tokens is in the stored order.
func TestUserToken(t *testing.T) {
	e, store := newStoreEnv(t, tokenSnapshot(t))
	text, row, err := auth.NewToken(auth.KindUser, "demo-admin", auth.SourceLocal, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !userToken.MatchString(text) || row.Kind != auth.KindUser || row.UserID != "demo-admin" || row.Source != auth.SourceLocal ||
		strings.Contains(text, fmt.Sprintf("%x", row.Digest)) {
		t.Fatalf("user token %d characters, row %+v", len(text), row)
	}
	if _, err := store.AddToken(row, auth.MaxTokensPerKind); err != nil {
		t.Fatal(err)
	}
	if s, err := subjectOf(e, bearer(text)); err != nil || s == nil || s.User.ID != "demo-admin" || !e.svc.Permits(s, "nexus:*") {
		t.Fatalf("user token: %+v %v", s, err)
	}
	rewrite(t, store, func(s *model.Snapshot) {
		s.Realms = slices.DeleteFunc(s.Realms, func(r string) bool { return r == auth.RealmUserToken })
	})
	e.clock.Add(auth.ReloadInterval)
	if s, err := subjectOf(e, bearer(text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("user token after its realm left the order: %+v %v", s, err)
	}
	if _, _, err := auth.NewToken("other", "demo-admin", auth.SourceLocal, e.clock.Now()); err == nil {
		t.Fatal("a token of an unknown kind was issued")
	}
}

// stateBytes are the bytes of state.sqlite and its WAL.
func stateBytes(t *testing.T, dir string) []byte {
	t.Helper()
	var all []byte
	for _, name := range []string{state.FileName, state.FileName + "-wal"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		all = append(all, data...)
	}
	return all
}

// Only a SHA-256 of the secret half reaches the file; the test searches the
// file for the secret half and finds the public half.
// The public half of a token row is its id on the command line (token
// revoke <id>). URL safe Base64 starts with '-' in 1 of 64 cases, which a
// parser of arguments takes for an option, so such a half is drawn again.
// 10,000 draws hold one with '-' first with a chance of 1 - (63/64)^10000.
func TestPublicHalfNeverStartsWithADash(t *testing.T) {
	for i := range 10_000 {
		_, row, err := auth.NewToken(auth.KindNpm, "demo-admin", auth.SourceLocal, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(row.Public, "-") {
			t.Fatalf("draw %d: the public half %s starts with a dash", i, row.Public)
		}
	}
}

func TestTokenRowHoldsOnlyTheDigest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	st, err := state.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Write(state.Kinds, tokenSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	e := storeEnv(t, st)
	var texts []string
	for _, user := range []string{"demo-reader", "demo-admin", "demo-legacy-md5"} {
		texts = append(texts, npmLogin(t, e, user, password(t, user)).Text)
	}
	docker, err := e.svc.DockerToken(basic(request(http.MethodGet, "/v2/token", nil), "demo-reader", password(t, "demo-reader")), "demo-docker")
	if err != nil {
		t.Fatal(err)
	}
	data := stateBytes(t, dir)
	for _, text := range texts {
		public, secret := halves(t, text)
		if bytes.Contains(data, []byte(secret)) || bytes.Contains(data, []byte(text)) {
			t.Errorf("state.sqlite holds the secret half of a token")
		}
		if !bytes.Contains(data, []byte(public)) {
			t.Errorf("state.sqlite lacks the public half %s: the search reads the wrong bytes", public)
		}
	}
	public, secret := halves(t, docker.Text)
	if bytes.Contains(data, []byte(secret)) || bytes.Contains(data, []byte(public)) {
		t.Error("a Docker bearer token reached state.sqlite")
	}
}

// A user holds at most 20 tokens per kind; the 21st revokes the oldest
// (decision N10).
func TestTokenCapPerKind(t *testing.T) {
	e, _ := newStoreEnv(t, tokenSnapshot(t))
	var texts []string
	for i := range auth.MaxTokensPerKind + 1 {
		texts = append(texts, npmLogin(t, e, "demo-reader", password(t, "demo-reader")).Text)
		e.clock.Add(time.Duration(i%2) * time.Millisecond) // equal times as well as later ones
	}
	if s, err := subjectOf(e, bearer(texts[0])); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("the oldest token after the 21st: %+v %v", s, err)
	}
	for i, text := range texts[1:] {
		if s, err := subjectOf(e, bearer(text)); err != nil || s == nil {
			t.Fatalf("token %d: %v", i+2, err)
		}
	}
	list, err := e.svc.Tokens("demo-reader", auth.SourceLocal)
	if err != nil || len(list) != auth.MaxTokensPerKind {
		t.Fatalf("%d tokens, want %d: %v", len(list), auth.MaxTokensPerKind, err)
	}
	// another user and another kind count apart
	npmLogin(t, e, "demo-admin", password(t, "demo-admin"))
	if s, err := subjectOf(e, bearer(texts[1])); err != nil || s == nil {
		t.Fatalf("a token of another user revoked one of demo-reader: %v", err)
	}
}

// A token signs its user in while the user is active and its realms are in
// the stored order; a change ends it at the next request after the reload.
func TestTokenOfADisabledOrRemovedUser(t *testing.T) {
	e, store := newStoreEnv(t, tokenSnapshot(t))
	reader := npmLogin(t, e, "demo-reader", password(t, "demo-reader"))
	legacy := npmLogin(t, e, "demo-legacy-sha1", password(t, "demo-legacy-sha1"))
	off := npmLogin(t, e, "demo-admin", password(t, "demo-admin"))
	rewrite(t, store, func(s *model.Snapshot) {
		userOf(s, "demo-reader").Status = "disabled"
		userOf(s, "demo-admin").Status = "locked"
		s.Users = slices.DeleteFunc(s.Users, func(u model.User) bool { return u.ID == "demo-legacy-sha1" })
	})
	// within the interval the tokens still answer
	if s, _ := subjectOf(e, bearer(reader.Text)); s == nil {
		t.Fatal("refused before the reload")
	}
	e.clock.Add(auth.ReloadInterval)
	for name, tok := range map[string]model.IssuedToken{"disabled": reader, "removed": legacy, "locked": off} {
		if s, err := subjectOf(e, bearer(tok.Text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
			t.Errorf("token of a %s user: %+v %v", name, s, err)
		}
	}
	// the token of a local user that is gone is revoked at its first use, so
	// a user of the same name added later does not inherit it; the tokens of
	// a disabled user stay
	if list, err := store.Tokens("demo-legacy-sha1", auth.SourceLocal); err != nil || len(list) != 0 {
		t.Errorf("tokens of the removed user: %+v %v", list, err)
	}
	if list, err := store.Tokens("demo-reader", auth.SourceLocal); err != nil || len(list) != 1 {
		t.Errorf("tokens of the disabled user: %+v %v", list, err)
	}
	// npm tokens end when their realm leaves the order
	other := npmLogin(t, e, "demo-nobody", password(t, "demo-nobody"))
	rewrite(t, store, func(s *model.Snapshot) {
		s.Realms = slices.DeleteFunc(s.Realms, func(r string) bool { return r == auth.RealmNpm })
	})
	e.clock.Add(auth.ReloadInterval)
	if s, err := subjectOf(e, bearer(other.Text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("npm token after its realm left the order: %+v %v", s, err)
	}
}

// A token in the shape of the old product, a known prefix and one part, gets
// an answer of its own that says tokens are issued new (decision N9).
func TestOldTokenShape(t *testing.T) {
	e, _ := newStoreEnv(t, tokenSnapshot(t))
	for _, old := range []string{"NpmToken." + "4b9c2a6e-0000-4000" + "-8000-00000000c0de", "NpmToken.0123", "DockerToken." + "4b9c2a6e-0000-4000" + "-8000-00000000c0de"} {
		s, err := subjectOf(e, bearer(old))
		var me *model.Error
		if s != nil || !errors.Is(err, model.ErrOldToken) || !errors.As(err, &me) || me.Status != http.StatusUnauthorized ||
			!strings.Contains(me.Message, "issues tokens new") || strings.Contains(me.Message, old) {
			t.Errorf("old shape %q: %+v %v", old, s, err)
		}
	}
	for _, damaged := range []string{"", "NpmToken", "NpmToken.", "Other.4b9c", "4b9c2a6e-0000-4000" + "-8000-00000000c0de", "NpmToken.a.b"} {
		if s, err := subjectOf(e, bearer(damaged)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
			t.Errorf("damaged %q: %+v %v", damaged, s, err)
		}
	}
}

func TestNpmLoginRefusals(t *testing.T) {
	e, store := newStoreEnv(t, tokenSnapshot(t))
	login := func(user, pass, address string) error {
		r := request(http.MethodPut, "/repository/demo-npm/-/user/org.couchdb.user:"+user, nil)
		r.RemoteAddr = address + ":40000"
		_, err := e.svc.NpmLogin(r, user, pass)
		return err
	}
	for _, c := range []struct{ user, pass string }{{"demo-reader", "demo-wrong-pass-1"}, {"demo-no-such-user", "demo-wrong-pass-1"},
		{"demo-off", password(t, "demo-off")}, {"demo-change", password(t, "demo-change")}, {noHashUser, ""}} {
		if err := login(c.user, c.pass, "192.0.2.70"); err != model.ErrUnauthorized {
			t.Errorf("%s: %v", c.user, err)
		}
	}
	for range auth.FreeFailures {
		_ = login("demo-admin", "demo-wrong-pass-1", "192.0.2.71")
	}
	var me *model.Error
	if err := login("demo-admin", password(t, "demo-admin"), "192.0.2.71"); !errors.As(err, &me) || me.Status != http.StatusTooManyRequests ||
		me.RetryAfter < 1 {
		t.Errorf("throttled pair: %v", err)
	}
	if list, err := store.Tokens("demo-admin", auth.SourceLocal); err != nil || len(list) != 0 {
		t.Fatalf("a refused login issued a token: %+v %v", list, err)
	}
	// without the npm token realm a good pair gets no token
	rewrite(t, store, func(s *model.Snapshot) {
		s.Realms = slices.DeleteFunc(s.Realms, func(r string) bool { return r == auth.RealmNpm })
	})
	e.clock.Add(auth.ReloadInterval)
	if err := login("demo-reader", password(t, "demo-reader"), "192.0.2.72"); !errors.As(err, &me) || me.Status != http.StatusForbidden ||
		!strings.Contains(me.Message, auth.RealmNpm) {
		t.Errorf("npm realm off: %v", err)
	}
	if list, err := store.Tokens("demo-reader", auth.SourceLocal); err != nil || len(list) != 0 {
		t.Fatalf("a login without the realm issued a token: %+v %v", list, err)
	}
}

// Docker bearer tokens live in memory for 300 s, with a cap (decision N11).
func TestDockerTokens(t *testing.T) {
	snap := tokenSnapshot(t)
	e, store := newStoreEnv(t, snap)
	issue := func(user, pass string) (model.IssuedToken, error) {
		return e.svc.DockerToken(basic(request(http.MethodGet, "/v2/token?scope=repository:app:pull", nil), user, pass), "demo-docker")
	}
	tok, err := issue("demo-reader", password(t, "demo-reader"))
	if err != nil || !dockerToken.MatchString(tok.Text) || tok.ExpiresIn != 300 || !tok.IssuedAt.Equal(e.clock.Now()) {
		t.Fatalf("Docker token: %d characters, expires in %d, %v", len(tok.Text), tok.ExpiresIn, err)
	}
	if s, err := subjectOf(e, bearer(tok.Text)); err != nil || s == nil || s.User.ID != "demo-reader" ||
		!e.svc.Permits(s, "nexus:repository-view:maven2:demo-maven:read") {
		t.Fatalf("subject of the Docker token: %+v %v", s, err)
	}
	if list, err := store.Tokens("demo-reader", auth.SourceLocal); err != nil || len(list) != 0 {
		t.Fatalf("a Docker token is a row: %+v %v", list, err)
	}
	public, _ := halves(t, tok.Text)
	if s, err := subjectOf(e, bearer("DockerToken."+public+"."+strings.Repeat("A", 43))); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("Docker token with another secret: %+v %v", s, err)
	}
	e.clock.Add(auth.DockerTokenTime - time.Second)
	if s, err := subjectOf(e, bearer(tok.Text)); err != nil || s == nil {
		t.Fatalf("after 299 s: %v", err)
	}
	e.clock.Add(time.Second)
	if s, err := subjectOf(e, bearer(tok.Text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("after 300 s: %+v %v", s, err)
	}
	// a wrong pair gets none, and neither does any pair without the realm
	if _, err := issue("demo-reader", "demo-wrong-pass-1"); err != model.ErrUnauthorized {
		t.Fatalf("wrong pair: %v", err)
	}
	if _, err := e.svc.DockerToken(bearer(tok.Text), "demo-docker"); err != model.ErrUnauthorized {
		t.Fatalf("a bearer token as the credential of the token route: %v", err)
	}
	live, err := issue("demo-admin", password(t, "demo-admin"))
	if err != nil {
		t.Fatal(err)
	}
	rewrite(t, store, func(s *model.Snapshot) {
		s.Realms = slices.DeleteFunc(s.Realms, func(r string) bool { return r == auth.RealmDocker })
	})
	e.clock.Add(auth.ReloadInterval)
	var me *model.Error
	if _, err := issue("demo-admin", password(t, "demo-admin")); !errors.As(err, &me) || me.Status != http.StatusForbidden ||
		!strings.Contains(me.Message, auth.RealmDocker) {
		t.Fatalf("Docker realm off: %v", err)
	}
	if s, err := subjectOf(e, bearer(live.Text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("a live Docker token after its realm left the order: %+v %v", s, err)
	}
}

func TestDockerTokenCap(t *testing.T) {
	e, _ := newStoreEnv(t, tokenSnapshot(t))
	if auth.DockerTokenLimit(e.svc) != auth.MaxDockerTokens || auth.MaxDockerTokens != 10_000 {
		t.Fatalf("bound %d", auth.DockerTokenLimit(e.svc))
	}
	auth.SetMaxDockerTokens(e.svc, 50)
	r := basic(request(http.MethodGet, "/v2/token", nil), "demo-reader", password(t, "demo-reader"))
	var texts []string
	for range 80 {
		tok, err := e.svc.DockerToken(r, "demo-docker")
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, tok.Text)
		e.clock.Add(time.Millisecond)
	}
	if n := auth.DockerTokenCount(e.svc); n != 50 {
		t.Fatalf("%d Docker tokens past the bound of 50", n)
	}
	if s, _ := subjectOf(e, bearer(texts[0])); s != nil {
		t.Fatal("the oldest token stayed past the bound")
	}
	if s, err := subjectOf(e, bearer(texts[79])); err != nil || s == nil {
		t.Fatalf("the newest token: %v", err)
	}
	// expired tokens go first
	e.clock.Add(auth.DockerTokenTime)
	if _, err := e.svc.DockerToken(r, "demo-docker"); err != nil {
		t.Fatal(err)
	}
	if n := auth.DockerTokenCount(e.svc); n != 1 {
		t.Fatalf("%d Docker tokens after the others expired", n)
	}
}

// No secret half in a log line, an error text or an answer but the one
// that issues it.
func TestNoTokenSecretInOutput(t *testing.T) {
	e, _ := newStoreEnv(t, tokenSnapshot(t))
	tok := npmLogin(t, e, "demo-reader", password(t, "demo-reader"))
	docker, err := e.svc.DockerToken(basic(request(http.MethodGet, "/v2/token", nil), "demo-reader", password(t, "demo-reader")), "demo-docker")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, text := range []string{tok.Text, docker.Text} {
		public, secret := halves(t, text)
		for _, sent := range []string{text, "NpmToken." + public + "." + secret[:40] + "xyz", "DockerToken." + public + ".A" + secret[1:], text + "x"} {
			s, err := subjectOf(e, bearer(sent))
			fmt.Fprintf(&out, "%v %+v %#v %v\n", s, s, s, err)
		}
		s, _ := subjectOf(e, bearer(text))
		fmt.Fprintf(&out, "%+v %v\n", s, e.svc.RevokeToken(s, text+"x"))
		list, err := e.svc.Tokens(s.User.ID, s.User.Source)
		fmt.Fprintf(&out, "%v %+v %#v %v\n", list, list, list, err)
	}
	out.WriteString(e.logs.String())
	for _, text := range []string{tok.Text, docker.Text} {
		_, secret := halves(t, text)
		if strings.Contains(out.String(), secret) || strings.Contains(out.String(), secret[:40]) {
			t.Errorf("the output holds the secret half of a token")
		}
	}
	if !strings.Contains(out.String(), "demo-reader") {
		t.Fatalf("the output was not captured: %q", out.String())
	}
}

// Hardening line 6 of the phase 3 plan: random values come from crypto/rand
// only, in the three packages of identity.
func TestRandomOnlyFromCryptoRand(t *testing.T) {
	for _, dir := range []string{".", "../authz", "../ldapauth"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %d files, %v", dir, len(files), err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte(`"math/rand`)) {
				t.Errorf("%s imports math/rand", file)
			}
		}
	}
	data, err := os.ReadFile("tokens.go")
	if err != nil || !bytes.Contains(data, []byte(`"crypto/rand"`)) {
		t.Fatalf("tokens.go draws no value from crypto/rand: %v", err)
	}
}

// Review N2: an anonymous Docker token takes no place in the store of Docker
// tokens, so a flood of the token route without credentials evicts no token
// of a signed in user. It is the anonymous subject until it ends.
func TestAnonymousDockerTokensTakeNoPlace(t *testing.T) {
	snap := tokenSnapshot(t)
	snap.Anonymous.Enabled = true
	snap.Repositories = append(snap.Repositories, model.Repository{Name: "demo-docker", Format: "docker", Type: "hosted", Online: true,
		Attributes: map[string]any{"docker": map[string]any{"forceBasicAuth": false}}})
	e, _ := newStoreEnv(t, snap)
	auth.SetMaxDockerTokens(e.svc, 5)
	user, err := e.svc.DockerToken(basic(request(http.MethodGet, "/v2/token", nil), "demo-reader", password(t, "demo-reader")), "demo-docker")
	if err != nil {
		t.Fatal(err)
	}
	var anonymous []model.IssuedToken
	for range 50 {
		tok, err := e.svc.DockerToken(request(http.MethodGet, "/v2/token", nil), "demo-docker")
		if err != nil || !dockerToken.MatchString(tok.Text) || tok.ExpiresIn != 300 || !tok.IssuedAt.Equal(e.clock.Now()) {
			t.Fatalf("anonymous token: %d characters, expires in %d, %v", len(tok.Text), tok.ExpiresIn, err)
		}
		anonymous = append(anonymous, tok)
		e.clock.Add(time.Millisecond)
	}
	if n := auth.DockerTokenCount(e.svc); n != 1 {
		t.Errorf("%d Docker tokens in memory after 50 anonymous ones, want the one of demo-reader", n)
	}
	if s, err := subjectOf(e, bearer(user.Text)); err != nil || s == nil || s.User.ID != "demo-reader" {
		t.Fatalf("token of demo-reader after the anonymous ones: %+v %v", s, err)
	}
	if s, err := subjectOf(e, bearer(anonymous[0].Text)); s != nil || err != nil {
		t.Errorf("anonymous token: %+v %v", s, err)
	}
	public, _ := halves(t, anonymous[0].Text)
	if s, err := subjectOf(e, bearer("DockerToken."+public+"."+strings.Repeat("A", 43))); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("anonymous token with another secret: %+v %v", s, err)
	}
	e.clock.Add(auth.DockerTokenTime)
	if s, err := subjectOf(e, bearer(anonymous[49].Text)); s != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Errorf("anonymous token after 300 s: %+v %v", s, err)
	}
}
