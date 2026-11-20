package rest

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth/ldapfake"
)

// closedPort is a port of 127.0.0.1 that nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// checkForm is a form of the contract shape at the address, with the bind
// password, or without the field for "-".
func checkForm(name, host string, port int, password string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(ldapBody(name, host, password), &m) // the body of the tests is an object
	m["port"] = port
	return m
}

func bodyOf(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// checked is the decoded answer of a check: 200 with ok and message.
func checked(t *testing.T, label string, a Answer, err error) map[string]any {
	t.Helper()
	answered(t, label, a, err, 200)
	noBind(t, label, a)
	var m map[string]any
	data, _ := a.JSON()
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: %s", label, data)
	}
	if _, isBool := m["ok"].(bool); !isBool || m["message"] == "" {
		t.Fatalf("%s: %s", label, data)
	}
	return m
}

// A directory that is down is a check with ok false, never a 500.
func TestLDAPChecksOfADirectoryThatIsDown(t *testing.T) {
	store := ldapFixture(t)
	env, w := ldapAs(store, "nexus:ldap:update")
	form := checkForm("new", "127.0.0.1", closedPort(t), bindOne)
	for label, call := range map[string]func() (Answer, error){
		"connection": func() (Answer, error) { return VerifyLDAPConnection(env, w, bodyOf(t, form)) },
		"mapping":    func() (Answer, error) { return VerifyLDAPUserMapping(env, w, bodyOf(t, form)) },
		"login": func() (Answer, error) {
			return VerifyLDAPLogin(env, w, bodyOf(t, map[string]any{"connection": form, "username": "demo-ldap-admin", "password": "x"}))
		},
	} {
		a, err := call()
		m := checked(t, label, a, err)
		if m["ok"] != false || m["message"] != "the directory could not be reached" {
			t.Fatalf("%s: %v", label, m)
		}
		if label == "mapping" && (m["users"] == nil || len(m["users"].([]any)) != 0) {
			t.Fatalf("mapping: users %v", m["users"])
		}
	}
	if store.writes != 0 || store.reset != 0 {
		t.Fatalf("a check wrote: %d writes, %d resets", store.writes, store.reset)
	}
}

func TestLDAPCheckRefusals(t *testing.T) {
	store := ldapFixture(t)
	env, w := ldapAs(store, "nexus:ldap:update")
	port := closedPort(t)
	noPort := checkForm("new", "127.0.0.1", port, bindOne)
	delete(noPort, "port")
	for _, c := range []struct {
		label  string
		call   func() (Answer, error)
		fields []string
	}{
		{"no body", func() (Answer, error) { return VerifyLDAPConnection(env, w, nil) }, []string{"*"}},
		{"no port", func() (Answer, error) { return VerifyLDAPConnection(env, w, bodyOf(t, noPort)) }, []string{"port"}},
		{"a new name without a password", func() (Answer, error) {
			return VerifyLDAPUserMapping(env, w, bodyOf(t, checkForm("new", "127.0.0.1", port, "-")))
		}, []string{"authPassword"}},
		{"login without the connection", func() (Answer, error) {
			return VerifyLDAPLogin(env, w, []byte(`{"username":"a","password":"b"}`))
		}, []string{"connection"}},
		{"login with a form field and no user", func() (Answer, error) {
			return VerifyLDAPLogin(env, w, bodyOf(t, map[string]any{"connection": noPort, "password": "b"}))
		}, []string{"connection.port", "username"}},
		{"login of a new name without a password", func() (Answer, error) {
			return VerifyLDAPLogin(env, w, bodyOf(t, map[string]any{"connection": checkForm("new", "127.0.0.1", port, ""),
				"username": "a", "password": "b"}))
		}, []string{"connection.authPassword"}},
	} {
		a, err := c.call()
		answered(t, c.label, a, err, 400, c.fields...)
	}
}

