package admin

import (
	"fmt"
	"slices"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

func TestUserVectors(t *testing.T) {
	var file struct {
		Family    string          `json:"family"`
		Note      string          `json:"note"`
		Users     []model.User    `json:"users"`
		Anonymous model.Anonymous `json:"anonymous"`
		Cases     []struct {
			Row       string           `json:"row"`
			Op        string           `json:"op"` // delete, account, password, admin-password, status
			Actor     *model.User      `json:"actor"`
			User      string           `json:"user"`
			Source    *string          `json:"source"`
			Anonymous *model.Anonymous `json:"anonymous"`
			Status    string           `json:"status"`
			Refusals  *[]wantRefusal   `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "users", &file)
	counts := tally{}
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s %s %+v)", i, c.Row, c.Op, c.User, c.Actor)
		var user *model.User
		if c.User != "" {
			found := slices.IndexFunc(file.Users, func(u model.User) bool {
				return u.ID == c.User && (c.Source == nil || u.Source == *c.Source)
			})
			if found < 0 {
				t.Fatalf("%s: the file holds no such user", label)
			}
			user = &file.Users[found]
		}
		s := &model.Snapshot{Users: slices.Clone(file.Users), Anonymous: file.Anonymous}
		if c.Anonymous != nil {
			s.Anonymous = *c.Anonymous
		}
		var err error
		switch {
		case c.Op == "delete" && user != nil:
			var actor *model.Subject
			if c.Actor != nil {
				actor = &model.Subject{User: *c.Actor}
			}
			err = CheckUserDelete(s, actor, *user)
		case c.Op == "account" && user != nil:
			err = CheckAccountSave(*user)
		case c.Op == "password" && user != nil:
			err = CheckPasswordChange(*user)
		case c.Op == "admin-password" && user != nil && c.Actor != nil:
			err = CheckPasswordChange(*user)
		case c.Op == "status":
			err = CheckUserStatus(c.Status, user)
		default:
			t.Fatalf("%s: unknown op, or no user", label)
		}
		checkRefusals(t, label, err, c.Refusals)
		if c.Refusals != nil {
			counts.add(c.Row, len(*c.Refusals) > 0)
		}
	}
	counts.check(t, "users", "Q5", "Q6", "Q7", "Q18")
}
