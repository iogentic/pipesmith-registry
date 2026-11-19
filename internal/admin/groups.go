package admin

import (
	"fmt"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// membersField is the field of the members of a group.
const membersField = "group.memberNames"

// CheckMembers checks the members of a group create or save: every member
// exists, has the format of the group, and does not contain the group,
// directly or through other groups. Groups inside groups are allowed. Each
// refusal is a 400 at group.memberNames that names the member. A member
// that the stored group holds already is never refused.
func CheckMembers(s *model.Snapshot, group, format string, members []string) error {
	byName := make(map[string]model.Repository, len(s.Repositories))
	for _, r := range s.Repositories {
		byName[r.Name] = r
	}
	held := map[string]bool{}
	if stored, ok := byName[group]; ok {
		for _, m := range memberNames(stored) {
			held[m] = true
		}
	}
	var list Refusals
	for _, m := range members {
		member, exists := byName[m]
		switch {
		case held[m]:
		case m == group:
			list = append(list, &Refusal{Code: CodeMemberCycle, Field: membersField,
				Message: fmt.Sprintf("the group %s cannot contain itself", group)})
		case !exists:
			list = append(list, &Refusal{Code: CodeMemberMissing, Field: membersField,
				Message: fmt.Sprintf("the repository %s does not exist", m)})
		case member.Format != format:
			list = append(list, &Refusal{Code: CodeMemberFormat, Field: membersField,
				Message: fmt.Sprintf("the repository %s has the format %s, not %s", m, member.Format, format)})
		case groupContains(byName, m, group):
			list = append(list, &Refusal{Code: CodeMemberCycle, Field: membersField,
				Message: fmt.Sprintf("the group %s contains %s, so %s cannot contain %s", m, group, group, m)})
		}
	}
	return list.err()
}

// groupContains reports whether the repository start contains target
// through its stored members, at any depth. Each group is entered once, so
// a stored cycle ends.
func groupContains(byName map[string]model.Repository, start, target string) bool {
	entered := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		name := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		r, ok := byName[name]
		if !ok || r.Type != "group" || entered[name] {
			continue
		}
		entered[name] = true
		for _, m := range memberNames(r) {
			if m == target {
				return true
			}
			stack = append(stack, m)
		}
	}
	return false
}

// memberNames are the members a repository stores in the attribute
// group.memberNames, as the import reads them.
func memberNames(r model.Repository) []string {
	group, _ := r.Attributes["group"].(map[string]any)
	return stringList(group["memberNames"])
}

// stringList gives the strings of a decoded list, or of a list of strings.
// Other items are left out.
func stringList(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		var out []string
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
