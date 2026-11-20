package routing_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/routing"
)

// Explain answers for one rule and one path as Test does, and names the
// matcher that matched first and every matcher that does not compile, by
// its index from 0 (contract S17, ruling SY1 of leg 6-system).
func TestExplain(t *testing.T) {
	for _, c := range []struct {
		name     string
		rule     model.RoutingRule
		path     string
		allowed  bool
		matcher  int
		faults   []int
		modeBad  bool
		contains []string // words of the fault messages, in the order of faults
	}{
		{"block, the second matcher matches", model.RoutingRule{Mode: "BLOCK", Matchers: []string{"^/a/.*", "^/b/.*"}},
			"/b/x", false, 1, nil, false, nil},
		{"block, no matcher matches", model.RoutingRule{Mode: "BLOCK", Matchers: []string{"^/a/.*", "^/b/.*"}},
			"/c/x", true, -1, nil, false, nil},
		{"allow, the first matcher matches", model.RoutingRule{Mode: "ALLOW", Matchers: []string{"/a/.*", "/.*"}},
			"/a/x", true, 0, nil, false, nil},
		{"allow, no matcher matches", model.RoutingRule{Mode: "ALLOW", Matchers: []string{"/a/.*"}},
			"/c/x", false, -1, nil, false, nil},
		{"the slash is supplied", model.RoutingRule{Mode: "BLOCK", Matchers: []string{"/a/.*"}},
			"a/x", false, 0, nil, false, nil},
		{"a matcher matches the whole path only", model.RoutingRule{Mode: "BLOCK", Matchers: []string{"/a"}},
			"/a/x", true, -1, nil, false, nil},
		{"no matcher, block", model.RoutingRule{Mode: "BLOCK", Matchers: []string{}}, "/a", true, -1, nil, false, nil},
		{"no matcher, allow", model.RoutingRule{Mode: "ALLOW"}, "/a", false, -1, nil, false, nil},
		{"two faults by index", model.RoutingRule{Mode: "BLOCK", Matchers: []string{"^/ok/.*", "(", "[a[b]]", "^/x$"}},
			"/x", false, -1, []int{1, 2}, false, []string{"missing closing )", "class inside a class"}},
		{"a fault blocks a path that would pass", model.RoutingRule{Mode: "ALLOW", Matchers: []string{"/.*", "*"}},
			"/x", false, -1, []int{1}, false, []string{"missing argument"}},
		{"an unknown mode", model.RoutingRule{Mode: "allow", Matchers: []string{"/.*"}}, "/x", false, -1, nil, true, nil},
	} {
		got := routing.Explain(c.rule, c.path)
		if got.Allowed != c.allowed || got.Matcher != c.matcher {
			t.Errorf("%s: allowed %v, matcher %d, want %v and %d", c.name, got.Allowed, got.Matcher, c.allowed, c.matcher)
		}
		var indexes []int
		for i, f := range got.Faults {
			indexes = append(indexes, f.Index)
			if i < len(c.contains) && !strings.Contains(f.Message, c.contains[i]) {
				t.Errorf("%s: fault %d says %q, want it to hold %q", c.name, f.Index, f.Message, c.contains[i])
			}
		}
		if !reflect.DeepEqual(indexes, c.faults) {
			t.Errorf("%s: faults at %v, want %v", c.name, indexes, c.faults)
		}
		if (got.Mode != "") != c.modeBad {
			t.Errorf("%s: mode fault %q, want one: %v", c.name, got.Mode, c.modeBad)
		}
	}
}

// Explain gives the verdict of Test for every vector of routing.json, and a
// fault where Test gives an error.
func TestExplainAgreesWithTest(t *testing.T) {
	v := loadVectors(t)
	n := 0
	for _, c := range v.Tests {
		allowed, err := routing.Test(c.Rule, c.Path)
		got := routing.Explain(c.Rule, c.Path)
		if got.Allowed != allowed {
			t.Errorf("%s: Explain allowed %v, Test %v", c.Name, got.Allowed, allowed)
		}
		if broken := len(got.Faults) > 0 || got.Mode != ""; broken != (err != nil) {
			t.Errorf("%s: Explain faults %v mode %q, Test error %v", c.Name, got.Faults, got.Mode, err)
		}
		n++
	}
	for _, r := range v.Rules {
		for _, path := range []string{"/com/example/internal/x.jar", "/com/example/demo-lib/1.0/x.jar", "/"} {
			allowed, _ := routing.Test(r, path)
			if got := routing.Explain(r, path); got.Allowed != allowed {
				t.Errorf("rule %s, %s: Explain allowed %v, Test %v", r.Name, path, got.Allowed, allowed)
			}
			n++
		}
	}
	if n < 10 {
		t.Fatalf("%d comparisons, want at least 10", n)
	}
}
