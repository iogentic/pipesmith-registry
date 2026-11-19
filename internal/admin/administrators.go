package admin

import (
	"fmt"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/authz"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The full permission set, and the status and source of a user that counts
// as its holder.
const (
	fullPermission = "nexus:*"
	statusActive   = "active"
	sourceLocal    = "default"
)

// Administrators are the users that hold the full permission set, sorted:
// active local users whose roles expand, through internal/authz, to a
// permission that implies nexus:*. Any role that carries such a permission
// counts, not only the built in administrator role.
func Administrators(s *model.Snapshot) []string {
	resolver := authz.NewResolver(s)
	var ids []string
	for _, u := range s.Users {
		if u.Status == statusActive && u.Source == sourceLocal &&
			authz.AnyImplies(resolver.Permissions(u.Roles), fullPermission) {
			ids = append(ids, u.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// CheckAdministrators refuses a change with 409 when the snapshot before it
// holds a user with the full permission set and the snapshot after it holds
// none (decision A10). The one rule covers the delete of a user, a status
// other than active, the loss of a role, and a role save or delete that
// takes the permission away.
func CheckAdministrators(before, after *model.Snapshot) error {
	held := Administrators(before)
	if len(held) == 0 || len(Administrators(after)) > 0 {
		return nil
	}
	return &Refusal{Code: CodeLastAdministrator, Message: fmt.Sprintf(
		"the change leaves no active local user with the full permission set, which %s holds now",
		strings.Join(held, ", "))}
}
