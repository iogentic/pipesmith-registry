package rest

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// ldapStore is admin.IdentityStore over a snapshot in memory with a data
// directory for the secret files. fail makes the next Update fail after
// its function ran, as a write of the state that fails.
type ldapStore struct {
	snap          *model.Snapshot
	dir           string
	writes, reset int
	fail          bool
}

func (f *ldapStore) Load() (*model.Snapshot, error) { return copyLDAP(f.snap), nil }
func (f *ldapStore) DataDir() string                { return f.dir }
func (f *ldapStore) ResetCredentials()              { f.reset++ }
func (f *ldapStore) MarkStale()                     {}
func (f *ldapStore) Update(_ []state.Kind, fn func(*model.Snapshot) error) error {
	s := copyLDAP(f.snap)
	if err := fn(s); err != nil {
		return err
	}
	if f.fail {
		f.fail = false
		return errors.New("the write of the state failed")
	}
	f.snap, f.writes = s, f.writes+1
	return nil
}

func copyLDAP(s *model.Snapshot) *model.Snapshot {
	c := copyIdentity(s)
	c.LDAP = slices.Clone(s.LDAP)
	return c
}

const (
	bindOne = "example-bind-1"
	bindTwo = "example-bind-2"
)

func ldapFixture(t *testing.T) *ldapStore {
	t.Helper()
	store := identityFixture()
	store.snap.LDAP = []model.LDAPServer{{ID: "conn-1", Name: "corp", Order: 1, Protocol: "ldap", Host: "ldap.example.com",
		Port: 389, SearchBase: "dc=example,dc=com", AuthScheme: "SIMPLE", AuthUsername: "uid=demo-bind", ConnectionTimeoutSeconds: 30,
		ConnectionRetryDelaySeconds: 300, MaxIncidentsCount: 3, UserIDAttribute: "uid", GroupType: "static", CAFile: "/srv/ca.pem"}}
	t.Setenv(ldapauth.PasswordVariable("conn-1"), "")
	return &ldapStore{snap: store.snap, dir: filepath.Join(t.TempDir(), "data")}
}

// ldapBody is a form of the contract shape for the connection name.
func ldapBody(name, host, password string) []byte {
	body := `{"name":"` + name + `","protocol":"ldap","useTrustStore":false,"host":"` + host + `","port":389,` +
		`"searchBase":"dc=example,dc=com","authScheme":"SIMPLE","authRealm":"","authUsername":"uid=demo-bind",` +
		`"connectionTimeoutSeconds":30,"connectionRetryDelaySeconds":300,"maxIncidentsCount":3,"userBaseDn":"",` +
		`"userSubtree":false,"userObjectClass":"","userLdapFilter":"","userIdAttribute":"uid","userRealNameAttribute":"",` +
		`"userEmailAddressAttribute":"","userPasswordAttribute":"","ldapGroupsAsRoles":false,"groupType":"static",` +
		`"groupBaseDn":"","groupSubtree":false,"groupObjectClass":"","groupIdAttribute":"","groupMemberAttribute":"",` +
		`"groupMemberFormat":"","userMemberOfAttribute":""`
	if password != "-" {
		body += `,"authPassword":"` + password + `"`
	}
	return []byte(body + "}")
}

// ldapAs is the write of a subject with the permissions over the store.
func ldapAs(store *ldapStore, permissions ...string) (*Env, IdentityWrite) {
	env, _ := as(&identityStore{snap: store.snap}, "admin", permissions...)
	return env, IdentityWrite{Store: store, Subject: &model.Subject{User: model.User{ID: "admin", Source: "default"}}}
}

// noBind fails for an answer that holds a bind password of the tests.
func noBind(t *testing.T, label string, a Answer) {
	t.Helper()
	data, _ := a.JSON()
	if strings.Contains(string(data), bindOne) || strings.Contains(string(data), bindTwo) || len(admin.Leaks(a.Body, "")) > 0 {
		t.Fatalf("%s: a bind password in %s", label, data)
	}
}

func secretOf(t *testing.T, store *ldapStore, id string) (string, os.FileMode) {
	t.Helper()
	path := state.LDAPSecretPath(store.dir, id)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", 0
	}
	data, err := state.ReadSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), info.Mode().Perm()
}

