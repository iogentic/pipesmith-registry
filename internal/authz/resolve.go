package authz

import (
	"slices"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Resolver expands role ids to permission strings against one snapshot. It
// is safe for concurrent use; it never changes after NewResolver.
type Resolver struct {
	roles      map[string]model.Role
	privileges map[string]string // privilege id to its permission string; absent when it grants nothing
	ignored    map[string]int
}

// NewResolver builds the roles and privileges of a snapshot: the stored rows,
// and the built in and generated ones where no stored row has the same id
// (decision N15).
func NewResolver(s *model.Snapshot) *Resolver {
	r := &Resolver{roles: map[string]model.Role{}, privileges: map[string]string{}, ignored: map[string]int{}}
	stored := map[string]bool{}
	for _, p := range s.Privileges {
		stored[p.Name] = true
		if perm, ok := PermissionOf(p); ok {
			r.privileges[p.Name] = perm
		} else {
			r.ignored[p.Type]++
		}
	}
	for _, p := range append(builtinPrivileges(), generatedPrivileges(s.Repositories)...) {
		if stored[p.Name] {
			continue
		}
		if perm, ok := PermissionOf(p); ok {
			r.privileges[p.Name] = perm
		}
	}
	for _, role := range builtinRoles() {
		r.roles[role.ID] = role
	}
	for _, role := range s.Roles {
		r.roles[role.ID] = role
	}
	return r
}

// Roles is the set of role ids reached from roles through contained roles,
// sorted, the given ones included when they exist.
func (r *Resolver) Roles(roles []string) []string {
	seen := map[string]bool{}
	queue := slices.Clone(roles)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		role, ok := r.roles[id]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		queue = append(queue, role.Roles...)
	}
	list := make([]string, 0, len(seen))
	for id := range seen {
		list = append(list, id)
	}
	slices.Sort(list)
	return list
}

// Permissions expands the roles, their contained roles and their privileges
// to permission strings, sorted and without repeats. A missing role or
// privilege is ignored and a cycle ends at the first repeat.
func (r *Resolver) Permissions(roles []string) []string {
	list := []string{}
	for _, id := range r.Roles(roles) {
		for _, p := range r.roles[id].Privileges {
			if perm, ok := r.privileges[p]; ok {
				list = append(list, perm)
			}
		}
	}
	slices.Sort(list)
	return slices.Compact(list)
}

// Ignored counts the stored privileges that grant nothing, by kind:
// content selectors and unknown kinds, and rows whose values would change
// the parts of the string.
func (r *Resolver) Ignored() map[string]int {
	counts := make(map[string]int, len(r.ignored))
	for k, v := range r.ignored {
		counts[k] = v
	}
	return counts
}
