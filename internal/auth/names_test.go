package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Review S5: the outer white space of a user name is cut on every credential
// path before the name keys the limiter and before it reaches the local
// users or a directory, so "alice", "alice " and " alice\t" are one name
// and one pair.
func TestOuterSpacesOfAUserName(t *testing.T) {
	d := ldapDirectory()
	e, _ := newStoreEnv(t, tokenSnapshot(t), d)
	reader, person := password(t, "demo-reader"), ldapPassword
	at := func(r *http.Request, address string) *http.Request {
		r.RemoteAddr = address + ":40000"
		return r
	}
	for _, c := range []struct{ name, pass, want string }{
		{"demo-reader ", reader, "demo-reader"},
		{" demo-reader\t", reader, "demo-reader"},
		{"demo-ldap-person  ", person, "demo-ldap-person"},
	} {
		if rec := e.signIn(c.name, c.pass, "192.0.2.90"); rec.Code != http.StatusOK {
			t.Errorf("sign in of %q: %d", c.name, rec.Code)
		}
		if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), c.name, c.pass)); err != nil || s == nil || s.User.ID != c.want {
			t.Errorf("Basic of %q: %+v %v", c.name, s, err)
		}
		if _, err := e.svc.DockerToken(basic(request(http.MethodGet, "/v2/token", nil), c.name, c.pass), "demo-docker"); err != nil {
			t.Errorf("Docker token of %q: %v", c.name, err)
		}
		tok, err := e.svc.NpmLogin(request(http.MethodPut, "/repository/demo-npm/-/user/org.couchdb.user:x", nil), c.name, c.pass)
		if err != nil {
			t.Fatalf("npm login of %q: %v", c.name, err)
		}
		if s, err := subjectOf(e, bearer(tok.Text)); err != nil || s == nil || s.User.ID != c.want {
			t.Errorf("npm token of %q: %+v %v", c.name, s, err)
		}
	}
	// the spellings share one pair of the limiter, whatever the path
	const address = "192.0.2.91"
	spellings := []string{"demo-admin", "demo-admin ", " demo-admin", "demo-admin\t", "  demo-admin  "}
	for i, name := range spellings {
		var err error
		switch i % 3 {
		case 0:
			_, err = subjectOf(e, at(basic(request(http.MethodGet, "/", nil), name, "demo-wrong-pass-1"), address))
		case 1:
			if rec := e.signIn(name, "demo-wrong-pass-1", address); rec.Code != http.StatusUnauthorized {
				t.Fatalf("failure %d of %q: %d", i+1, name, rec.Code)
			}
			continue
		case 2:
			_, err = e.svc.NpmLogin(at(request(http.MethodPut, "/", nil), address), name, "demo-wrong-pass-1")
		}
		if !errors.Is(err, model.ErrUnauthorized) {
			t.Fatalf("failure %d of %q: %v", i+1, name, err)
		}
	}
	if len(spellings) != auth.FreeFailures {
		t.Fatalf("%d spellings, want %d", len(spellings), auth.FreeFailures)
	}
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), address); rec.Code != http.StatusTooManyRequests {
		t.Errorf("right password after %d failures of the spellings: %d, want 429", len(spellings), rec.Code)
	}
}