func TestLDAPWrites(t *testing.T) {
	store := ldapFixture(t)
	env, w := ldapAs(store, "nexus:*")

	a, err := CreateLDAP(env, w, ldapBody("second", "ldap2.example.com", bindOne))
	answered(t, "create", a, err, 201)
	noBind(t, "create", a)
	if a.Body != nil || store.writes != 1 || store.reset != 1 || len(store.snap.LDAP) != 2 {
		t.Fatalf("after the create: %+v, %d writes, %d resets", a, store.writes, store.reset)
	}
	created := store.snap.LDAP[1]
	if created.ID == "" || created.ID == "conn-1" || created.Order != 2 || created.CAFile != "" || created.Host != "ldap2.example.com" {
		t.Fatalf("created %+v", created)
	}
	if got, mode := secretOf(t, store, created.ID); got != bindOne || mode != 0o600 {
		t.Fatalf("the secret file holds %q with mode %o", got, mode)
	}

	// a save with a new name and no password keeps the secret, the id and the order (R2b, I18)
	a, err = SaveLDAP(env, w, "second", ldapBody("second-renamed", "ldap2.example.com", "-"))
	answered(t, "save renamed", a, err, 204)
	saved := store.snap.LDAP[1]
	if saved.Name != "second-renamed" || saved.ID != created.ID || saved.Order != 2 {
		t.Fatalf("saved %+v", saved)
	}
	if got, _ := secretOf(t, store, created.ID); got != bindOne {
		t.Fatalf("the save without a password changed the secret: %q", got)
	}
	// a save with a password replaces it; the CA file of the stored connection stays
	a, err = SaveLDAP(env, w, "corp", ldapBody("corp", "ldap.example.com", bindTwo))
	answered(t, "save with a password", a, err, 204)
	noBind(t, "save with a password", a)
	if got, _ := secretOf(t, store, "conn-1"); got != bindTwo || store.snap.LDAP[0].CAFile != "/srv/ca.pem" {
		t.Fatalf("after the save: %q, %+v", got, store.snap.LDAP[0])
	}

	// the order, then the same order again
	a, err = OrderLDAP(env, w, []byte(`["second-renamed","corp"]`))
	answered(t, "order", a, err, 204)
	if store.snap.LDAP[0].Order != 2 || store.snap.LDAP[1].Order != 1 {
		t.Fatalf("orders %+v", store.snap.LDAP)
	}
	a, err = OrderLDAP(env, w, []byte(`["second-renamed","corp"]`))
	answered(t, "the same order", a, err, 204)

	// the cache: one more change and one more reset, no record changed
	before := copyLDAP(store.snap)
	writes, resets := store.writes, store.reset
	a, err = ClearLDAPCache(env, w)
	answered(t, "clear cache", a, err, 204)
	if store.writes != writes+1 || store.reset != resets+1 || !slices.Equal(before.LDAP, store.snap.LDAP) {
		t.Fatalf("clear cache: %d writes, %d resets", store.writes-writes, store.reset-resets)
	}

	// the delete removes the row and the secret file
	a, err = DeleteLDAP(env, w, "second-renamed")
	answered(t, "delete", a, err, 204)
	if len(store.snap.LDAP) != 1 || store.snap.LDAP[0].ID != "conn-1" {
		t.Fatalf("after the delete: %+v", store.snap.LDAP)
	}
	if got, mode := secretOf(t, store, created.ID); got != "" || mode != 0 {
		t.Fatalf("the secret file stayed: %q", got)
	}
	a, err = DeleteLDAP(env, w, "second-renamed")
	answered(t, "delete again", a, err, 404)
}

func TestLDAPWriteRefusals(t *testing.T) {
	store := ldapFixture(t)
	env, w := ldapAs(store, "nexus:*")
	for _, c := range []struct {
		label  string
		call   func() (Answer, error)
		status int
		fields []string
	}{
		{"create without a body", func() (Answer, error) { return CreateLDAP(env, w, nil) }, 400, []string{"*"}},
		{"create without a password", func() (Answer, error) { return CreateLDAP(env, w, ldapBody("new", "h", "-")) }, 400, []string{"authPassword"}},
		{"create with an empty password", func() (Answer, error) { return CreateLDAP(env, w, ldapBody("new", "h", "")) }, 400, []string{"authPassword"}},
		{"create of a name that exists", func() (Answer, error) { return CreateLDAP(env, w, ldapBody("corp", "h", bindOne)) }, 409, nil},
		{"create with the trust store", func() (Answer, error) {
			return CreateLDAP(env, w, []byte(strings.Replace(string(ldapBody("new", "h", bindOne)), `"useTrustStore":false`, `"useTrustStore":true`, 1)))
		}, 400, []string{"useTrustStore"}},
		{"create without a host", func() (Answer, error) { return CreateLDAP(env, w, ldapBody("new", "", bindOne)) }, 400, []string{"host"}},
		{"save of no connection", func() (Answer, error) { return SaveLDAP(env, w, "none", ldapBody("none", "h", "-")) }, 404, nil},
		{"save with an empty password", func() (Answer, error) { return SaveLDAP(env, w, "corp", ldapBody("corp", "h", "")) }, 400, []string{"authPassword"}},
		{"save with another id", func() (Answer, error) {
			return SaveLDAP(env, w, "corp", []byte(strings.Replace(string(ldapBody("corp", "h", "-")), "{", `{"id":"conn-9",`, 1)))
		}, 400, []string{"id"}},
		{"order without a name", func() (Answer, error) { return OrderLDAP(env, w, []byte(`[]`)) }, 400, []string{"*"}},
		{"order of an object", func() (Answer, error) { return OrderLDAP(env, w, []byte(`{}`)) }, 400, []string{"*"}},
		{"delete of no connection", func() (Answer, error) { return DeleteLDAP(env, w, "none") }, 404, nil},
	} {
		a, err := c.call()
		answered(t, c.label, a, err, c.status, c.fields...)
		noBind(t, c.label, a)
	}
	if store.writes != 0 || len(store.snap.LDAP) != 1 {
		t.Fatalf("a refusal wrote: %d writes", store.writes)
	}
	if entries, _ := os.ReadDir(filepath.Join(store.dir, "secrets")); len(entries) != 0 {
		t.Fatalf("a refusal wrote %d secret files", len(entries))
	}
}

