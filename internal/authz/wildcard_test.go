package authz_test

import (
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/authz"
)

// The cases follow Shiro 1.13.0 WildcardPermission (setParts and implies)
// and the examples of the interface contract, section Permissions.
var impliesCases = []struct {
	granted, requested string
	want               bool
}{
	// fewer parts: the missing trailing parts count as *
	{"nexus:*", "nexus:repository-view:maven2:demo-maven:read", true},
	{"nexus", "nexus:settings:read", true},
	{"nexus:repository-view", "nexus:repository-view:maven2:demo-maven:read", true},
	{"nexus:repository-view:maven2", "nexus:repository-view:maven2:demo-maven:read", true},
	{"*", "nexus:settings:read", true},
	// equal parts
	{"nexus:settings:read", "nexus:settings:read", true},
	{"nexus:settings", "nexus:settings", true},
	{"nexus:settings:read", "nexus:settings:update", false},
	{"nexus:*:read", "nexus:settings:read", true},
	{"nexus:*:read", "nexus:settings:update", false},
	{"nexus:repository-view:*:*:browse", "nexus:repository-view:maven2:demo-maven:browse", true},
	{"nexus:repository-view:*:*:browse", "nexus:repository-view:maven2:demo-maven:read", false},
	{"nexus:repository-view:maven2:*:*", "nexus:repository-view:npm:demo-npm:read", false},
	{"nexus:repository-admin:*:*:*", "nexus:repository-view:maven2:demo-maven:read", false},
	// more parts: the extra parts must all be *
	{"nexus:settings:read:*", "nexus:settings:read", true},
	{"nexus:settings:read:*:*", "nexus:settings:read", true},
	{"*:*:*", "nexus", true},
	{"nexus:settings:read:demo", "nexus:settings:read", false},
	{"nexus:settings:read", "nexus:settings", false},
	{"nexus:repository-view:maven2:demo-maven:browse,read", "nexus:repository-view:maven2:demo-maven", false},
	// lists: the granted part must hold every requested value
	{"nexus:settings:read,update", "nexus:settings:update", true},
	{"nexus:settings:read", "nexus:settings:read,update", false},
	{"nexus:settings:read,update", "nexus:settings:update,read", true},
	{"nexus:settings:*", "nexus:settings:read,update", true},
	{"nexus:settings:read,*", "nexus:settings:delete", true},
	{"nexus:settings,search:read", "nexus:search:read", true},
	{"nexus:settings,search:read", "nexus:settings,search:read", true},
	{"nexus:settings:read,read", "nexus:settings:read", true},
	{",nexus:read", "nexus:read", true},
	// a * in the requested string is a value, not a wildcard
	{"nexus:repository-view:maven2:demo-maven:read", "nexus:repository-view:*:*:read", false},
	{"nexus:repository-view:*:*:read", "nexus:repository-view:*:*:read", true},
	// no partial wildcard
	{"nexus:repo*", "nexus:repository-view", false},
	{"nexus:repository-view:maven*:demo-maven:read", "nexus:repository-view:maven2:demo-maven:read", false},
	// letter case does not count
	{"NEXUS:Settings:READ", "nexus:settings:read", true},
	{"nexus:settings:read", "NEXUS:SETTINGS:READ", true},
	{"nexus:repository-view:maven2:Demo-Maven:read", "nexus:repository-view:maven2:demo-maven:read", true},
	// trimming: the whole string only, not the parts
	{" nexus:settings:read ", "nexus:settings:read", true},
	{"nexus:settings: read", "nexus:settings:read", false},
	// empty parts: trailing ones are dropped, inner ones are the value ""
	{"nexus:settings:read:", "nexus:settings:read", true},
	{"nexus::read", "nexus::read", true},
	{"nexus::read", "nexus:settings:read", false},
	// strings that are no permission imply nothing
	{"", "nexus:settings:read", false},
	{"   ", "nexus", false},
	{"::", "nexus", false},
	{"nexus:,:read", "nexus:x:read", false},
	{"nexus:settings:read", "", false},
	{"nexus:*", "::", false},
}

