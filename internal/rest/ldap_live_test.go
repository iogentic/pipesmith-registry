package rest

// The checks of an LDAP form against the OpenLDAP container of tests/ldap,
// which run.sh there starts. Without PSR_LDAP_ADDR they skip; run.sh fails
// a run with a skip. The passwords are the invented ones of
// tests/ldap/fixture.ldif.

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

const (
	liveBind      = "demo-bind-pass-4"
	liveAdminPass = "admin-pass-1"
)

func liveAddr(t *testing.T, name string) (string, int) {
	t.Helper()
	addr := os.Getenv(name)
	if addr == "" {
		t.Skipf("%s is not set; tests/ldap/run.sh runs this test against the container", name)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return host, n
}

// liveForm is a form of the fixture directory at the address of the
// variable, with the bind password, or without the field for "-".
func liveForm(t *testing.T, name, variable, protocol, password string) map[string]any {
	t.Helper()
	host, port := liveAddr(t, variable)
	server := model.LDAPServer{Name: name, Protocol: protocol, Host: host, Port: port, SearchBase: "dc=example,dc=com",
		AuthScheme: "SIMPLE", AuthUsername: "uid=demo-bind,ou=services,dc=example,dc=com", ConnectionTimeoutSeconds: 5,
		ConnectionRetryDelaySeconds: 300, MaxIncidentsCount: 3, UserBaseDN: "ou=people", UserSubtree: true,
		UserObjectClass: "inetOrgPerson", UserIDAttribute: "uid", UserRealNameAttribute: "cn", UserEmailAddressAttribute: "mail",
		LDAPGroupsAsRoles: true, GroupType: "static", GroupBaseDN: "ou=groups", GroupSubtree: true, GroupObjectClass: "groupOfNames",
		GroupIDAttribute: "cn", GroupMemberAttribute: "member", GroupMemberFormat: "${dn}"}
	form := map[string]any{}
	data, _ := jsonOf(server)
	_ = jsonInto(data, &form)
	delete(form, "id")
	if password != "-" {
		form["authPassword"] = password
	}
	return form
}

func liveCheck(t *testing.T, label string, a Answer, err error, ok bool, words ...string) map[string]any {
	t.Helper()
	m := checked(t, label, a, err)
	for _, secret := range []string{liveBind, liveAdminPass} {
		if data, _ := a.JSON(); strings.Contains(string(data), secret) {
			t.Fatalf("%s: the answer holds a password", label)
		}
	}
	if m["ok"] != ok {
		t.Fatalf("%s: %v, want ok %v", label, m, ok)
	}
	for _, w := range words {
		if !strings.Contains(m["message"].(string), w) {
			t.Fatalf("%s: the message %q does not name %q", label, m["message"], w)
		}
	}
	return m
}

func TestLiveLDAPChecks(t *testing.T) {
	store := ldapFixture(t)
	store.snap.LDAP = nil
	env, w := ldapAs(store, "nexus:ldap:update")
	form := liveForm(t, "live", "PSR_LDAP_ADDR", "ldap", liveBind)

	a, err := VerifyLDAPConnection(env, w, bodyOf(t, form))
	liveCheck(t, "connection", a, err, true, "service bind as uid=demo-bind", "search base dc=example,dc=com found")

	a, err = VerifyLDAPUserMapping(env, w, bodyOf(t, form))
	m := liveCheck(t, "mapping", a, err, true, "7 users")
	users := m["users"].([]any)
	first := users[0].(map[string]any)
	if len(users) != 7 || first["userId"] != "demo-ldap-admin" || first["name"] != "Demo Ldap Admin" ||
		first["email"] != "demo-ldap-admin@example.com" || len(first["groups"].([]any)) != 2 {
		t.Fatalf("mapping: %v", users)
	}

	login := func(f map[string]any, user, password string) (Answer, error) {
		return VerifyLDAPLogin(env, w, bodyOf(t, map[string]any{"connection": f, "username": user, "password": password}))
	}
	a, err = login(form, "demo-ldap-admin", liveAdminPass)
	liveCheck(t, "login", a, err, true, "demo-ldap-admin", "demo-admins, demo-readers")
	a, err = login(form, "demo-ldap-admin", "wrong-pass")
	liveCheck(t, "wrong password", a, err, false, "did not accept")

	// a wrong bind password fails at the service bind
	a, err = VerifyLDAPConnection(env, w, bodyOf(t, liveForm(t, "live", "PSR_LDAP_ADDR", "ldap", "wrong-bind")))
	liveCheck(t, "wrong bind", a, err, false, "service bind")

	// a directory that is down
	a, err = VerifyLDAPConnection(env, w, bodyOf(t, liveForm(t, "live", "PSR_LDAP_DOWN_ADDR", "ldap", liveBind)))
	liveCheck(t, "down", a, err, false, "the directory could not be reached")
	if store.writes != 0 {
		t.Fatalf("a check wrote %d times", store.writes)
	}
}

// Rulings I19 and I20 against the container: a blank password of a stored
// name binds with the stored secret, and ldaps trusts the stored CA file of
// that connection besides the system roots.
func TestLiveLDAPChecksWithTheStoredConnection(t *testing.T) {
	store := ldapFixture(t)
	ca := os.Getenv("PSR_LDAP_CA")
	if ca == "" {
		t.Skip("PSR_LDAP_CA is not set; tests/ldap/run.sh runs this test against the container")
	}
	stored := liveForm(t, "live", "PSR_LDAPS_ADDR", "ldaps", "-")
	server, err := admin.LDAPConnection(stored, "")
	if err != nil {
		t.Fatal(err)
	}
	server.ID, server.Order, server.CAFile = "live-1", 1, ca
	store.snap.LDAP = []model.LDAPServer{server}
	t.Setenv(ldapauth.PasswordVariable("live-1"), "")
	if _, err := admin.WriteLDAPSecret(store.dir, "live-1", liveBind); err != nil {
		t.Fatal(err)
	}
	env, w := ldapAs(store, "nexus:ldap:update")
	a, err := VerifyLDAPConnection(env, w, bodyOf(t, stored))
	liveCheck(t, "stored secret and CA", a, err, true, "certificate verified", "service bind")

	// the same form under a new name has neither the secret nor the CA: the
	// handshake fails, which is a directory not reached (I25)
	other := liveForm(t, "other", "PSR_LDAPS_ADDR", "ldaps", liveBind)
	a, err = VerifyLDAPConnection(env, w, bodyOf(t, other))
	liveCheck(t, "no stored CA", a, err, false, "the directory could not be reached")
	if store.writes != 0 {
		t.Fatalf("a check wrote %d times", store.writes)
	}
}
