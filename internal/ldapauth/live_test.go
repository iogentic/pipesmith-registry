package ldapauth_test

// Tests against the OpenLDAP container of tests/ldap, which run.sh there
// starts. Without PSR_LDAP_ADDR they skip; run.sh fails a run with a skip.

import (
	"context"
	"errors"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/ldapauth"
	"github.com/mirosso25/pipesmith-registry/internal/model"
)

// Invented passwords of tests/ldap/fixture.ldif.
const fixtureBindPassword = "demo-bind-pass-4"

var fixturePasswords = map[string]string{
	"demo-ldap-admin":      "admin-pass-1",
	"demo-ldap-reader":     "reader-pass-Äöü-2",
	"demo-ldap-nobody":     "nobody-pass-3",
	"demo-ldap-odd,(x)*":   "odd-pass-5",
	"demo-ldap-twin":       "twin-pass-6",
	"demo-ldap-contractor": "contractor-pass-7",
}

func addrFromEnv(t *testing.T, name string) (string, int) {
	t.Helper()
	addr := os.Getenv(name)
	if addr == "" {
		t.Skipf("%s is not set; tests/ldap/run.sh runs this test against the container", name)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return host, n
}

func liveServer(t *testing.T) model.LDAPServer {
	t.Helper()
	s := validServer()
	s.Host, s.Port = addrFromEnv(t, "PSR_LDAP_ADDR")
	s.ConnectionTimeoutSeconds = 5
	return s
}

// liveDirectory builds the directory the way the server does: the bind
// password comes from the variable of the connection id.
func liveDirectory(t *testing.T, s model.LDAPServer, opts ...ldapauth.Option) *ldapauth.Directory {
	t.Helper()
	t.Setenv(ldapauth.PasswordVariable(s.ID), fixtureBindPassword)
	password, ok := ldapauth.PasswordFromEnv(s.ID)
	if !ok {
		t.Fatal("the bind password variable is not read")
	}
	if strings.EqualFold(s.AuthScheme, "NONE") {
		password = ""
	}
	d, err := ldapauth.New(s, password, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func signIn(t *testing.T, d interface {
	Login(context.Context, string, string) (model.User, []string, error)
}, username string) (model.User, []string) {
	t.Helper()
	user, groups, err := d.Login(context.Background(), username, fixturePasswords[username])
	if err != nil {
		t.Fatalf("sign in of %s: %v", username, err)
	}
	return user, groups
}

func wantGroups(t *testing.T, who string, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got == nil || !slices.Equal(got, want) {
		t.Errorf("groups of %s: %q, want %q", who, got, want)
	}
}

func TestLiveSignInStaticGroups(t *testing.T) {
	d := liveDirectory(t, liveServer(t))
	user, groups := signIn(t, d, "demo-ldap-admin")
	want := model.User{ID: "demo-ldap-admin", FirstName: "Demo Ldap Admin", Email: "demo-ldap-admin@example.com",
		Status: "active", Source: "LDAP", Roles: []string{}}
	if user.ID != want.ID || user.FirstName != want.FirstName || user.LastName != "" || user.Email != want.Email ||
		user.Status != want.Status || user.Source != want.Source || user.Roles == nil || len(user.Roles) != 0 ||
		user.PasswordHash != "" {
		t.Errorf("user %#v, want %#v", user, want)
	}
	wantGroups(t, "admin", groups, "demo-admins", "demo-readers")
	_, groups = signIn(t, d, "demo-ldap-reader")
	wantGroups(t, "reader", groups, "demo-readers", "demo-team")
	_, groups = signIn(t, d, "demo-ldap-nobody")
	wantGroups(t, "nobody", groups, "demo-unmapped")
	user, groups = signIn(t, d, "demo-ldap-odd,(x)*")
	if user.ID != "demo-ldap-odd,(x)*" {
		t.Errorf("odd user id %q", user.ID)
	}
	wantGroups(t, "odd", groups, "demo-odd")
	_, groups = signIn(t, d, "demo-ldap-contractor")
	wantGroups(t, "contractor", groups)
}

func TestLiveWrongPairs(t *testing.T) {
	d := liveDirectory(t, liveServer(t))
	cases := []struct{ username, password string }{
		{"demo-ldap-admin", "admin-pass-2"},
		{"demo-ldap-admin", "reader-pass-Äöü-2"},
		{"demo-ldap-reader", "reader-pass-Aou-2"},
		{"demo-nobody-at-all", "admin-pass-1"},
		{"*)(uid=*", "admin-pass-1"},
		{"*", "admin-pass-1"},
		{"demo-ldap-*", "admin-pass-1"},
		{"demo-ldap-admin)(|(uid=*", "admin-pass-1"},
		{"demo-bind", fixtureBindPassword},
	}
	for _, c := range cases {
		user, groups, err := d.Login(context.Background(), c.username, c.password)
		if err != ldapauth.ErrInvalidCredentials || user.ID != "" || groups != nil {
			t.Errorf("Login(%q) = %v, %q, %v", c.username, user, groups, err)
		}
	}
}

func TestLiveLetterCase(t *testing.T) {
	d := liveDirectory(t, liveServer(t))
	user, _, err := d.Login(context.Background(), "DEMO-LDAP-READER", fixturePasswords["demo-ldap-reader"])
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != "demo-ldap-reader" {
		t.Fatalf("id %q, want the spelling of the directory", user.ID)
	}
}

func TestLiveAnonymousScheme(t *testing.T) {
	s := liveServer(t)
	s.AuthScheme = "NONE"
	s.AuthUsername = ""
	d := liveDirectory(t, s)
	_, groups := signIn(t, d, "demo-ldap-reader")
	wantGroups(t, "reader", groups, "demo-readers", "demo-team")
	if _, _, err := d.Login(context.Background(), "demo-ldap-reader", "wrong"); err != ldapauth.ErrInvalidCredentials {
		t.Fatalf("wrong password: %v", err)
	}
}

func TestLiveServiceBindRefused(t *testing.T) {
	s := liveServer(t)
	d, err := ldapauth.New(s, canaryBindPassword)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = d.Login(context.Background(), "demo-ldap-reader", fixturePasswords["demo-ldap-reader"])
	if !errors.Is(err, ldapauth.ErrUnavailable) || errors.Is(err, ldapauth.ErrInvalidCredentials) {
		t.Fatalf("wrong bind password: %v", err)
	}
	for _, secret := range []string{canaryBindPassword, fixturePasswords["demo-ldap-reader"]} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("the error holds a password")
		}
	}
}

func TestLiveUserSubtree(t *testing.T) {
	s := liveServer(t)
	d := liveDirectory(t, s)
	if _, _, err := d.Login(context.Background(), "demo-ldap-twin", fixturePasswords["demo-ldap-twin"]); err != ldapauth.ErrInvalidCredentials {
		t.Fatalf("two entries with one id: %v", err)
	}
	s.UserSubtree = false
	d = liveDirectory(t, s)
	if _, _, err := d.Login(context.Background(), "demo-ldap-contractor", fixturePasswords["demo-ldap-contractor"]); err != ldapauth.ErrInvalidCredentials {
		t.Fatalf("person below the user base without subtree: %v", err)
	}
	user, _ := signIn(t, d, "demo-ldap-twin")
	if user.FirstName != "Demo Ldap Twin One" {
		t.Fatalf("twin without subtree: %q", user.FirstName)
	}
}

func TestLiveGroupSubtree(t *testing.T) {
	s := liveServer(t)
	s.GroupSubtree = false
	_, groups := signIn(t, liveDirectory(t, s), "demo-ldap-reader")
	wantGroups(t, "reader", groups, "demo-readers")
}

func TestLiveGroupClasses(t *testing.T) {
	s := liveServer(t)
	s.GroupObjectClass = "groupOfUniqueNames"
	s.GroupMemberAttribute = "uniqueMember"
	s.GroupMemberFormat = "uid=${username},ou=people,dc=example,dc=com"
	d := liveDirectory(t, s)
	_, groups := signIn(t, d, "demo-ldap-reader")
	wantGroups(t, "reader, unique names", groups, "demo-unique-readers")
	_, groups = signIn(t, d, "demo-ldap-admin")
	wantGroups(t, "admin, unique names", groups)

	s = liveServer(t)
	s.GroupObjectClass = "posixGroup"
	s.GroupMemberAttribute = "memberUid"
	s.GroupMemberFormat = "${username}"
	_, groups = signIn(t, liveDirectory(t, s), "demo-ldap-reader")
	wantGroups(t, "reader, posix", groups, "demo-posix-readers")

	s = liveServer(t)
	s.GroupMemberFormat = "uid=${username},ou=people,dc=example,dc=com"
	_, groups = signIn(t, liveDirectory(t, s), "demo-ldap-odd,(x)*")
	wantGroups(t, "odd, format with the user name", groups, "demo-odd")
}

func TestLiveDynamicGroups(t *testing.T) {
	s := liveServer(t)
	s.GroupType = "dynamic"
	s.UserMemberOfAttribute = "memberOf"
	d := liveDirectory(t, s)
	_, groups := signIn(t, d, "demo-ldap-admin")
	wantGroups(t, "admin", groups,
		"cn=demo-admins,ou=groups,dc=example,dc=com", "cn=demo-readers,ou=groups,dc=example,dc=com",
		"demo-admins", "demo-readers")
	_, groups = signIn(t, d, "demo-ldap-reader")
	wantGroups(t, "reader", groups,
		"cn=demo-readers,ou=groups,dc=example,dc=com", "cn=demo-team,ou=teams,ou=groups,dc=example,dc=com",
		"demo-readers", "demo-team")
	_, groups = signIn(t, d, "demo-ldap-contractor")
	wantGroups(t, "contractor", groups)
}

func TestLiveGroupsOff(t *testing.T) {
	s := liveServer(t)
	s.LDAPGroupsAsRoles = false
	_, groups := signIn(t, liveDirectory(t, s), "demo-ldap-admin")
	wantGroups(t, "admin", groups)
}

func TestLiveUserFilter(t *testing.T) {
	s := liveServer(t)
	s.UserLDAPFilter = "(mail=*@example.com)"
	d := liveDirectory(t, s)
	signIn(t, d, "demo-ldap-admin")
	if _, _, err := d.Login(context.Background(), "demo-ldap-contractor", fixturePasswords["demo-ldap-contractor"]); err != ldapauth.ErrInvalidCredentials {
		t.Fatalf("person outside the filter: %v", err)
	}
	s.UserLDAPFilter = "!(uid=demo-ldap-nobody)"
	d = liveDirectory(t, s)
	signIn(t, d, "demo-ldap-reader")
	if _, _, err := d.Login(context.Background(), "demo-ldap-nobody", fixturePasswords["demo-ldap-nobody"]); err != ldapauth.ErrInvalidCredentials {
		t.Fatalf("person outside the negated filter: %v", err)
	}
}

func TestLiveLDAPS(t *testing.T) {
	s := liveServer(t)
	s.Protocol = "ldaps"
	s.Host, s.Port = addrFromEnv(t, "PSR_LDAPS_ADDR")
	caFile := os.Getenv("PSR_LDAP_CA")
	if caFile == "" {
		t.Skip("PSR_LDAP_CA is not set")
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	_, groups := signIn(t, liveDirectory(t, s, ldapauth.WithCACertificates(ca)), "demo-ldap-reader")
	wantGroups(t, "reader over ldaps", groups, "demo-readers", "demo-team")

	_, _, err = liveDirectory(t, s).Login(context.Background(), "demo-ldap-reader", fixturePasswords["demo-ldap-reader"])
	if !errors.Is(err, ldapauth.ErrUnavailable) {
		t.Fatalf("ldaps without the test CA: %v", err)
	}
}

func TestLiveChainNextAnswers(t *testing.T) {
	down := liveServer(t)
	down.ID, down.Name, down.Order = "demo-down-1", "demo-down", 1
	down.Host, down.Port = addrFromEnv(t, "PSR_LDAP_DOWN_ADDR")
	live := liveServer(t)
	live.Order = 2
	chain := ldapauth.NewChain(liveDirectory(t, live), liveDirectory(t, down))
	start := time.Now()
	user, groups := signIn(t, chain, "demo-ldap-reader")
	if elapsed := time.Since(start); elapsed > time.Duration(down.ConnectionTimeoutSeconds)*time.Second {
		t.Errorf("the stopped server held the sign in for %v", elapsed)
	}
	if user.ID != "demo-ldap-reader" {
		t.Errorf("user %q", user.ID)
	}
	wantGroups(t, "reader through the chain", groups, "demo-readers", "demo-team")
	// One directory refused the pair and one could not answer: the pair is refused.
	if _, _, err := chain.Login(context.Background(), "demo-ldap-reader", "wrong"); err != ldapauth.ErrInvalidCredentials {
		t.Fatalf("wrong password with a stopped server in the chain: %v", err)
	}

	// A directory that refuses the pair does not stop the next one.
	first := liveServer(t)
	first.ID, first.Order = "demo-filtered-1", 1
	first.UserLDAPFilter = "(uid=demo-ldap-admin)"
	chain = ldapauth.NewChain(liveDirectory(t, first), liveDirectory(t, live))
	signIn(t, chain, "demo-ldap-reader")
	if _, _, err := chain.Login(context.Background(), "demo-ldap-reader", "wrong"); err != ldapauth.ErrInvalidCredentials {
		t.Fatalf("wrong password through the chain: %v", err)
	}
}

func TestLiveConcurrentSignIns(t *testing.T) {
	d := liveDirectory(t, liveServer(t))
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				username := []string{"demo-ldap-admin", "demo-ldap-reader"}[(i+j)%2]
				password := fixturePasswords[username]
				wrong := j == 3
				if wrong {
					password += "-wrong"
				}
				user, _, err := d.Login(context.Background(), username, password)
				switch {
				case wrong && err != ldapauth.ErrInvalidCredentials:
					errs <- "wrong password of " + username + ": " + errString(err)
				case !wrong && (err != nil || user.ID != username):
					errs <- "sign in of " + username + ": " + errString(err)
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func errString(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}
