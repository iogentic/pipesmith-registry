package auth_test

import (
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Review 3-63, point b: role mappings match a user id without regard to
// letter case, as the old product looks them up by a lower case copy of the
// id (lab research 05-identity.md, USER_LO). Two mappings of one source
// whose ids differ only in case both apply, and the load warns about them.
func TestRoleMappingsThatDifferOnlyInCaseWarn(t *testing.T) {
	snap := fixture(t)
	snap.RoleMappings = append(snap.RoleMappings, model.RoleMapping{UserID: "demo-ldap-person", Source: "LDAP", Roles: []string{"demo-maven-reader"}})
	e := newEnv(t, snap, ldapDirectory())
	s, err := subjectOf(e, basic(request("GET", "/", nil), "demo-ldap-person", ldapPassword))
	if err != nil || s == nil {
		t.Fatalf("sign in: %+v %v", s, err)
	}
	for _, role := range []string{"demo-ops", "demo-maven-reader"} {
		if !strings.Contains(strings.Join(s.User.Roles, ","), role) {
			t.Errorf("roles %v miss %s of a mapping that differs only in case", s.User.Roles, role)
		}
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "role mappings differ only in letter case") || !strings.Contains(logs, "DEMO-LDAP-PERSON") ||
		!strings.Contains(logs, "demo-ldap-person") {
		t.Fatalf("no warning about the two mappings: %q", logs)
	}
	quiet := newEnv(t, fixture(t), ldapDirectory())
	if strings.Contains(quiet.logs.String(), "letter case") {
		t.Fatalf("a warning without such mappings: %q", quiet.logs.String())
	}
}