// Review R6 of the second security review: every path cuts the outer white
// space of a typed name (review S5), so a local user whose stored id has
// outer white space, as an import can bring it, signs in with the name
// without it and gets its stored id: through the interface, Basic, npm
// login, its npm token and a user token as its password. An id that is that
// name exactly comes first, and two stored ids of one such name sign in by
// neither.
func TestStoredIDWithOuterSpaces(t *testing.T) {
	v := vectorNamed(t, "shiro1-sha512")
	snap := tokenSnapshot(t)
	add := func(id string) {
		snap.Users = append(snap.Users, model.User{ID: id, Status: "active", Source: auth.SourceLocal,
			Roles: []string{"demo-maven-reader"}, PasswordHash: v.Hash})
	}
	const spaced = " demo-spaced\t"
	add(spaced)
	add(" demo-twin")
	add("demo-twin ")
	add(" demo-admin")
	e, store := newStoreEnv(t, snap)
	if rec := e.signIn("demo-spaced", v.Password, "192.0.2.95"); rec.Code != http.StatusOK {
		t.Fatalf("sign in of the stored id %q: %d", spaced, rec.Code)
	} else if got := decodeSession(t, rec).User.UserID; got != spaced {
		t.Errorf("session of %q", got)
	}
	if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-spaced", v.Password)); err != nil || s == nil || s.User.ID != spaced {
		t.Errorf("Basic: %+v %v", s, err)
	}
	npm := npmLogin(t, e, "demo-spaced", v.Password).Text
	text, row, err := auth.NewToken(auth.KindUser, spaced, auth.SourceLocal, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddToken(row, auth.MaxTokensPerKind); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(auth.ReloadInterval)
	if s, err := subjectOf(e, bearer(npm)); err != nil || s == nil || s.User.ID != spaced {
		t.Errorf("npm token: %+v %v", s, err)
	}
	if s, err := subjectOf(e, basic(request(http.MethodGet, "/", nil), "demo-spaced", text)); err != nil || s == nil || s.User.ID != spaced {
		t.Errorf("user token as the password: %+v %v", s, err)
	}
	if rec := e.signIn("demo-twin", v.Password, "192.0.2.96"); rec.Code != http.StatusUnauthorized {
		t.Errorf("a name of two stored ids: %d, want 401", rec.Code)
	}
	if rec := e.signIn("demo-admin", password(t, "demo-admin"), "192.0.2.96"); rec.Code != http.StatusOK ||
		decodeSession(t, rec).User.UserID != "demo-admin" {
		t.Errorf("the id that is the name exactly: %d %s", rec.Code, rec.Body)
	}
}

// foldingDirectory takes the spellings of a name that differ by soft hyphens,
// runs of inner spaces and letter case as one account, as a directory that
// prepares strings by RFC 4518 does, and records every name it is asked.
type foldingDirectory struct {
	mu             sync.Mutex
	name, password string
	asked          []string
}

func (d *foldingDirectory) Login(_ context.Context, username, password string) (model.User, []string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked = append(d.asked, username)
	alike := strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(username, "\u00ad", "")), " "))
	if alike == d.name && password == d.password {
		return model.User{ID: d.name, Source: auth.SourceLDAP, Status: "active"}, nil, nil
	}
	return model.User{}, nil, fmt.Errorf("demo folding directory: %w", auth.ErrWrongPair)
}

func (d *foldingDirectory) Lookup(context.Context, string) (model.User, []string, error) {
	return model.User{}, nil, fmt.Errorf("demo folding directory: %w", auth.ErrNoUser)
}

// Finding A1 of the security assessment and note R5 of the second security
// review: spellings that a directory takes as one account are one pair of
// the limiter, so ten of them from one address bring five guesses at its
// password, not ten. The name the directory gets stays as typed, without its
// outer white space only.
func TestSpellingsOfOneAccountShareAPair(t *testing.T) {
	d := &foldingDirectory{name: "alice smith", password: "demo-ldap-pass-3"}
	e := newEnv(t, fixture(t), d)
	spellings := []string{"alice smith", "alice  smith", "alice   smith", "ali\u00adce smith", "alice smi\u00adth",
		"al\u00adice  smith", "alice    smith", "a\u00adlice smith", "alice s\u00admith", "alice     smith"}
	var codes []int
	for i, name := range spellings {
		codes = append(codes, e.signIn(name, fmt.Sprintf("demo-guess-%d", i), "192.0.2.92").Code)
	}
	want := []int{401, 401, 401, 401, 401, 429, 429, 429, 429, 429}
	if !slices.Equal(codes, want) {
		t.Errorf("answers %v, want %v", codes, want)
	}
	if !slices.Equal(d.asked, spellings[:auth.FreeFailures]) {
		t.Errorf("the directory was asked %q, want the first %d spellings as typed", d.asked, auth.FreeFailures)
	}
}