func TestImplies(t *testing.T) {
	if len(impliesCases) < 40 {
		t.Fatalf("only %d cases", len(impliesCases))
	}
	for _, c := range impliesCases {
		if got := authz.Implies(c.granted, c.requested); got != c.want {
			t.Errorf("Implies(%q, %q) = %v, want %v", c.granted, c.requested, got, c.want)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	for _, s := range []string{"", " ", "\t\n", ":", "::", ":::", ",", "nexus:,", "nexus:,:read", "nexus:,,:read"} {
		if _, err := authz.Parse(s); err == nil {
			t.Errorf("Parse(%q) gave no error", s)
		}
	}
	for _, s := range []string{"nexus", "nexus:*", "*", "nexus::read", ":nexus", "nexus:settings:read:"} {
		if _, err := authz.Parse(s); err != nil {
			t.Errorf("Parse(%q): %v", s, err)
		}
	}
}

// The contract: "A * in the permission of a screen means any value: the
// screen opens when the user holds the permission for at least one
// repository or object".
func TestImpliesSome(t *testing.T) {
	cases := []struct {
		granted, requested string
		want               bool
	}{
		{"nexus:repository-view:maven2:demo-maven:browse,read", "nexus:repository-view:*:*:browse", true},
		{"nexus:repository-view:maven2:demo-maven:read", "nexus:repository-view:*:*:browse", false},
		{"nexus:repository-view:maven2:*:browse", "nexus:repository-view:*:*:browse", true},
		{"nexus:*", "nexus:repository-admin:*:*:read", true},
		{"nexus:settings:read", "nexus:*:read", true},
		{"nexus:settings:read", "nexus:users:read", false},
		{"nexus:repository-view:maven2:demo-maven:read:demo", "nexus:repository-view:*:*:read", false},
		{"nexus:repository-view:maven2:demo-maven:read", "nexus:repository-view:maven2:demo-maven:read", true},
		{"nexus:repository-view:maven2:demo-maven:read", "nexus:repository-view:npm:*:read", false},
		{"", "nexus:*", false},
		{"nexus:*", "", false},
	}
	for _, c := range cases {
		if got := authz.ImpliesSome(c.granted, c.requested); got != c.want {
			t.Errorf("ImpliesSome(%q, %q) = %v, want %v", c.granted, c.requested, got, c.want)
		}
	}
}

func TestAnyImplies(t *testing.T) {
	granted := []string{"::", "nexus:settings:read", "nexus:repository-view:maven2:demo-maven:browse,read"}
	if !authz.AnyImplies(granted, "nexus:repository-view:maven2:demo-maven:read") {
		t.Error("the third grant implies the read")
	}
	if authz.AnyImplies(granted, "nexus:repository-view:raw:demo-raw:read") {
		t.Error("no grant implies a read of demo-raw")
	}
	if authz.AnyImplies(nil, "nexus:settings:read") {
		t.Error("no grant implies nothing")
	}
	if !authz.AnyImpliesSome(granted, "nexus:repository-view:*:*:browse") {
		t.Error("the third grant implies a browse of some repository")
	}
	if authz.AnyImpliesSome(granted, "nexus:users:*") {
		t.Error("no grant implies some users permission")
	}
}

func FuzzParse(f *testing.F) {
	for _, c := range impliesCases {
		f.Add(c.granted, c.requested)
	}
	f.Fuzz(func(t *testing.T, granted, requested string) {
		p, err := authz.Parse(granted)
		if err != nil {
			if authz.Implies(granted, requested) {
				t.Fatalf("%q does not parse and implies %q", granted, requested)
			}
			return
		}
		if q, err := authz.Parse(requested); err == nil && p.Implies(q) && !p.ImpliesSome(q) {
			t.Fatalf("%q implies %q but not some of it", granted, requested)
		}
		if !p.Implies(p) {
			t.Fatalf("%q does not imply itself", granted)
		}
	})
}
