package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
)

const authorizingNote = "note: the realm NexusAuthorizingRealm left the order; it has no effect on this server"

// Owner choice 3-26: realms set that takes the authorizing realm of the old
// product out of the stored order notes once that the realm has no effect,
// since roles resolve without it. An order that keeps it, or that did not
// hold it before, gives no note. The order is written either way.
func TestRealmsSetNotesTheAuthorizingRealm(t *testing.T) {
	dir := importedFixture(t)
	if snap, _ := stateOf(t, dir); !slices.Contains(snap.Realms, auth.RealmAuthorizing) {
		t.Fatalf("the fixture order %v lacks %s", snap.Realms, auth.RealmAuthorizing)
	}
	code, out, errOut := withInput(t, "", "realms", "set", "NexusAuthenticatingRealm,LdapRealm")
	if code != 0 || strings.Count(out, authorizingNote) != 1 || !strings.Contains(out, "roles resolve without it") {
		t.Fatalf("realms set without the authorizing realm: %d %q %q", code, out, errOut)
	}
	if snap, _ := stateOf(t, dir); slices.Contains(snap.Realms, auth.RealmAuthorizing) {
		t.Fatalf("order %v after realms set", snap.Realms)
	}
	for _, list := range []string{"NexusAuthenticatingRealm", "NexusAuthenticatingRealm,NexusAuthorizingRealm"} {
		if code, out, errOut := withInput(t, "", "realms", "set", list); code != 0 || strings.Contains(out, "NexusAuthorizingRealm left") {
			t.Fatalf("realms set %s: %d %q %q", list, code, out, errOut)
		}
	}
}