// The limiter key folds a user name as RFC 4518 prepares a string for a
// match (read on 2026-11-19 at rfc-editor.org): the code points section 2.2
// maps to nothing are removed, those it maps to SPACE are spaces, section
// 2.3 normalizes to Unicode form KC, section 2.6.1 makes runs of spaces
// insignificant, and the name counts in lower case. The lists below are the
// lists of section 2.2. The RFC writes the variation selectors as FF00-FE0F,
// a range that ends before it starts; they are FE00-FE0F.
func TestLimiterKeyFoldsAsRFC4518(t *testing.T) {
	nothing := [][2]rune{
		{0x00AD, 0x00AD}, {0x1806, 0x1806}, {0x034F, 0x034F}, {0x180B, 0x180D}, {0xFE00, 0xFE0F}, {0xFFFC, 0xFFFC},
		{0x0000, 0x0008}, {0x000E, 0x001F}, {0x007F, 0x0084}, {0x0086, 0x009F}, {0x06DD, 0x06DD}, {0x070F, 0x070F},
		{0x180E, 0x180E}, {0x200C, 0x200F}, {0x202A, 0x202E}, {0x2060, 0x2063}, {0x206A, 0x206F}, {0xFEFF, 0xFEFF},
		{0xFFF9, 0xFFFB}, {0x1D173, 0x1D17A}, {0xE0001, 0xE0001}, {0xE0020, 0xE007F}, {0x200B, 0x200B},
	}
	space := [][2]rune{
		{0x0009, 0x000D}, {0x0085, 0x0085}, {0x0020, 0x0020}, {0x00A0, 0x00A0}, {0x1680, 0x1680}, {0x2000, 0x200A},
		{0x2028, 0x2029}, {0x202F, 0x202F}, {0x205F, 0x205F}, {0x3000, 0x3000},
	}
	for _, span := range nothing {
		for r := span[0]; r <= span[1]; r++ {
			if !auth.SamePair("ali"+string(r)+"ce", "alice") {
				t.Errorf("U+%04X is not mapped to nothing", r)
			}
		}
	}
	for _, span := range space {
		for r := span[0]; r <= span[1]; r++ {
			if !auth.SamePair("alice"+string(r)+"smith", "alice smith") || !auth.SamePair(string(r)+"alice"+string(r), "alice") {
				t.Errorf("U+%04X is not a space", r)
			}
		}
	}
	for _, c := range [][2]string{
		{"\uff41\uff4c\uff49\uff43\uff45", "alice"},          // full width letters
		{"\ufb01ona", "fiona"},                               // the ligature fi
		{"user\u2168", "userix"},                             // ROMAN NUMERAL NINE
		{"ALICE  SMITH", "alice smith"},                      // case and a run of spaces
		{" \t alice \u3000\u00a0 smith \r\n", "alice smith"}, // spaces of every kind, outer and inner
		{"e\u0301", "\u00e9"},                                // a combining accent composes
	} {
		if !auth.SamePair(c[0], c[1]) {
			t.Errorf("%q and %q are two pairs", c[0], c[1])
		}
	}
	for _, c := range [][2]string{{"alice", "alicia"}, {"alice smith", "alicesmith"}, {"alice", "alice-"}} {
		if auth.SamePair(c[0], c[1]) {
			t.Errorf("%q and %q are one pair", c[0], c[1])
		}
	}
}

// Row 3-51 of review 3: after the steps of RFC 4518 the limiter key takes
// full case folding (golang.org/x/text/cases.Fold), so sharp s and ss, the
// capital sharp s, and final sigma and sigma are one pair. Diacritics stay
// apart: a directory that ignores them matches looser than this key.
func TestLimiterKeyFoldsSharpSAndFinalSigma(t *testing.T) {
	for _, c := range [][2]string{
		{"straße", "strasse"}, // sharp s
		{"STRAẞE", "strasse"}, // capital sharp s
		{"ßmith", "SSMITH"},   // sharp s at the start, upper case ss
		{"οδος", "ΟΔΟΣ"},      // final sigma and capital sigma
		{"οδος", "οδοσ"},      // final sigma and sigma
	} {
		if !auth.SamePair(c[0], c[1]) {
			t.Errorf("%q and %q are two pairs", c[0], c[1])
		}
	}
	for _, c := range [][2]string{{"straße", "strase"}, {"émile", "emile"}} {
		if auth.SamePair(c[0], c[1]) {
			t.Errorf("%q and %q are one pair", c[0], c[1])
		}
	}
}
