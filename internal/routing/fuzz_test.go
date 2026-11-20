package routing_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/routing"
)

// FuzzRouting compiles one rule of any mode and matcher and asks for any
// path: nothing panics, the rule of a repository and the test of the rule
// give one answer, a rule that blocks every path blocks the path and is
// named, and a working rule answers by its mode and a whole match of the
// path with its leading slash. A run of 30 s:
//
//	tools/go.sh run 'go test -run "^$" -fuzz "^FuzzRouting$" -fuzztime 30s ./internal/routing'
func FuzzRouting(f *testing.F) {
	v := loadVectors(f)
	for i, rule := range v.Rules {
		path := v.Cases[i%len(v.Cases)].Path
		for _, matcher := range rule.Matchers {
			f.Add(rule.Mode, matcher, path)
		}
	}
	f.Add("BLOCK", "", "")
	f.Add("ALLOW", `\A/x\z`, "/x")
	f.Fuzz(func(t *testing.T, mode, matcher, path string) {
		rule := model.RoutingRule{Name: "fuzz", Mode: mode, Matchers: []string{matcher}}
		rs := routing.Compile(model.Routing{Rules: []model.RoutingRule{rule}, Links: map[string]string{"demo-proxy": "fuzz"}})
		allowed := rs.Allowed("demo-proxy", path)
		tested, err := routing.Test(rule, path)
		if allowed != tested {
			t.Fatalf("Allowed %v, Test %v", allowed, tested)
		}
		if err != nil {
			if allowed {
				t.Fatalf("a rule that blocks every path allowed %q: %v", path, err)
			}
			if len(rs.Problems()) != 1 {
				t.Fatalf("problems %v", rs.Problems())
			}
			return
		}
		if len(rs.Problems()) != 0 {
			t.Fatalf("a working rule has problems: %v", rs.Problems())
		}
		full := path
		if !strings.HasPrefix(full, "/") {
			full = "/" + full
		}
		// an oracle of its own: the leftmost longest match spans the whole
		// path exactly when some match does
		re := regexp.MustCompile(matcher)
		re.Longest()
		loc := re.FindStringIndex(full)
		matches := loc != nil && loc[0] == 0 && loc[1] == len(full)
		if want := matches == (mode == routing.ModeAllow); allowed != want {
			t.Fatalf("mode %s, matcher %q, path %q: allowed %v, want %v", mode, matcher, path, allowed, want)
		}
	})
}
