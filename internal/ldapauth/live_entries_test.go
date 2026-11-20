package ldapauth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// Review 3-40 against OpenLDAP: the entry of a user is named by its
// entryUUID, and the npm token of a person removed from the directory signs
// nobody in once a new entry of the same name is there.
func TestLiveTokenFollowsNoNameToANewEntry(t *testing.T) {
	s := liveServer(t)
	s.UserBaseDN, s.GroupBaseDN = "ou=temporary", "ou=temporary"
	d := liveDirectory(t, s)
	w := writer(t)
	person := "uid=" + tempUser + "," + tempBase
	remove := func() {
		if err := w.Del(ldap.NewDelRequest(person, nil)); err != nil && !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			t.Errorf("remove %s: %v", person, err)
		}
	}
	add := func() {
		t.Helper()
		a := ldap.NewAddRequest(person, nil)
		a.Attribute("objectClass", []string{"inetOrgPerson"})
		a.Attribute("uid", []string{tempUser})
		a.Attribute("cn", []string{"Demo Ldap Temp"})
		a.Attribute("sn", []string{"Temp"})
		a.Attribute("userPassword", []string{tempPassword})
		if err := w.Add(a); err != nil {
			t.Fatal(err)
		}
	}
	remove()
	add()
	t.Cleanup(remove)

	snap := &model.Snapshot{
		Realms:    []string{auth.RealmLDAP, auth.RealmNpm},
		Anonymous: model.Anonymous{UserID: "anonymous", RealmName: "NexusAuthorizingRealm"},
	}
	st, err := state.Create(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Write(state.Kinds, snap); err != nil {
		t.Fatal(err)
	}
	gen, err := st.Generation()
	if err != nil {
		t.Fatal(err)
	}
	c := &liveClock{t: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	svc, err := auth.New(auth.Options{Snapshot: snap, Generation: gen, Store: st, Directories: []auth.Directory{asAuth{d}}, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := d.Lookup(t.Context(), tempUser)
	if err != nil || !strings.HasPrefix(first.Entry, "entryUUID:") {
		t.Fatalf("entry of the person: %q %v", first.Entry, err)
	}
	login := httptest.NewRequest(http.MethodPut, "/repository/demo-npm/-/user/org.couchdb.user:"+tempUser, nil)
	tok, err := svc.NpmLogin(login, tempUser, tempPassword)
	if err != nil {
		t.Fatal(err)
	}
	c.Add(auth.ReloadInterval)
	use := func() (*model.Subject, error) {
		r := httptest.NewRequest(http.MethodGet, "/repository/demo-npm/", nil)
		r.Header.Set("Authorization", "Bearer "+tok.Text)
		return svc.Subject(r)
	}
	if sub, err := use(); err != nil || sub == nil || sub.User.ID != tempUser {
		t.Fatalf("the npm token of its entry: %+v %v", sub, err)
	}

	remove()
	add()
	second, _, err := d.Lookup(t.Context(), tempUser)
	if err != nil || second.Entry == first.Entry || !strings.HasPrefix(second.Entry, "entryUUID:") {
		t.Fatalf("entry of the new person: %q, the old one %q, %v", second.Entry, first.Entry, err)
	}
	c.Add(auth.CredentialTime)
	if sub, err := use(); sub != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("the npm token of the removed person, a new entry of its name there: %+v %v", sub, err)
	}
}
