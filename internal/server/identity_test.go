package server

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	adminrules "github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	idstate "github.com/iogentic/pipesmith-registry/internal/state"
)

// The seam of the identity writes: with security on the server keeps the
// identity store of its options.
func TestIdentitySeam(t *testing.T) {
	sec := newFakeSecurity()
	a := newSecureApp(t, sec)
	if a.srv.identity != adminrules.IdentityStore(sec) {
		t.Fatalf("the server keeps %v as its identity store, want the fake", a.srv.identity)
	}
	if updates, resets := sec.writes(); updates != 0 || resets != 0 {
		t.Fatalf("the start wrote: %d updates, %d resets", updates, resets)
	}
}

const (
	rolesPath     = "/service/rest/v1/security/roles"
	anonymousPath = "/service/rest/v1/security/anonymous"
	realmsPath    = "/service/rest/v1/security/realms/active"
)

// identityRequest is one write of the area security with a body that
// changes something.
type identityRequest struct {
	method, target, body, ctype string
	status                      int
}

// identityWrites are the nine writes in an order in which each one passes.
var identityWrites = []identityRequest{
	{"POST", rolesPath, `{"id":"demo-new","name":"New","description":"","privileges":["demo-all"],"roles":[]}`, jsonType, 200},
	{"PUT", rolesPath + "/demo-new", `{"id":"demo-new","name":"Renamed","privileges":[],"roles":[]}`, jsonType, 204},
	{"POST", usersPath, `{"userId":"demo-new","firstName":"N","lastName":"U","emailAddress":"n@example.com","password":"example-new-1",` +
		`"status":"active","roles":["demo-new"]}`, jsonType, 200},
	{"PUT", usersPath + "/demo-new", `{"userId":"demo-new","firstName":"M","lastName":"U","emailAddress":"n@example.com","source":"default",` +
		`"status":"active","readOnly":false,"roles":[],"externalRoles":[]}`, jsonType, 204},
	{"PUT", usersPath + "/demo-new/change-password", "example-new-2", "text/plain", 204},
	{"DELETE", usersPath + "/demo-new?realm=default", "", "", 204},
	{"DELETE", rolesPath + "/demo-new", "", "", 204},
	{"PUT", anonymousPath, `{"enabled":true,"userId":"anonymous","realmName":"NexusAuthorizingRealm"}`, jsonType, 200},
	{"PUT", realmsPath, `["NexusAuthenticatingRealm","NexusAuthorizingRealm"]`, jsonType, 204},
}

// noSecret fails for an answer that holds a stored form of a password or a
// password of the writes (A11).
func noSecret(t *testing.T, label string, r *answer) {
	t.Helper()
	for _, secret := range []string{"$shiro1$", "example-new-1", "example-new-2", "passwordHash"} {
		if strings.Contains(string(r.body), secret) {
			t.Fatalf("%s: the answer holds %q: %s", label, secret, r.body)
		}
	}
}

func sendWrite(t *testing.T, a *secureApp, c identityRequest, headers ...string) *answer {
	t.Helper()
	if c.ctype != "" {
		headers = append(headers, "Content-Type", c.ctype)
	}
	return do(t, a.addr(), c.method, c.target, []byte(c.body), headers...)
}

