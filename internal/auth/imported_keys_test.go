package auth_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// Keys of the old product taken over by an import with secrets sign their
// users in (decisions K5 to K8 of docs/plans/secrets-import.md, which
// reverse N9 for imported keys only). Every key is drawn at run time.

// oldKey is a key in the shape of the old product.
func oldKey(t testing.TB) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	h := hex.EncodeToString(b)
	return strings.Join([]string{h[:8], h[8:12], h[12:16], h[16:20], h[20:]}, "-")
}

type importedEnv struct {
	*env
	store      *state.Store
	installKey []byte
}

// importEnv is a service over a state.sqlite with snap and the imported
// keys: per entry the kind, the user and its source.
func importEnv(t testing.TB, snap *model.Snapshot, keys map[string]model.ImportedKey, dirs ...auth.Directory) *importedEnv {
	t.Helper()
	st := stateOf(t, snap)
	installKey := make([]byte, 32)
	if _, err := rand.Read(installKey); err != nil {
		t.Fatal(err)
	}
	var rows []model.ImportedKey
	for key, row := range keys {
		row.Digest = model.ImportedDigest(installKey, sha256.Sum256([]byte(key)))
		row.Created = time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
		rows = append(rows, row)
	}
	if err := st.WriteImport([]state.Kind{state.KindRoles}, snap, rows); err != nil {
		t.Fatal(err)
	}
	gen, err := st.Generation()
	if err != nil {
		t.Fatal(err)
	}
	c := newClock()
	logs := &bytes.Buffer{}
	svc, err := auth.New(auth.Options{Snapshot: snap, Generation: gen, Store: st, Directories: dirs, Now: c.Now,
		Imported: st, InstallKey: func() ([]byte, error) { return installKey, nil },
		Logger: slog.New(slog.NewTextHandler(&lockedWriter{w: logs}, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return &importedEnv{env: &env{t: t, svc: svc, handler: svc.SessionHandler(), clock: c, logs: logs}, store: st,
		installKey: installKey}
}

func (e *importedEnv) row(t testing.TB, kind, key string) (model.ImportedKey, bool) {
	t.Helper()
	got, found, err := e.store.ImportedKey(kind, model.ImportedDigest(e.installKey, sha256.Sum256([]byte(key))))
	if err != nil {
		t.Fatal(err)
	}
	return got, found
}

func local(kind, user string) model.ImportedKey {
	return model.ImportedKey{Kind: kind, UserID: user, Source: auth.SourceLocal}
}

func TestImportedKeysSignTheirUsersIn(t *testing.T) {
	npmKey, dockerKey, ldapKey := oldKey(t), oldKey(t), oldKey(t)
	e := importEnv(t, tokenSnapshot(t), map[string]model.ImportedKey{
		npmKey: local("npm", "demo-reader"), dockerKey: local("docker", "demo-reader"),
		ldapKey: {Kind: "npm", UserID: "demo-ldap-person", Source: auth.SourceLDAP},
	}, ldapDirectory())
	for _, c := range []struct{ name, header, user, source string }{
		{"npm", "NpmToken." + npmKey, "demo-reader", auth.SourceLocal},
		{"docker", "DockerToken." + dockerKey, "demo-reader", auth.SourceLocal},
		{"npm of a directory user", "NpmToken." + ldapKey, "demo-ldap-person", auth.SourceLDAP},
	} {
		s, err := subjectOf(e.env, bearer(c.header))
		if err != nil || s == nil || s.User.ID != c.user || s.User.Source != c.source {
			t.Fatalf("%s: %+v %v", c.name, s, err)
		}
		if c.source == auth.SourceLDAP && !slices.Equal(s.Permissions, personRights) {
			t.Fatalf("%s: rights %v, want %v", c.name, s.Permissions, personRights)
		}
		if c.user == "demo-reader" && !slices.Contains(s.Permissions, "nexus:repository-view:maven2:demo-maven:read") {
			t.Fatalf("%s: rights %v", c.name, s.Permissions)
		}
	}
	// the last use is written, at most once per auth.TouchInterval
	first, _ := e.row(t, "npm", npmKey)
	if !first.LastUsed.Equal(e.clock.Now()) {
		t.Fatalf("last use %v, want %v", first.LastUsed, e.clock.Now())
	}
	e.clock.Add(time.Second)
	if _, err := subjectOf(e.env, bearer("NpmToken."+npmKey)); err != nil {
		t.Fatal(err)
	}
	if again, _ := e.row(t, "npm", npmKey); !again.LastUsed.Equal(first.LastUsed) {
		t.Fatal("the last use was written again within the interval")
	}
}

// Refusals: a key never imported, a key of another kind, the realm of the
// kind off, a user that cannot sign in. A key that matches nothing keeps the
// answer of N9.
func TestImportedKeysAreRefused(t *testing.T) {
	npmKey, offKey := oldKey(t), oldKey(t)
	snap := tokenSnapshot(t)
	e := importEnv(t, snap, map[string]model.ImportedKey{npmKey: local("npm", "demo-reader"), offKey: local("npm", "demo-off")})
	for name, header := range map[string]string{
		"never imported": "NpmToken." + oldKey(t),
		"another kind":   "DockerToken." + npmKey,
		"one sign wrong": "NpmToken." + npmKey[:len(npmKey)-1] + "x",
		"a user token":   "UserToken." + npmKey,
	} {
		if s, err := subjectOf(e.env, bearer(header)); s != nil || !errors.Is(err, model.ErrOldToken) {
			t.Errorf("%s: %+v %v, want the answer of N9", name, s, err)
		}
	}
	// a key that matches answers as one that does not (review secrets-3)
	if s, err := subjectOf(e.env, bearer("NpmToken."+offKey)); s != nil || !errors.Is(err, model.ErrOldToken) {
		t.Errorf("a disabled user: %+v %v", s, err)
	}
	if err := e.store.Write([]state.Kind{state.KindRealms}, func() *model.Snapshot {
		s := tokenSnapshot(t)
		s.Realms = slices.DeleteFunc(s.Realms, func(r string) bool { return r == auth.RealmNpm })
		return s
	}()); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(auth.ReloadInterval)
	if s, err := subjectOf(e.env, bearer("NpmToken."+npmKey)); s != nil || !errors.Is(err, model.ErrOldToken) {
		t.Errorf("the realm of npm tokens off: %+v %v", s, err)
	}
}

// Without an install key no imported key signs in.
func TestImportedKeysNeedTheInstallKey(t *testing.T) {
	key := oldKey(t)
	snap := tokenSnapshot(t)
	st := stateOf(t, snap)
	installKey := make([]byte, 32)
	if err := st.WriteImport([]state.Kind{state.KindRoles}, snap, []model.ImportedKey{{Kind: "npm", UserID: "demo-reader",
		Source: auth.SourceLocal, Digest: model.ImportedDigest(installKey, sha256.Sum256([]byte(key)))}}); err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func() ([]byte, error){
		"no key":     func() ([]byte, error) { return nil, nil },
		"read fails": func() ([]byte, error) { return nil, errors.New("demo: unreadable") },
		"other key":  func() ([]byte, error) { return bytes.Repeat([]byte{1}, 32), nil },
	} {
		svc, err := auth.New(auth.Options{Snapshot: snap, Store: st, Imported: st, InstallKey: read})
		if err != nil {
			t.Fatal(err)
		}
		if s, err := svc.Subject(bearer("NpmToken." + key)); s != nil || !errors.Is(err, model.ErrOldToken) {
			t.Errorf("%s: %+v %v", name, s, err)
		}
	}
	svc, err := auth.New(auth.Options{Snapshot: snap, Store: st, Imported: st,
		InstallKey: func() ([]byte, error) { return installKey, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := svc.Subject(bearer("NpmToken." + key)); err != nil || s == nil {
		t.Fatalf("the right install key: %+v %v", s, err)
	}
}

// The key of a local user that is gone is deleted at its first use (K8).
func TestTheImportedKeyOfAUserThatIsGoneIsDeleted(t *testing.T) {
	key := oldKey(t)
	e := importEnv(t, tokenSnapshot(t), map[string]model.ImportedKey{key: local("npm", "demo-gone")})
	if s, err := subjectOf(e.env, bearer("NpmToken."+key)); s != nil || !errors.Is(err, model.ErrOldToken) {
		t.Fatalf("%+v %v", s, err)
	}
	if _, found := e.row(t, "npm", key); found {
		t.Fatal("the key of a user that is gone is still there")
	}
}

// npm logout with an imported key deletes it; the key of another user is
// not the subject's to delete.
func TestNpmLogoutDeletesAnImportedKey(t *testing.T) {
	mine, theirs := oldKey(t), oldKey(t)
	e := importEnv(t, tokenSnapshot(t), map[string]model.ImportedKey{mine: local("npm", "demo-reader"),
		theirs: local("npm", "demo-nobody")})
	s, err := subjectOf(e.env, bearer("NpmToken."+mine))
	if err != nil || s == nil {
		t.Fatal(err)
	}
	if err := e.svc.RevokeToken(s, "NpmToken."+theirs); !errors.Is(err, model.ErrNoToken) {
		t.Fatalf("the key of another user: %v", err)
	}
	if err := e.svc.RevokeToken(s, "NpmToken."+mine); err != nil {
		t.Fatal(err)
	}
	if _, found := e.row(t, "npm", mine); found {
		t.Fatal("npm logout left the imported key")
	}
	if _, found := e.row(t, "npm", theirs); !found {
		t.Fatal("npm logout deleted the key of another user")
	}
	e.clock.Add(auth.ReloadInterval)
	if s, err := subjectOf(e.env, bearer("NpmToken."+mine)); s != nil || !errors.Is(err, model.ErrOldToken) {
		t.Fatalf("after the logout: %+v %v", s, err)
	}
}
