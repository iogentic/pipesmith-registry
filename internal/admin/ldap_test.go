package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/authz"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

func TestLDAPVectors(t *testing.T) {
	var file struct {
		Family string         `json:"family"`
		Note   string         `json:"note"`
		Base   map[string]any `json:"base"`
		Stored []string       `json:"stored"`
		Cases  []struct {
			Row      string         `json:"row"`
			Op       string         `json:"op"`
			Prefix   string         `json:"prefix"`
			Set      map[string]any `json:"set"`
			Drop     []string       `json:"drop"`
			Scheme   string         `json:"scheme"`
			Create   bool           `json:"create"`
			Password *string        `json:"password"`
			Repeat   int            `json:"repeat"`
			Held     []string       `json:"held"`
			Changed  *bool          `json:"changed"`
			Names    []string       `json:"names"`
			Refusals *[]wantRefusal `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "ldap", &file)
	counts := tally{}
	alone := map[string]bool{} // the fields a case of op changed changes alone
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s)", i, c.Row, c.Op)
		if c.Op == "changed" && len(c.Set) == 1 && len(c.Drop) == 0 {
			for key := range c.Set {
				alone[key] = true
			}
		}
		body := maps.Clone(file.Base)
		maps.Copy(body, c.Set)
		for _, key := range c.Drop {
			delete(body, key)
		}
		var err error
		switch c.Op {
		case "form":
			var server model.LDAPServer
			server, err = LDAPConnection(body, c.Prefix)
			if err == nil && (server.ID != "" || server.Order != 0 || server.CAFile != "" || server.Name != body["name"]) {
				t.Errorf("%s: the connection read %+v", label, server)
			}
		case "password":
			password := c.Password
			if password != nil && c.Repeat > 0 {
				long := strings.Repeat(*password, c.Repeat)
				password = &long
			}
			err = CheckLDAPPassword(c.Scheme, password, c.Create, c.Prefix)
		case "grant":
			err = CheckLDAPGrant("the create of the LDAP connection demo-corp", func(p string) bool { return authz.AnyImplies(c.Held, p) })
		case "changed":
			stored, serr := LDAPConnection(file.Base, "")
			saved, berr := LDAPConnection(body, "")
			if serr != nil || berr != nil || c.Changed == nil {
				t.Fatalf("%s: %v %v", label, serr, berr)
			}
			if got := LDAPDirectoryChanged(stored, saved); got != *c.Changed {
				t.Errorf("%s: changed %v, want %v", label, got, *c.Changed)
			}
			counts.add(c.Row, *c.Changed)
			continue
		case "order":
			s := &model.Snapshot{}
			for i, name := range file.Stored {
				s.LDAP = append(s.LDAP, model.LDAPServer{ID: fmt.Sprint("id-", i), Name: name, Order: i + 1})
			}
			if err = CheckLDAPOrder(s, c.Names); err == nil {
				if !SetLDAPOrder(s, c.Names) {
					t.Errorf("%s: the order did not change", label)
				}
				for _, l := range s.LDAP {
					if l.Order < 1 || l.Order > len(c.Names) || c.Names[l.Order-1] != l.Name {
						t.Errorf("%s: %s has the order %d", label, l.Name, l.Order)
					}
				}
				if SetLDAPOrder(s, c.Names) {
					t.Errorf("%s: the same order again changed it", label)
				}
			}
		default:
			t.Fatalf("%s: unknown op", label)
		}
		checkRefusals(t, label, err, c.Refusals)
		if c.Refusals != nil {
			counts.add(c.Row, len(*c.Refusals) > 0)
		}
	}
	counts.check(t, "ldap", "S27", "I18", "I17", "S26", "I27")
	// every field that decides the directory has a case that changes it alone
	for _, key := range ldapDirectoryFields {
		if !alone[key] {
			t.Errorf("ldap.json: no case of op changed changes %s alone", key)
		}
	}
}

// The form never takes the stored id, order or CA file from a body, and it
// reads every field of the contract shape.
func TestLDAPConnectionReadsTheContractShape(t *testing.T) {
	want := model.LDAPServer{Name: "demo-corp", Protocol: "ldaps", Host: "ldap.example.com", Port: 636,
		SearchBase: "dc=example,dc=com", AuthScheme: "NONE", ConnectionTimeoutSeconds: 5, ConnectionRetryDelaySeconds: 60,
		MaxIncidentsCount: 2, UserBaseDN: "ou=people", UserSubtree: true, UserObjectClass: "inetOrgPerson", UserIDAttribute: "uid",
		LDAPGroupsAsRoles: true, GroupType: "dynamic", UserMemberOfAttribute: "memberOf"}
	body := fieldsOf(want)
	body["id"], body["order"], body["caFile"] = "x", 4, "/tmp/ca.pem"
	got, err := LDAPConnection(body, "")
	if err != nil || got != want {
		t.Fatalf("read %+v, %v; want %+v", got, err, want)
	}
}

func TestLDAPPasswordField(t *testing.T) {
	for body, want := range map[string]*string{`{}`: nil, `{"authPassword":null}`: nil} {
		got, err := LDAPPassword(decodeObject(t, body), "")
		if err != nil || got != want {
			t.Errorf("%s: %v %v", body, got, err)
		}
	}
	got, err := LDAPPassword(map[string]any{"authPassword": ""}, "")
	if err != nil || got == nil || *got != "" {
		t.Errorf("an empty string: %v %v", got, err)
	}
	_, err = LDAPPassword(map[string]any{"authPassword": 7.0}, "connection.")
	if status, body := Answer(err); status != 400 || fmt.Sprint(body) != "[{connection.authPassword the value of authPassword is no string}]" {
		t.Errorf("a number: %d %v", status, body)
	}
}

func decodeObject(t *testing.T, text string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNewLDAPID(t *testing.T) {
	a, err := NewLDAPID()
	b, err2 := NewLDAPID()
	if err != nil || err2 != nil || a == b || len(a) != 36 || a[14] != '4' || strings.Count(a, "-") != 4 {
		t.Fatalf("%q %q %v %v", a, b, err, err2)
	}
	s := &model.Snapshot{LDAP: []model.LDAPServer{{Order: 3}, {Order: 1}}}
	if NextLDAPOrder(s) != 4 || NextLDAPOrder(&model.Snapshot{}) != 1 {
		t.Fatal("the order of a new connection")
	}
}

// The secret file of a connection: written with mode 0600, read back as
// the server binds with it, removed, and each step undone.
func TestLDAPSecretFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	t.Setenv(ldapauth.PasswordVariable("demo-1"), "")
	path := state.LDAPSecretPath(dir, "demo-1")
	if got, err := LDAPSecret(dir, "demo-1"); got != "" || err != nil {
		t.Fatalf("no file: %q %v", got, err)
	}
	undo, err := WriteLDAPSecret(dir, "demo-1", "example-bind-1")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the file: %v %v", info, err)
	}
	if got, _ := LDAPSecret(dir, "demo-1"); got != "example-bind-1" {
		t.Fatalf("read %q", got)
	}
	undo()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("undo of the first write left the file: %v", err)
	}
	if _, err := WriteLDAPSecret(dir, "demo-1", "example-bind-1"); err != nil {
		t.Fatal(err)
	}
	undo, _ = WriteLDAPSecret(dir, "demo-1", "example-bind-2")
	undo()
	if got, _ := LDAPSecret(dir, "demo-1"); got != "example-bind-1" {
		t.Fatalf("undo of a replace: %q", got)
	}
	// the variable wins, as serve takes it first
	t.Setenv(ldapauth.PasswordVariable("demo-1"), "example-bind-env")
	if got, _ := LDAPSecret(dir, "demo-1"); got != "example-bind-env" {
		t.Fatalf("with the variable: %q", got)
	}
	t.Setenv(ldapauth.PasswordVariable("demo-1"), "")
	undo, err = RemoveLDAPSecret(dir, "demo-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the remove left the file: %v", err)
	}
	undo()
	if got, _ := LDAPSecret(dir, "demo-1"); got != "example-bind-1" {
		t.Fatalf("undo of the remove: %q", got)
	}
	if _, err := RemoveLDAPSecret(dir, "demo-2"); err != nil {
		t.Fatalf("no file to remove: %v", err)
	}
	if _, err := WriteLDAPSecret("", "demo-1", "x"); err == nil {
		t.Fatal("a write without a data directory passed")
	}
}

func TestLDAPOrderChanged(t *testing.T) {
	s := &model.Snapshot{LDAP: []model.LDAPServer{{Name: "b", Order: 7}, {Name: "a", Order: 2}, {Name: "c", Order: 7}}}
	for names, want := range map[string]bool{"a b c": false, "a c b": true, "b a c": true} {
		if got := LDAPOrderChanged(s, strings.Fields(names)); got != want {
			t.Errorf("%s: %v, want %v", names, got, want)
		}
	}
}