func TestIdentityWrites(t *testing.T) {
	sec := newFakeSecurity()
	a := newSecureApp(t, sec)
	for i, c := range identityWrites {
		r := sendWrite(t, a, c, "basic", asAdmin)
		label := c.method + " " + c.target
		if r.status != c.status {
			t.Fatalf("%s: %d %q", label, r.status, r.body)
		}
		noSecret(t, label, r)
		if c.status == 204 && len(r.body) != 0 {
			t.Fatalf("%s: a 204 with a body %q", label, r.body)
		}
		if c.status == 200 && r.header.Get("Content-Type") != jsonType {
			t.Fatalf("%s: content type %q", label, r.header.Get("Content-Type"))
		}
		if updates, resets := sec.writes(); updates != i+1 || resets != i+1 {
			t.Fatalf("%s: %d updates and %d resets, want %d", label, updates, resets, i+1)
		}
	}
	snap := sec.Snapshot()
	if !snap.Anonymous.Enabled || len(snap.Realms) != 2 || len(snap.Users) != 1 || len(snap.Roles) != 1 {
		t.Fatalf("after the writes: %+v", snap)
	}
	if r := a.send("GET", anonymousPath, "basic", asAdmin); r.status != 200 || r.object(t)["enabled"] != true {
		t.Fatalf("anonymous after the save: %d %s", r.status, r.body)
	}
	// refusals in the shapes of section Errors; none writes
	before, _ := sec.writes()
	for _, c := range []identityRequest{
		{"POST", rolesPath, "", "", 400},
		{"POST", usersPath, "", "", 400},
		{"PUT", anonymousPath, "", "", 400},
		{"PUT", realmsPath, "", "", 400},
		{"PUT", realmsPath, `[]`, jsonType, 400},
		{"PUT", rolesPath + "/x1", "", "", 400},
		{"PUT", rolesPath + "/x1", `{"name":"x"}`, jsonType, 404},
		{"DELETE", rolesPath + "/x1", "", "", 404},
		{"PUT", usersPath + "/x1", "", "", 400},
		{"PUT", usersPath + "/x1", `{"status":"active"}`, jsonType, 404},
		{"DELETE", usersPath + "/x1", "", "", 404},
		{"PUT", usersPath + "/x1/change-password", "", "", 404},
		{"DELETE", rolesPath + "/nx-admin", "", "", 400},
		{"DELETE", rolesPath + "/demo-administrators", "", "", 409},
		{"POST", rolesPath, `{"id":"demo-administrators","name":"Again"}`, jsonType, 409},
		{"DELETE", usersPath + "/demo-admin", "", "", 409},
		{"PUT", usersPath + "/demo-admin", `{"userId":"demo-admin","status":"disabled","roles":["demo-administrators"]}`, jsonType, 409},
		{"PUT", usersPath + "/demo-admin", `{"userId":"demo-admin","status":"active","roles":[]}`, jsonType, 409},
	} {
		r := sendWrite(t, a, c, "basic", asAdmin)
		label := c.method + " " + c.target + " " + c.body
		if r.status != c.status {
			t.Errorf("%s: %d %q, want %d", label, r.status, r.body, c.status)
			continue
		}
		noSecret(t, label, r)
		switch v := r.json(t).(type) {
		case []any:
			if c.status != 400 || len(v) == 0 {
				t.Errorf("%s: a list %s", label, r.body)
			}
		case map[string]any:
			if c.status == 400 || v["message"] == "" || len(v) != 1 {
				t.Errorf("%s: %s", label, r.body)
			}
		default:
			t.Errorf("%s: %s", label, r.body)
		}
	}
	if after, _ := sec.writes(); after != before {
		t.Fatalf("a refusal wrote: %d updates, want %d", after, before)
	}
	if u := sec.Snapshot().Users[0]; u.Status != "active" || len(u.Roles) != 1 {
		t.Fatalf("the last administrator changed: %+v", u)
	}
}

// Each write asks its own permission: the reader and nobody get 403, a
// request without credentials 401, and nothing is written.
func TestIdentityWritesNeedTheirRight(t *testing.T) {
	sec := newFakeSecurity()
	a := newSecureApp(t, sec)
	for _, c := range identityWrites {
		for who, want := range map[string]int{asReader: 403, asNobody: 403, "": 401} {
			var headers []string
			if who != "" {
				headers = []string{"basic", who}
			}
			r := sendWrite(t, a, c, headers...)
			if r.status != want {
				t.Errorf("%s %s as %q: %d %q, want %d", c.method, c.target, who, r.status, r.body, want)
			}
			if want == 403 && !strings.Contains(r.object(t)["message"].(string), "nexus:") {
				t.Errorf("%s %s: %s", c.method, c.target, r.body)
			}
		}
	}
	// the administrator passes each rule
	for _, c := range identityWrites {
		if r := sendWrite(t, a, c, "basic", asAdmin); r.status != c.status {
			t.Fatalf("%s %s as the administrator: %d %q", c.method, c.target, r.status, r.body)
		}
	}
	if updates, _ := sec.writes(); updates != len(identityWrites) {
		t.Fatalf("%d updates, want %d", updates, len(identityWrites))
	}
}

