package ldapauth_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// Review B1 against OpenLDAP: the npm token of a directory user holds the
// rights of its groups, found by a lookup under the service bind, and once
// the user is removed from the directory the token is refused within
// auth.CredentialTime.

const (
	writerDN       = "uid=demo-writer,ou=services,dc=example,dc=com"
	writerPassword = "demo-writer-pass-8"
	tempBase       = "ou=temporary,dc=example,dc=com"
	tempUser       = "demo-ldap-temp"
	tempPassword   = "temp-pass-9"
	tempGroup      = "demo-temp-group"
)

// asAuth gives the errors of ldapauth the meaning auth reads from them, as
// the directory of the command line does.
type asAuth struct{ d *ldapauth.Directory }

func mapped(err error, refused error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ldapauth.ErrInvalidCredentials), errors.Is(err, ldapauth.ErrNoEntry):
		return refused
	case errors.Is(err, ldapauth.ErrUnavailable):
		return fmt.Errorf("%w: %w", auth.ErrUnreachable, err)
	}
	return err
}

func (a asAuth) Login(ctx context.Context, username, password string) (model.User, []string, error) {
	u, g, err := a.d.Login(ctx, username, password)
	return u, g, mapped(err, auth.ErrWrongPair)
}

func (a asAuth) Lookup(ctx context.Context, username string) (model.User, []string, error) {
	u, g, err := a.d.Lookup(ctx, username)
	return u, g, mapped(err, auth.ErrNoUser)
}

// writer binds as the one identity that may write below ou=temporary.
func writer(t *testing.T) *ldap.Conn {
	t.Helper()
	host, port := addrFromEnv(t, "PSR_LDAP_ADDR")
	conn, err := ldap.DialURL("ldap://" + net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Bind(writerDN, writerPassword); err != nil {
		t.Fatal(err)
	}
	return conn
}

type liveClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *liveClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *liveClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestLiveTokenOfADirectoryUser(t *testing.T) {
	s := liveServer(t)
	s.UserBaseDN, s.GroupBaseDN = "ou=temporary", "ou=temporary"
	d := liveDirectory(t, s)
	w := writer(t)
	person := "uid=" + tempUser + "," + tempBase
	group := "cn=" + tempGroup + "," + tempBase
	remove := func(dn string) {
		if err := w.Del(ldap.NewDelRequest(dn, nil)); err != nil && !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			t.Errorf("remove %s: %v", dn, err)
		}
	}
	remove(group)
	remove(person)
	add := ldap.NewAddRequest(person, nil)
	add.Attribute("objectClass", []string{"inetOrgPerson"})
	add.Attribute("uid", []string{tempUser})
	add.Attribute("cn", []string{"Demo Ldap Temp"})
	add.Attribute("sn", []string{"Temp"})
	add.Attribute("mail", []string{tempUser + "@example.com"})
	add.Attribute("userPassword", []string{tempPassword})
	if err := w.Add(add); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { remove(person) })
	add = ldap.NewAddRequest(group, nil)
	add.Attribute("objectClass", []string{"groupOfNames"})
	add.Attribute("cn", []string{tempGroup})
	add.Attribute("member", []string{person})
	if err := w.Add(add); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { remove(group) })

	// a role named like the group grants a right; the user holds no role
	// mapping, so the right comes from the group only
	snap := &model.Snapshot{
		Roles:     []model.Role{{ID: tempGroup, Source: "default", Name: tempGroup, Privileges: []string{"nx-settings-read"}}},
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
	c := &liveClock{t: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
	svc, err := auth.New(auth.Options{Snapshot: snap, Generation: gen, Store: st, Directories: []auth.Directory{asAuth{d}}, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPut, "/repository/demo-npm/-/user/org.couchdb.user:"+tempUser, nil)
	tok, err := svc.NpmLogin(login, tempUser, tempPassword)
	if err != nil {
		t.Fatal(err)
	}
	c.Add(auth.ReloadInterval) // the issue raised the generation
	use := func() (*model.Subject, error) {
		r := httptest.NewRequest(http.MethodGet, "/repository/demo-npm/", nil)
		r.Header.Set("Authorization", "Bearer "+tok.Text)
		return svc.Subject(r)
	}
	sub, err := use()
	if err != nil || sub == nil || sub.User.ID != tempUser || sub.User.Source != auth.SourceLDAP ||
		!svc.Permits(sub, "nexus:settings:read") || svc.Permits(sub, "nexus:security:users:read") {
		t.Fatalf("subject of the npm token: %+v %v", sub, err)
	}

	remove(person)
	c.Add(auth.CredentialTime - time.Second)
	if sub, err := use(); err != nil || sub == nil {
		t.Fatalf("removed, before the answer ended: %+v %v", sub, err)
	}
	c.Add(time.Second)
	if sub, err := use(); sub != nil || !errors.Is(err, model.ErrUnauthorized) {
		t.Fatalf("removed, after the answer ended: %+v %v", sub, err)
	}
}
