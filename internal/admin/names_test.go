package admin

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

type nameCase struct {
	Row      string         `json:"row"`
	Op       string         `json:"op"` // pattern, create, save, ldap-save
	Kind     Kind           `json:"kind"`
	Name     string         `json:"name"`
	Repeat   int            `json:"repeat"`
	Existing []string       `json:"existing"`
	Stored   string         `json:"stored"`
	Sent     *string        `json:"sent"`
	SentID   *string        `json:"sentId"`
	Refusals *[]wantRefusal `json:"refusals"`
}

// holding is a snapshot with a record of the kind for each name.
func holding(kind Kind, names []string) *model.Snapshot {
	s := &model.Snapshot{}
	for _, n := range names {
		switch kind {
		case KindRepository:
			s.Repositories = append(s.Repositories, model.Repository{Name: n, Format: "raw", Type: "hosted"})
		case KindBlobStore:
			s.BlobStores = append(s.BlobStores, model.BlobStore{Name: n, Type: "File", Path: "/data/" + n})
		case KindLDAPConnection:
			s.LDAP = append(s.LDAP, model.LDAPServer{ID: "id-" + n, Name: n})
		case KindRole:
			s.Roles = append(s.Roles, model.Role{ID: n, Name: n, Source: "default"})
		case KindUser:
			s.Users = append(s.Users, model.User{ID: n, Status: "active", Source: "default"})
		}
	}
	return s
}

// The kinds whose records the snapshot holds; the others are checked
// against a list of names.
var snapshotKinds = []Kind{KindRepository, KindBlobStore, KindLDAPConnection, KindRole, KindUser}

func TestNameVectors(t *testing.T) {
	var file struct {
		Family string     `json:"family"`
		Note   string     `json:"note"`
		Cases  []nameCase `json:"cases"`
	}
	readJSON(t, "names", &file)
	counts := tally{}
	for i, c := range file.Cases {
		name := c.Name
		if c.Repeat > 0 {
			name = strings.Repeat(c.Name, c.Repeat)
		}
		label := fmt.Sprintf("case %d (%s %s %s %.20q)", i, c.Row, c.Op, c.Kind, name)
		var err error
		switch c.Op {
		case "pattern":
			err = CheckName(c.Kind, name)
		case "create":
			exists := slices.Contains(c.Existing, name)
			if slices.Contains(snapshotKinds, c.Kind) {
				exists = Exists(holding(c.Kind, c.Existing), c.Kind, name)
			}
			err = CheckCreate(c.Kind, name, exists)
		case "save":
			err = CheckRename(c.Kind, c.Stored, c.Sent)
		case "ldap-save":
			s := holding(KindLDAPConnection, c.Existing)
			i := slices.IndexFunc(s.LDAP, func(l model.LDAPServer) bool { return l.Name == c.Stored })
			if i < 0 {
				t.Fatalf("%s: the snapshot holds no connection %q", label, c.Stored)
			}
			err = CheckLDAPSave(s, s.LDAP[i], c.SentID, c.Sent)
		default:
			t.Fatalf("%s: unknown op", label)
		}
		checkRefusals(t, label, err, c.Refusals)
		if c.Refusals != nil {
			counts.add(c.Row, len(*c.Refusals) > 0)
		}
	}
	counts.check(t, "names", "R1", "R2")
}

// Every kind of record with a name has a field and a noun; a role has an
// id, a user a userId.
func TestNameFields(t *testing.T) {
	want := map[Kind]string{KindRepository: "name", KindBlobStore: "name", KindCleanupPolicy: "name",
		KindRoutingRule: "name", KindLDAPConnection: "name", KindRole: "id", KindUser: "userId"}
	for kind, field := range want {
		if got := NameField(kind); got != field {
			t.Errorf("%s: field %q, want %q", kind, got, field)
		}
		if kind.noun() == "" || strings.Contains(kind.noun(), "-") {
			t.Errorf("%s: noun %q", kind, kind.noun())
		}
	}
}

// A role the snapshot does not store but the resolver builds in exists; a
// kind the snapshot does not hold never does.
func TestExists(t *testing.T) {
	empty := &model.Snapshot{}
	if !Exists(empty, KindRole, "nx-admin") || Exists(empty, KindRole, "demo-ops") {
		t.Error("a built in role exists, an unknown one does not")
	}
	s := holding(KindRepository, []string{"demo-raw"})
	if !Exists(s, KindRepository, "demo-raw") || Exists(s, KindCleanupPolicy, "demo-raw") || Exists(s, KindBlobStore, "demo-raw") {
		t.Error("a repository exists only as a repository")
	}
}
