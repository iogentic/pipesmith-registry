package admin

import (
	"fmt"

	"github.com/iogentic/pipesmith-registry/internal/authz"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// rolesField is the field of the contained roles of a role.
const rolesField = "roles"

// CheckRole checks a role create or save: the role is not read only (400
// at *), and every contained role exists and does not contain the role,
// directly or through other roles (400 at roles, naming the role). A
// contained role that the stored role holds already is never refused, so a
// missing role or a cycle that an import brought stays.
func CheckRole(s *model.Snapshot, id string, contained []string) error {
	if err := checkWritable(s, id, "change"); err != nil {
		return err
	}
	stored := make(map[string]model.Role, len(s.Roles))
	for _, r := range s.Roles {
		stored[r.ID] = r
	}
	held := map[string]bool{}
	for _, c := range stored[id].Roles {
		held[c] = true
	}
	resolver := authz.NewResolver(s)
	var list Refusals
	for _, c := range contained {
		switch {
		case held[c]:
		case c == id:
			list = append(list, &Refusal{Code: CodeRoleCycle, Field: rolesField,
				Message: fmt.Sprintf("the role %s cannot contain itself", id)})
		case len(resolver.Roles([]string{c})) == 0:
			list = append(list, &Refusal{Code: CodeRoleMissing, Field: rolesField,
				Message: fmt.Sprintf("the role %s does not exist", c)})
		case roleContains(stored, c, id):
			list = append(list, &Refusal{Code: CodeRoleCycle, Field: rolesField,
				Message: fmt.Sprintf("the role %s contains %s, so %s cannot contain %s", c, id, id, c)})
		}
	}
	return list.err()
}

// CheckRoleDelete refuses the delete of a built in or read only role with
// 400 at *.
func CheckRoleDelete(s *model.Snapshot, id string) error {
	return checkWritable(s, id, "be deleted")
}

// checkWritable refuses a role that is read only: a stored row that says
// so, or a role that only the built in roles of internal/authz hold.
func checkWritable(s *model.Snapshot, id, what string) error {
	for _, r := range s.Roles {
		if r.ID == id {
			if !r.ReadOnly {
				return nil
			}
			return &Refusal{Code: CodeRoleReadOnly, Field: "*",
				Message: fmt.Sprintf("the role %s is read only and cannot %s", id, what)}
		}
	}
	if len(authz.NewResolver(s).Roles([]string{id})) > 0 {
		return &Refusal{Code: CodeRoleReadOnly, Field: "*",
			Message: fmt.Sprintf("the role %s is built in and cannot %s", id, what)}
	}
	return nil
}

// roleContains reports whether the role start contains target through the
// contained roles of the stored rows, at any depth. A built in role
// contains no role; each role is entered once, so a stored cycle ends.
func roleContains(stored map[string]model.Role, start, target string) bool {
	entered := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		r, ok := stored[id]
		if !ok || entered[id] {
			continue
		}
		entered[id] = true
		for _, c := range r.Roles {
			if c == target {
				return true
			}
			stack = append(stack, c)
		}
	}
	return false
}
