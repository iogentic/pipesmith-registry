package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Review 3-60: every connection of the protocol ldap sends the passwords of
// the users who sign in through it in clear, with or without a service
// bind, and the build says so once. ldaps gives no such line.
func TestLDAPWarningForEveryClearConnection(t *testing.T) {
	dir := importedFixture(t)
	snap, _ := stateOf(t, dir)
	base := snap.LDAP[0]
	base.Protocol, base.AuthScheme = "ldap", "NONE"
	tls := base
	tls.Protocol = "ldaps"
	tls.Port = 636
	for name, c := range map[string]struct {
		server model.LDAPServer
		warn   bool
	}{
		"ldap without a service bind": {base, true},
		"ldaps":                       {tls, false},
	} {
		var log bytes.Buffer
		b := newLDAPBuilds(dir, logTo(&log))
		dirs, err := b.build([]model.LDAPServer{c.server}, nil)
		if err != nil || len(dirs) != 1 {
			t.Fatalf("%s: %d directories, %v, log %q", name, len(dirs), err, log.String())
		}
		got := strings.Count(log.String(), "LDAP connection demo-directory sends the passwords of its users in clear")
		if c.warn && got != 1 || !c.warn && got != 0 {
			t.Errorf("%s: %d warnings, log %q", name, got, log.String())
		}
		// a reload of the same row logs nothing again
		log.Reset()
		b.directories([]model.LDAPServer{c.server})
		if log.Len() != 0 {
			t.Errorf("%s: the reload logged %q", name, log.String())
		}
	}
}