// Ruling I19: a blank password of a stored name takes the stored secret;
// sent to another address it needs the full permission.
func TestLDAPCheckWithTheStoredSecret(t *testing.T) {
	store := ldapFixture(t)
	port := closedPort(t)
	elsewhere := checkForm("corp", "127.0.0.1", port, "")
	env, w := ldapAs(store, "nexus:ldap:update")
	a, err := VerifyLDAPConnection(env, w, bodyOf(t, elsewhere))
	answered(t, "another address", a, err, 403)
	if m, isMessage := a.Body.(admin.Message); !isMessage || !strings.Contains(m.Message, "nexus:*") {
		t.Fatalf("another address: %+v", a.Body)
	}
	// with the full permission and no stored secret: ok false
	env, w = ldapAs(store, "nexus:*")
	a, err = VerifyLDAPConnectionOf(env, w, elsewhere)
	m := checked(t, "no stored secret", a, err)
	if m["ok"] != false || !strings.Contains(m["message"].(string), "no stored bind password") {
		t.Fatalf("no stored secret: %v", m)
	}
	// with a stored secret the check connects
	if _, err := admin.WriteLDAPSecret(store.dir, "conn-1", bindOne); err != nil {
		t.Fatal(err)
	}
	a, err = VerifyLDAPConnectionOf(env, w, elsewhere)
	m = checked(t, "stored secret", a, err)
	if m["ok"] != false || m["message"] != "the directory could not be reached" {
		t.Fatalf("stored secret: %v", m)
	}
	// the stored address takes the stored secret with the permission of the
	// route: a directory that takes only that password passes the check
	fake := startFake(t, bindOne)
	store.snap.LDAP[0].Host, store.snap.LDAP[0].Port = "127.0.0.1", fake.Port()
	env, w = ldapAs(store, "nexus:ldap:update")
	a, err = VerifyLDAPConnection(env, w, bodyOf(t, checkForm("corp", "127.0.0.1", fake.Port(), "-")))
	if m = checked(t, "the stored address", a, err); m["ok"] != true || !slices.Equal(fake.Binds(), []string{bindOne}) {
		t.Fatalf("the stored address: %v, binds %d", m, len(fake.Binds()))
	}
	// another stored secret fails at the service bind of that directory
	if _, err := admin.WriteLDAPSecret(store.dir, "conn-1", bindTwo); err != nil {
		t.Fatal(err)
	}
	a, err = VerifyLDAPConnection(env, w, bodyOf(t, checkForm("corp", "127.0.0.1", fake.Port(), "-")))
	if m = checked(t, "another stored secret", a, err); m["ok"] != false || !strings.Contains(m["message"].(string), "service bind") {
		t.Fatalf("another stored secret: %v", m)
	}
}

// VerifyLDAPConnectionOf checks a form given as an object.
func VerifyLDAPConnectionOf(env *Env, w IdentityWrite, form map[string]any) (Answer, error) {
	data, _ := json.Marshal(form) // a form of the tests always encodes
	return VerifyLDAPConnection(env, w, data)
}

func jsonOf(v any) ([]byte, error)      { return json.Marshal(v) }
func jsonInto(data []byte, v any) error { return json.Unmarshal(data, v) }

// Ruling I22: a check that takes the stored secret runs with the stored
// directory settings; another base, filter or attribute needs the full
// permission, since the search runs as the service identity.
func TestLDAPCheckWithTheStoredSecretKeepsTheDirectory(t *testing.T) {
	store := ldapFixture(t)
	port := closedPort(t)
	store.snap.LDAP[0].Host, store.snap.LDAP[0].Port = "127.0.0.1", port
	if _, err := admin.WriteLDAPSecret(store.dir, "conn-1", bindOne); err != nil {
		t.Fatal(err)
	}
	for _, change := range []map[string]any{
		{"searchBase": "dc=example,dc=org"}, {"userBaseDn": "ou=services"}, {"userLdapFilter": "(objectClass=*)"},
		{"userIdAttribute": "cn"}, {"userEmailAddressAttribute": "userPassword"}, {"userRealNameAttribute": "description"},
		{"groupIdAttribute": "member"}, {"ldapGroupsAsRoles": true, "groupBaseDn": "ou=groups", "groupIdAttribute": "cn",
			"groupMemberAttribute": "member", "groupMemberFormat": "${dn}"},
	} {
		form := checkForm("corp", "127.0.0.1", port, "-")
		for key, value := range change {
			form[key] = value
		}
		env, w := ldapAs(store, "nexus:ldap:update")
		a, err := VerifyLDAPUserMapping(env, w, bodyOf(t, form))
		answered(t, fmt.Sprint(change), a, err, 403)
		if m, isMessage := a.Body.(admin.Message); !isMessage || !strings.Contains(m.Message, "nexus:*") {
			t.Fatalf("%v: %+v", change, a.Body)
		}
		// with a password of its own the form runs, and with the full permission too
		form["authPassword"] = bindTwo
		a, err = VerifyLDAPUserMapping(env, w, bodyOf(t, form))
		answered(t, fmt.Sprint(change, " with a password"), a, err, 200)
		delete(form, "authPassword")
		env, w = ldapAs(store, "nexus:*")
		a, err = VerifyLDAPUserMapping(env, w, bodyOf(t, form))
		answered(t, fmt.Sprint(change, " with the full permission"), a, err, 200)
	}
}