// Ruling I17: a create, a save that changes the directory, a delete and a
// change of order need the full permission; a save of the name, the
// timeouts or the password does not.
func TestLDAPWritesNeedTheFullPermission(t *testing.T) {
	store := ldapFixture(t)
	env, w := ldapAs(store, "nexus:ldap:create", "nexus:ldap:update", "nexus:ldap:delete")
	for _, c := range []struct {
		label string
		call  func() (Answer, error)
	}{
		{"create", func() (Answer, error) { return CreateLDAP(env, w, ldapBody("new", "ldap.example.com", bindOne)) }},
		{"save of the host", func() (Answer, error) { return SaveLDAP(env, w, "corp", ldapBody("corp", "evil.example.com", "-")) }},
		{"delete", func() (Answer, error) { return DeleteLDAP(env, w, "corp") }},
	} {
		a, err := c.call()
		answered(t, c.label, a, err, 403)
		if m, ok := a.Body.(admin.Message); !ok || !strings.Contains(m.Message, "nexus:*") {
			t.Fatalf("%s: %+v", c.label, a.Body)
		}
	}
	if store.writes != 0 || len(store.snap.LDAP) != 1 || store.snap.LDAP[0].Host != "ldap.example.com" {
		t.Fatalf("a refused change wrote: %+v", store.snap.LDAP)
	}
	if got, _ := secretOf(t, store, store.snap.LDAP[0].ID); got != "" {
		t.Fatalf("a refused change wrote a secret: %q", got)
	}
	// what does not decide the directory passes with the permission of the route
	a, err := SaveLDAP(env, w, "corp", ldapBody("corp-renamed", "ldap.example.com", bindTwo))
	answered(t, "save of name and password", a, err, 204)
	if store.snap.LDAP[0].Name != "corp-renamed" {
		t.Fatalf("saved %+v", store.snap.LDAP[0])
	}
	a, err = OrderLDAP(env, w, []byte(`["corp-renamed"]`))
	answered(t, "the stored order", a, err, 204)
}

// A change whose write of the state fails leaves the secret file as it was.
func TestLDAPSecretUndone(t *testing.T) {
	store := ldapFixture(t)
	env, w := ldapAs(store, "nexus:*")
	if _, err := admin.WriteLDAPSecret(store.dir, "conn-1", bindOne); err != nil {
		t.Fatal(err)
	}
	store.fail = true
	if _, err := SaveLDAP(env, w, "corp", ldapBody("corp", "ldap.example.com", bindTwo)); err == nil {
		t.Fatal("the failed write passed")
	}
	store.fail = true
	if _, err := DeleteLDAP(env, w, "corp"); err == nil {
		t.Fatal("the failed delete passed")
	}
	if got, _ := secretOf(t, store, "conn-1"); got != bindOne {
		t.Fatalf("the secret after the failed writes: %q", got)
	}
	store.fail = true
	if _, err := CreateLDAP(env, w, ldapBody("new", "h", bindTwo)); err == nil {
		t.Fatal("the failed create passed")
	}
	if entries, _ := os.ReadDir(filepath.Join(store.dir, "secrets")); len(entries) != 1 {
		t.Fatalf("the failed create left %d secret files", len(entries))
	}
}

