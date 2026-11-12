package authz

import (
	"strings"

	"github.com/mirosso25/pipesmith-registry/internal/model"
)

// Privilege kinds of the old product.
const (
	KindWildcard        = "wildcard"
	KindApplication     = "application"
	KindRepositoryView  = "repository-view"
	KindRepositoryAdmin = "repository-admin"
	KindContentSelector = "repository-content-selector"
	KindScript          = "script"
)

const all = "*"

// PermissionOf builds the permission string of a stored privilege. It
// reports false for a privilege that grants nothing: a content selector
// (decision N14), an unknown kind, a wildcard without a pattern, and a value
// that would change the parts of the string.
//
// As in the old product a missing or empty property is *, the actions are
// split at ',' and trimmed, and the string is lower cased. The old product
// builds typed permissions part by part, so a ',' or ':' inside a format,
// repository or script name stays one value there; a string cannot say
// that, and such a row grants nothing here.
func PermissionOf(p model.Privilege) (string, bool) {
	prop := func(key string) string {
		if v := p.Properties[key]; v != "" {
			return v
		}
		return all
	}
	switch p.Type {
	case KindWildcard:
		perm, err := Parse(p.Properties["pattern"])
		if err != nil {
			return "", false
		}
		return perm.String(), true
	case KindApplication:
		return typed([]string{"nexus"}, strings.Split(prop("domain"), ":"), prop("actions"))
	case KindRepositoryView, KindRepositoryAdmin:
		return typed([]string{"nexus", p.Type}, []string{prop("format"), prop("repository")}, prop("actions"))
	case KindScript:
		name := p.Properties["name"]
		if name == "" {
			name = prop("scriptName")
		}
		return typed([]string{"nexus", "script"}, []string{name}, prop("actions"))
	}
	return "", false
}

// typed joins the fixed parts, the value parts and the action list. A value
// with ',' or ':' and an action with ':' refuse the row; an action list of
// only commas grants nothing, as an empty part matches nothing in Shiro.
func typed(fixed, values []string, actions string) (string, bool) {
	for _, v := range values {
		if strings.ContainsAny(v, ",:") {
			return "", false
		}
	}
	var list []string
	for _, a := range strings.Split(actions, ",") {
		if a = javaTrim(a); a != "" {
			if strings.Contains(a, ":") {
				return "", false
			}
			list = append(list, a)
		}
	}
	if len(list) == 0 {
		return "", false
	}
	parts := append(append(fixed[:len(fixed):len(fixed)], values...), strings.Join(list, ","))
	return strings.ToLower(strings.Join(parts, ":")), true
}
