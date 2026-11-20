package ldapauth

import (
	"encoding/hex"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/go-ldap/ldap/v3"
)

// attributeName is an attribute description of RFC 4512 without options:
// a name or a numeric OID. Settings that go into a filter must match it.
var attributeName = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9-]*|[0-9]+(?:\.[0-9]+)*)$`)

// memberAttribute is an attribute name, optionally with the dotted OID of a
// matching rule and a closing colon, as in "member:1.2.840.113556.1.4.1941:"
// (the in chain rule of Active Directory, which follows nested groups). The
// group filter appends "=" and the escaped member value.
var memberAttribute = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9-]*|[0-9]+(?:\.[0-9]+)*)(?::[0-9]+(?:\.[0-9]+)+:)?$`)

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
// With a group base, a value that is no DN below it names nothing (review
// 3-61), so a group of the same name elsewhere in the directory maps to no
// role; letter case of the base does not count.
func dynamicGroupNames(values []string, base *ldap.DN) []string {
	names := []string{}
	for _, value := range values {
		dn, err := ldap.ParseDN(value)
		if base != nil && (err != nil || !base.AncestorOfFold(dn)) {
			continue
		}
		names = append(names, value)
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

// entryAttributes are the attributes a user search asks for besides the
// mapped ones: the stable id of the entry and the marks of a disabled or
// locked account of Active Directory and of the password policy of
// OpenLDAP (review 3-40). A server ignores a name it does not know.
var entryAttributes = []string{"entryUUID", "objectGUID", "userAccountControl", "lockoutTime",
	"msDS-User-Account-Control-Computed", "pwdAccountLockedTime"}

// Bits of userAccountControl and of its computed form in Active Directory.
const (
	accountDisable = 0x2
	accountLockout = 0x10
)

// lockedEntry reports an entry the directory marks disabled or locked:
// ACCOUNTDISABLE in userAccountControl; a lockout in the computed control of
// Active Directory, which ends with the lockout duration, else a lockoutTime
// above 0 when the computed control is missing; pwdAccountLockedTime of the
// OpenLDAP password policy. Login does not ask: the bind of a locked
// account fails there, and a bind that the lockout no longer refuses clears
// lockoutTime.
func lockedEntry(entry *ldap.Entry) bool {
	number := func(attribute string) (int64, bool) {
		n, err := strconv.ParseInt(strings.TrimSpace(entry.GetEqualFoldAttributeValue(attribute)), 10, 64)
		return n, err == nil
	}
	if uac, ok := number("userAccountControl"); ok && uac&accountDisable != 0 {
		return true
	}
	if computed, ok := number("msDS-User-Account-Control-Computed"); ok {
		if computed&accountLockout != 0 {
			return true
		}
	} else if at, ok := number("lockoutTime"); ok && at > 0 {
		return true
	}
	return len(entry.GetEqualFoldAttributeValues("pwdAccountLockedTime")) > 0
}

// entryID names an entry for the binding of a token: its entryUUID, else
// its objectGUID in hex, else its DN in lower case.
func entryID(entry *ldap.Entry) string {
	if v := strings.TrimSpace(entry.GetEqualFoldAttributeValue("entryUUID")); v != "" {
		return "entryUUID:" + strings.ToLower(v)
	}
	if v := entry.GetEqualFoldRawAttributeValue("objectGUID"); len(v) > 0 {
		return "objectGUID:" + hex.EncodeToString(v)
	}
	if dn, err := ldap.ParseDN(entry.DN); err == nil {
		return "dn:" + strings.ToLower(dn.String())
	}
	return "dn:" + strings.ToLower(strings.TrimSpace(entry.DN))
}