// Ruling I17: a change of the order without the full permission is 403
// and writes nothing; the stored sequence again passes.
func TestLDAPOrderNeedsTheFullPermission(t *testing.T) {
	store := ldapFixture(t)
	second := store.snap.LDAP[0]
	second.ID, second.Name, second.Order = "conn-2", "second", 2
	store.snap.LDAP = append(store.snap.LDAP, second)
	env, w := ldapAs(store, "nexus:ldap:update")
	a, err := OrderLDAP(env, w, []byte(`["second","corp"]`))
	answered(t, "a changed order", a, err, 403)
	if m, isMessage := a.Body.(admin.Message); !isMessage || !strings.Contains(m.Message, "nexus:*") {
		t.Fatalf("a changed order: %+v", a.Body)
	}
	if store.writes != 0 || store.snap.LDAP[0].Order != 1 || store.snap.LDAP[1].Order != 2 {
		t.Fatalf("the refused order wrote: %d writes, %+v", store.writes, store.snap.LDAP)
	}
	a, err = OrderLDAP(env, w, []byte(`["corp","second"]`))
	answered(t, "the stored order", a, err, 204)
	env, w = ldapAs(store, "nexus:*")
	a, err = OrderLDAP(env, w, []byte(`["second","corp"]`))
	answered(t, "a changed order with the full permission", a, err, 204)
	if store.snap.LDAP[0].Order != 2 || store.snap.LDAP[1].Order != 1 {
		t.Fatalf("the order: %+v", store.snap.LDAP)
	}
}

// Ruling I23: a save that leaves a scheme with a bind removes the secret
// file; a save to a scheme with a bind needs authPassword while the
// connection has no secret file, and keeps an existing one without it.
func TestLDAPSchemeAndSecret(t *testing.T) {
	store := ldapFixture(t)
	env, w := ldapAs(store, "nexus:ldap:update")
	none := []byte(strings.Replace(string(ldapBody("corp", "ldap.example.com", "-")), `"authScheme":"SIMPLE"`, `"authScheme":"NONE"`, 1))
	// SIMPLE without a file and without a password
	a, err := SaveLDAP(env, w, "corp", ldapBody("corp", "ldap.example.com", "-"))
	answered(t, "SIMPLE without a file", a, err, 400, "authPassword")
	if _, err := admin.WriteLDAPSecret(store.dir, "conn-1", bindOne); err != nil {
		t.Fatal(err)
	}
	a, err = SaveLDAP(env, w, "corp", ldapBody("corp", "ldap.example.com", "-"))
	answered(t, "SIMPLE with a file", a, err, 204)
	if got, mode := secretOf(t, store, "conn-1"); got != bindOne || mode != 0o600 {
		t.Fatalf("the kept file: %q %o", got, mode)
	}
	// to NONE: the file goes
	a, err = SaveLDAP(env, w, "corp", none)
	answered(t, "to NONE", a, err, 204)
	if got, mode := secretOf(t, store, "conn-1"); got != "" || mode != 0 || store.snap.LDAP[0].AuthScheme != "NONE" {
		t.Fatalf("after the save to NONE: %q, %+v", got, store.snap.LDAP[0])
	}
	// back to SIMPLE: without a password 400, with one the file holds it
	a, err = SaveLDAP(env, w, "corp", ldapBody("corp", "ldap.example.com", "-"))
	answered(t, "back to SIMPLE without a password", a, err, 400, "authPassword")
	if store.snap.LDAP[0].AuthScheme != "NONE" {
		t.Fatalf("the refused save wrote: %+v", store.snap.LDAP[0])
	}
	a, err = SaveLDAP(env, w, "corp", ldapBody("corp", "ldap.example.com", bindTwo))
	answered(t, "back to SIMPLE with a password", a, err, 204)
	if got, mode := secretOf(t, store, "conn-1"); got != bindTwo || mode != 0o600 {
		t.Fatalf("the new file: %q %o", got, mode)
	}
	// a failed write of the state puts the file back
	store.fail = true
	if _, err := SaveLDAP(env, w, "corp", none); err == nil {
		t.Fatal("the failed save passed")
	}
	if got, _ := secretOf(t, store, "conn-1"); got != bindTwo {
		t.Fatalf("after the failed save: %q", got)
	}
}

// Ruling I26: a change of order is a change only when the sequence of the
// names differs; the numbers are not compared.
func TestLDAPOrderComparesTheSequence(t *testing.T) {
	store := ldapFixture(t)
	second := store.snap.LDAP[0]
	second.ID, second.Name, second.Order = "conn-2", "second", 5
	store.snap.LDAP = append(store.snap.LDAP, second)
	env, w := ldapAs(store, "nexus:ldap:update")
	a, err := OrderLDAP(env, w, []byte(`["corp","second"]`))
	answered(t, "the stored sequence with other numbers", a, err, 204)
	a, err = OrderLDAP(env, w, []byte(`["second","corp"]`))
	answered(t, "another sequence", a, err, 403)
}