// A write of a cookie session without its request token answers 403 and
// changes nothing; with it the write passes.
func TestIdentityWritesNeedTheRequestToken(t *testing.T) {
	sec := newFakeSecurity()
	a := newSecureApp(t, sec)
	for _, c := range identityWrites {
		for _, token := range []string{"", "csrf-s2", "wrong"} {
			headers := []string{"Cookie", "fake-session=s1"}
			if token != "" {
				headers = append(headers, "X-Pipesmith-Csrf", token)
			}
			r := sendWrite(t, a, c, headers...)
			if r.status != 403 || r.object(t)["message"] != "missing or wrong request token" {
				t.Errorf("%s %s with token %q: %d %q", c.method, c.target, token, r.status, r.body)
			}
		}
	}
	if updates, resets := sec.writes(); updates != 0 || resets != 0 {
		t.Fatalf("a write without its token wrote: %d updates, %d resets", updates, resets)
	}
	for _, c := range identityWrites {
		if r := sendWrite(t, a, c, "Cookie", "fake-session=s1", "X-Pipesmith-Csrf", "csrf-s1"); r.status != c.status {
			t.Fatalf("%s %s with the token: %d %q", c.method, c.target, r.status, r.body)
		}
	}
}

// onlySecurity is a Security without an identity store.
type onlySecurity struct{ model.Security }

func TestIdentityWritesWithoutAStore(t *testing.T) {
	a := startSecured(t, onlySecurity{newFakeSecurity()})
	for _, c := range identityWrites {
		r := do(t, a.addr(), c.method, c.target, []byte(c.body), "basic", asAdmin)
		if r.status != 503 || r.object(t)["message"] == "" {
			t.Errorf("%s %s: %d %q", c.method, c.target, r.status, r.body)
		}
	}
}

const ldapPath = "/service/rest/v1/security/ldap"

// ldapForm is a form of the contract shape with the bind password.
func ldapForm(name, password string) string {
	return `{"name":"` + name + `","protocol":"ldap","useTrustStore":false,"host":"ldap.example.com","port":389,` +
		`"searchBase":"dc=example,dc=com","authScheme":"SIMPLE","authUsername":"uid=demo-bind","authPassword":"` + password + `",` +
		`"connectionTimeoutSeconds":30,"connectionRetryDelaySeconds":300,"maxIncidentsCount":3,"userIdAttribute":"uid",` +
		`"ldapGroupsAsRoles":false,"groupType":"static"}`
}

// The writes of the LDAP connections: each asks its permission and the
// request token, a create answers 201 without a body, and the bind password
// lands in the secret file of the connection and in no answer.
func TestLDAPWritesOfTheServer(t *testing.T) {
	sec := newFakeSecurity()
	sec.dataDir = filepath.Join(t.TempDir(), "data")
	t.Setenv(ldapauth.PasswordVariable("conn-1"), "")
	a := newSecureApp(t, sec)
	writes := []identityRequest{
		{"POST", ldapPath, ldapForm("second", "example-bind-1"), jsonType, 201},
		{"PUT", ldapPath + "/second", ldapForm("second-renamed", "example-bind-2"), jsonType, 204},
		{"POST", ldapPath + "/change-order", `["second-renamed","corp"]`, jsonType, 204},
		{"POST", "/api/ldap/clear-cache", "", "", 204},
		{"DELETE", ldapPath + "/second-renamed", "", "", 204},
	}
	for _, c := range writes {
		for who, want := range map[string]int{asReader: 403, asNobody: 403, "": 401} {
			var headers []string
			if who != "" {
				headers = []string{"basic", who}
			}
			if r := sendWrite(t, a, c, headers...); r.status != want {
				t.Errorf("%s %s as %q: %d %q, want %d", c.method, c.target, who, r.status, r.body, want)
			}
		}
		if r := sendWrite(t, a, c, "Cookie", "fake-session=s1"); r.status != 403 {
			t.Errorf("%s %s without the request token: %d", c.method, c.target, r.status)
		}
	}
	if updates, _ := sec.writes(); updates != 0 {
		t.Fatalf("a refused write wrote: %d updates", updates)
	}
	var id string
	for i, c := range writes {
		r := sendWrite(t, a, c, "basic", asAdmin)
		label := c.method + " " + c.target
		if r.status != c.status || len(r.body) != 0 {
			t.Fatalf("%s: %d %q", label, r.status, r.body)
		}
		for _, secret := range []string{"example-bind-1", "example-bind-2"} {
			if strings.Contains(string(r.body), secret) {
				t.Fatalf("%s: the answer holds the bind password", label)
			}
		}
		if updates, resets := sec.writes(); updates != i+1 || resets != i+1 {
			t.Fatalf("%s: %d updates, %d resets", label, updates, resets)
		}
		if i == 0 {
			id = sec.Snapshot().LDAP[1].ID
			info, err := os.Stat(idstate.LDAPSecretPath(sec.dataDir, id))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("the secret file of %s: %v %v", id, info, err)
			}
		}
	}
	if _, err := os.Stat(idstate.LDAPSecretPath(sec.dataDir, id)); !os.IsNotExist(err) {
		t.Fatalf("the secret file stayed after the delete: %v", err)
	}
	if l := sec.Snapshot().LDAP; len(l) != 1 || l[0].Name != "corp" || l[0].Order != 2 {
		t.Fatalf("after the writes: %+v", l)
	}
}

