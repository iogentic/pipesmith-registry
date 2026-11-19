package admin

import (
	"fmt"
	"slices"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

type identity struct {
	Users      []model.User      `json:"users"`
	Roles      []model.Role      `json:"roles"`
	Privileges []model.Privilege `json:"privileges"`
}

type change struct {
	Op     string      `json:"op"` // delete-user, user-status, user-roles, role-save, role-delete
	User   string      `json:"user"`
	Status string      `json:"status"`
	Roles  []string    `json:"roles"`
	Role   *model.Role `json:"role"`
	ID     string      `json:"id"`
}

func (i identity) snapshot() *model.Snapshot {
	return &model.Snapshot{Users: slices.Clone(i.Users), Roles: slices.Clone(i.Roles),
		Privileges: slices.Clone(i.Privileges)}
}

// apply gives the snapshot after the change; before stays as it is.
func apply(t *testing.T, before *model.Snapshot, c change) *model.Snapshot {
	t.Helper()
	after := &model.Snapshot{Users: slices.Clone(before.Users), Roles: slices.Clone(before.Roles),
		Privileges: before.Privileges}
	user := slices.IndexFunc(after.Users, func(u model.User) bool { return u.ID == c.User })
	role := -1
	switch c.Op {
	case "role-save":
		role = slices.IndexFunc(after.Roles, func(r model.Role) bool { return r.ID == c.Role.ID })
	case "role-delete":
		role = slices.IndexFunc(after.Roles, func(r model.Role) bool { return r.ID == c.ID })
	}
	switch {
	case c.Op == "delete-user" && user >= 0:
		after.Users = slices.Delete(after.Users, user, user+1)
	case c.Op == "user-status" && user >= 0:
		after.Users[user].Status = c.Status
	case c.Op == "user-roles" && user >= 0:
		after.Users[user].Roles = c.Roles
	case c.Op == "role-save" && role >= 0:
		after.Roles[role] = *c.Role
	case c.Op == "role-delete" && role >= 0:
		after.Roles = slices.Delete(after.Roles, role, role+1)
	default:
		t.Fatalf("change %+v names no record of the snapshot", c)
	}
	return after
}

func TestAdministratorVectors(t *testing.T) {
	var file struct {
		Family         string              `json:"family"`
		Note           string              `json:"note"`
		Snapshots      map[string]identity `json:"snapshots"`
		Administrators map[string][]string `json:"administrators"`
		Cases          []struct {
			Row      string         `json:"row"`
			Snapshot string         `json:"snapshot"`
			Change   change         `json:"change"`
			Refusals *[]wantRefusal `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "administrators", &file)
	for name, ident := range file.Snapshots {
		want, listed := file.Administrators[name]
		if got := Administrators(ident.snapshot()); !listed || !slices.Equal(got, want) {
			t.Errorf("snapshot %s: administrators %v, want %v", name, got, want)
		}
	}
	counts, ops := tally{}, tally{}
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s %+v)", i, c.Row, c.Snapshot, c.Change)
		ident, ok := file.Snapshots[c.Snapshot]
		if !ok {
			t.Fatalf("%s: no snapshot %s", label, c.Snapshot)
		}
		before := ident.snapshot()
		after := apply(t, before, c.Change)
		checkRefusals(t, label, CheckAdministrators(before, after), c.Refusals)
		if c.Refusals != nil {
			counts.add(c.Row, len(*c.Refusals) > 0)
			if len(Administrators(before)) > 0 {
				ops.add(c.Change.Op, len(*c.Refusals) > 0)
			}
		}
	}
	counts.check(t, "administrators", "R7")
	ops.check(t, "administrators", "delete-user", "user-status", "user-roles", "role-save", "role-delete")
}

// The check compares the two snapshots it is given and changes neither.
func TestAdministratorsChangeNothing(t *testing.T) {
	before := &model.Snapshot{Users: []model.User{{ID: "demo-admin", Status: "active", Source: "default",
		Roles: []string{"nx-admin"}}}}
	after := &model.Snapshot{}
	if CheckAdministrators(before, after) == nil {
		t.Fatal("the delete of the last administrator passes")
	}
	if CheckAdministrators(before, before) != nil || CheckAdministrators(after, after) != nil {
		t.Error("a change that keeps the administrators, or one with none before, is refused")
	}
	if len(before.Users) != 1 || before.Users[0].Roles[0] != "nx-admin" {
		t.Errorf("the snapshot before changed: %+v", before.Users)
	}
}