// fakeUsers are n invented entries of the fake directory.
func fakeUsers(n int) []ldapfake.Entry {
	var list []ldapfake.Entry
	for i := range n {
		id := fmt.Sprintf("demo-person-%02d", i)
		list = append(list, ldapfake.Entry{DN: "uid=" + id + ",dc=example,dc=com", Attributes: map[string][]string{"uid": {id}}})
	}
	return list
}

func startFake(t *testing.T, password string) *ldapfake.Server {
	t.Helper()
	fake, err := ldapfake.Start(password)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	return fake
}

// Ruling I24: a check past a bound of the reads or of the time answers ok
// false with a message that names the bound.
func TestLDAPCheckBounds(t *testing.T) {
	store := ldapFixture(t)
	env, w := ldapAs(store, "nexus:ldap:update")
	fake := startFake(t, bindOne)
	fake.Users, fake.IgnoreSizeLimit = fakeUsers(40), true
	form := checkForm("new", "127.0.0.1", fake.Port(), bindOne)
	a, err := VerifyLDAPUserMapping(env, w, bodyOf(t, form))
	m := checked(t, "more users than the bound", a, err)
	if m["ok"] != false || !strings.Contains(m["message"].(string), "more than 21 user entries") || len(m["users"].([]any)) != 0 {
		t.Fatalf("more users than the bound: %v", m)
	}
	// a directory that keeps to the size limit: the first 20
	fake.IgnoreSizeLimit = false
	a, err = VerifyLDAPUserMapping(env, w, bodyOf(t, form))
	m = checked(t, "a directory within the bound", a, err)
	if m["ok"] != true || len(m["users"].([]any)) != 20 || !strings.Contains(m["message"].(string), "more than 20") {
		t.Fatalf("a directory within the bound: %v", m)
	}
	// the time of a check
	saved := ldapCheckTime
	t.Cleanup(func() { ldapCheckTime = saved })
	ldapCheckTime = 500 * time.Millisecond
	fake.Delay = 3 * time.Second
	start := time.Now()
	a, err = VerifyLDAPConnection(env, w, bodyOf(t, form))
	m = checked(t, "a slow directory", a, err)
	if m["ok"] != false || !strings.Contains(m["message"].(string), "took longer than 500ms") || time.Since(start) > 2*time.Second {
		t.Fatalf("a slow directory after %s: %v", time.Since(start), m)
	}
}

// Ruling I25: a refused port, a port that closes at once and one that
// stays silent give one message for each check; the detail goes to the log.
func TestLDAPCheckOfAnUnreachableDirectory(t *testing.T) {
	var log strings.Builder
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(saved) })
	store := ldapFixture(t)
	env, w := ldapAs(store, "nexus:ldap:update")
	closing, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closing.Close() })
	go func() {
		for {
			conn, err := closing.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	for name, port := range map[string]int{"refused": closedPort(t), "closed at once": closing.Addr().(*net.TCPAddr).Port} {
		form := checkForm("new", "127.0.0.1", port, bindOne)
		for label, call := range map[string]func() (Answer, error){
			"connection": func() (Answer, error) { return VerifyLDAPConnection(env, w, bodyOf(t, form)) },
			"mapping":    func() (Answer, error) { return VerifyLDAPUserMapping(env, w, bodyOf(t, form)) },
			"login": func() (Answer, error) {
				return VerifyLDAPLogin(env, w, bodyOf(t, map[string]any{"connection": form, "username": "u", "password": "p"}))
			},
		} {
			a, err := call()
			m := checked(t, name+" "+label, a, err)
			if m["ok"] != false || m["message"] != "the directory could not be reached" {
				t.Fatalf("%s %s: %v", name, label, m)
			}
		}
	}
	if !strings.Contains(log.String(), "connection refused") {
		t.Fatalf("the log holds no detail: %s", log.String())
	}
	if strings.Contains(log.String(), bindOne) {
		t.Fatal("the log holds the bind password")
	}
}