// The checks of an LDAP form: the permission to create or to save a
// connection, the request token, 200 with ok false for a directory that is
// down, and no write.
func TestLDAPChecksOfTheServer(t *testing.T) {
	sec := newFakeSecurity()
	sec.dataDir = filepath.Join(t.TempDir(), "data")
	sec.basic["demo-creator:creator-pw"] = subject("demo-creator", "", "nexus:ldap:create")
	sec.basic["demo-saver:saver-pw"] = subject("demo-saver", "", "nexus:ldap:update")
	a := newSecureApp(t, sec)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	form := strings.Replace(ldapForm("down", "example-bind-1"), `"host":"ldap.example.com","port":389`,
		`"host":"127.0.0.1","port":`+strconv.Itoa(port), 1)
	checks := []identityRequest{
		{"POST", "/api/ldap/verify-connection", form, jsonType, 200},
		{"POST", "/api/ldap/verify-user-mapping", form, jsonType, 200},
		{"POST", "/api/ldap/verify-login", `{"connection":` + form + `,"username":"demo-ldap-admin","password":"x"}`, jsonType, 200},
	}
	for _, c := range checks {
		for who, want := range map[string]int{asReader: 403, asNobody: 403, "": 401} {
			var headers []string
			if who != "" {
				headers = []string{"basic", who}
			}
			if r := sendWrite(t, a, c, headers...); r.status != want {
				t.Errorf("%s as %q: %d %q, want %d", c.target, who, r.status, r.body, want)
			}
		}
		if r := sendWrite(t, a, c, "Cookie", "fake-session=s1"); r.status != 403 {
			t.Errorf("%s without the request token: %d", c.target, r.status)
		}
		for _, who := range []string{"demo-creator:creator-pw", "demo-saver:saver-pw", asAdmin} {
			r := sendWrite(t, a, c, "basic", who)
			if r.status != 200 || r.header.Get("Content-Type") != jsonType {
				t.Fatalf("%s as %s: %d %q", c.target, who, r.status, r.body)
			}
			m := r.object(t)
			if m["ok"] != false || m["message"] == "" || strings.Contains(string(r.body), "example-bind-1") {
				t.Fatalf("%s as %s: %s", c.target, who, r.body)
			}
		}
	}
	// a form the rules refuse is 400 with the field ids of the contract
	r := sendWrite(t, a, identityRequest{"POST", "/api/ldap/verify-login", `{"connection":{"name":"x"},"username":"u","password":"p"}`, jsonType, 400},
		"basic", asAdmin)
	if r.status != 400 || !strings.Contains(string(r.body), `"id":"connection.host"`) {
		t.Fatalf("a refused form: %d %s", r.status, r.body)
	}
	if updates, resets := sec.writes(); updates != 0 || resets != 0 {
		t.Fatalf("a check wrote: %d updates, %d resets", updates, resets)
	}
}
