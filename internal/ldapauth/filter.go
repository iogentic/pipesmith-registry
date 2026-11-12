package ldapauth

import (
	"regexp"
	"slices"
	"strings"

	"github.com/go-ldap/ldap/v3"
)

// attributeName is an attribute description of RFC 4512 without options:
// a name or a numeric OID. Settings that go into a filter must match it.
var attributeName = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9-]*|[0-9]+(?:\.[0-9]+)*)$`)

// userFilter finds the entry of one user. The user name is escaped; the
// object class, the attribute and the extra filter are settings that New
// checked.
func userFilter(objectClass, idAttribute, extra, username string) string {
	return "(&" + classFilter(objectClass) + "(" + idAttribute + "=" + ldap.EscapeFilter(username) + ")" +
		extraFilter(extra) + ")"
}

// groupFilter finds the static groups that hold one member value.
func groupFilter(objectClass, memberAttribute, member string) string {
	return "(&" + classFilter(objectClass) + "(" + memberAttribute + "=" + ldap.EscapeFilter(member) + "))"
}

func classFilter(objectClass string) string {
	if objectClass == "" {
		return "(objectClass=*)"
	}
	return "(objectClass=" + objectClass + ")"
}

// extraFilter is the user filter setting with its parentheses, as the old
// product accepts it with and without them.
func extraFilter(extra string) string {
	extra = strings.TrimSpace(extra)
	if extra == "" || strings.HasPrefix(extra, "(") {
		return extra
	}
	return "(" + extra + ")"
}

// memberValue is the value a static group holds for a user: the member
// format with ${dn} and ${username} replaced. The user name is escaped as a
// DN value unless the format is the user name alone.
func memberValue(format, dn, username string) string {
	if format == "${username}" {
		return username
	}
	return strings.NewReplacer("${dn}", dn, "${username}", ldap.EscapeDN(username)).Replace(format)
}

// dynamicGroupNames turns the member of values of a user entry into role
// names (N19): the value of the first part of each DN, and the full DN.
func dynamicGroupNames(values []string) []string {
	names := []string{}
	for _, value := range values {
		names = append(names, value)
		dn, err := ldap.ParseDN(value)
		if err == nil && len(dn.RDNs) > 0 && len(dn.RDNs[0].Attributes) > 0 && dn.RDNs[0].Attributes[0].Value != "" {
			names = append(names, dn.RDNs[0].Attributes[0].Value)
		}
	}
	return sortedUnique(names)
}

func sortedUnique(names []string) []string {
	slices.Sort(names)
	return slices.Compact(names)
}

// joinDN puts a base DN that is relative to the search base below it, as the
// old product reads its user and group base settings.
func joinDN(relative, base string) string {
	relative, base = strings.TrimSpace(relative), strings.TrimSpace(base)
	switch {
	case relative == "":
		return base
	case base == "":
		return relative
	}
	return relative + "," + base
}
