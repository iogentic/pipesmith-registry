package ldapauth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
)

// Review 3-40: a lookup refuses an entry the directory marks disabled or
// locked, and names the entry by its entryUUID, else its objectGUID, else
// its DN. A lockout of Active Directory whose duration ended, which the
// computed control shows, does not refuse.
func TestLookupRefusesALockedEntryAndNamesIt(t *testing.T) {
	f := newFakeServer(t)
	guid := string([]byte{0x01, 0x02, 0xfe, 0xff})
	for uid, attrs := range map[string]map[string][]string{
		"demo-disabled":      {"userAccountControl": {"514"}},
		"demo-locked-ad":     {"userAccountControl": {"512"}, "lockoutTime": {"133000000000000000"}},
		"demo-locked-comp":   {"msDS-User-Account-Control-Computed": {"16"}, "lockoutTime": {"0"}},
		"demo-locked-pp":     {"pwdAccountLockedTime": {"000001010000Z"}},
		"demo-active-ad":     {"userAccountControl": {"512"}, "lockoutTime": {"0"}, "objectGUID": {guid}},
		"demo-lock-ended":    {"msDS-User-Account-Control-Computed": {"0"}, "lockoutTime": {"133000000000000000"}},
		"demo-uuid":          {"entryUUID": {"5F3A0C1E-0000-4000-8000-00000000AB12"}, "objectGUID": {guid}},
		"demo-plain-openldp": {},
	} {
		f.put(uid, attrs)
	}
	d := fakeDirectoryOf(t, f, nil)
	for _, uid := range []string{"demo-disabled", "demo-locked-ad", "demo-locked-comp", "demo-locked-pp"} {
		if u, _, err := d.Lookup(context.Background(), uid); !errors.Is(err, ldapauth.ErrNoEntry) {
			t.Errorf("%s: %+v %v, want ErrNoEntry", uid, u, err)
		}
	}
	for uid, entry := range map[string]string{
		"demo-active-ad":     "objectGUID:0102feff",
		"demo-lock-ended":    "dn:uid=demo-lock-ended,ou=people,dc=example,dc=com",
		"demo-uuid":          "entryUUID:5f3a0c1e-0000-4000-8000-00000000ab12",
		"demo-plain-openldp": "dn:uid=demo-plain-openldp,ou=people,dc=example,dc=com",
	} {
		u, _, err := d.Lookup(context.Background(), uid)
		if err != nil || u.Entry != entry {
			t.Errorf("%s: entry %q %v, want %q", uid, u.Entry, err, entry)
		}
		if u, _, err := d.Login(context.Background(), uid, "demo-any-pass"); err != nil || u.Entry != entry {
			t.Errorf("%s: sign in entry %q %v, want %q", uid, u.Entry, err, entry)
		}
	}
}
