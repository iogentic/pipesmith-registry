package admin

import (
	"fmt"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

func TestRoleVectors(t *testing.T) {
	var file struct {
		Family string       `json:"family"`
		Note   string       `json:"note"`
		Roles  []model.Role `json:"roles"`
		Cases  []struct {
			Row      string         `json:"row"`
			Op       string         `json:"op"` // create, save, delete
			ID       string         `json:"id"`
			Roles    []string       `json:"roles"`
			Refusals *[]wantRefusal `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "roles", &file)
	s := &model.Snapshot{Roles: file.Roles}
	counts := tally{}
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s %s %v)", i, c.Row, c.Op, c.ID, c.Roles)
		var err error
		switch c.Op {
		case "create", "save":
			if Exists(s, KindRole, c.ID) != (c.Op == "save") {
				t.Fatalf("%s: a create names a new role, a save a stored or built in one", label)
			}
			err = returns(t, label, func() error { return CheckRole(s, c.ID, c.Roles) })
		case "delete":
			err = CheckRoleDelete(s, c.ID)
		default:
			t.Fatalf("%s: unknown op", label)
		}
		checkRefusals(t, label, err, c.Refusals)
		if c.Refusals != nil {
			counts.add(c.Row, len(*c.Refusals) > 0)
		}
	}
	counts.check(t, "roles", "R5")
}

// A stored row of the id of a built in role decides: one that is not read
// only can change.
func TestStoredRowOfABuiltInRole(t *testing.T) {
	s := &model.Snapshot{Roles: []model.Role{{ID: "nx-anonymous", Source: "default"}}}
	if err := CheckRole(s, "nx-anonymous", nil); err != nil {
		t.Errorf("stored row, not read only: %v", err)
	}
	if err := CheckRole(&model.Snapshot{}, "nx-anonymous", nil); err == nil {
		t.Error("built in role without a stored row: no refusal")
	}
}
