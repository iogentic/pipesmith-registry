package routing_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/routing"
)

// The vectors of testdata/vectors/routing.json are written by hand; the
// file names the documentation and the source they follow.
type routingVectors struct {
	Sources []string            `json:"sources"`
	Rules   []model.RoutingRule `json:"rules"`
	Links   map[string]string   `json:"links"`
	Cases   []struct {
		Name       string `json:"name"`
		Repository string `json:"repository"`
		Path       string `json:"path"`
		Allowed    bool   `json:"allowed"`
	} `json:"cases"`
	Tests []struct {
		Name    string            `json:"name"`
		Rule    model.RoutingRule `json:"rule"`
		Path    string            `json:"path"`
		Allowed bool              `json:"allowed"`
		Error   string            `json:"error"`
	} `json:"tests"`
	Problems []string `json:"problems"`
}

func loadVectors(t testing.TB) routingVectors {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/routing.json")
	if err != nil {
		t.Fatal(err)
	}
	var v routingVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func compiled(v routingVectors) *routing.Rules {
	return routing.Compile(model.Routing{Rules: v.Rules, Links: v.Links})
}

func TestRoutingVectors(t *testing.T) {
	v := loadVectors(t)
	if n := len(v.Cases) + len(v.Tests); n < 30 {
		t.Fatalf("%d vectors, want at least 30", n)
	}
	rs := compiled(v)
	for _, c := range v.Cases {
		if got := rs.Allowed(c.Repository, c.Path); got != c.Allowed {
			t.Errorf("%s: Allowed(%q, %q) = %v, want %v", c.Name, c.Repository, c.Path, got, c.Allowed)
		}
	}
}

// The test of one rule and one path, for the preview of phase 6, answers as
// the rule of a repository does and names why a rule blocks every path.
func TestRuleTest(t *testing.T) {
	for _, c := range loadVectors(t).Tests {
		allowed, err := routing.Test(c.Rule, c.Path)
		if allowed != c.Allowed {
			t.Errorf("%s: allowed %v, want %v", c.Name, allowed, c.Allowed)
		}
		switch {
		case c.Error == "" && err != nil:
			t.Errorf("%s: %v", c.Name, err)
		case c.Error != "" && (err == nil || !strings.HasPrefix(err.Error(), c.Error)):
			t.Errorf("%s: error %v, want it to start with %q", c.Name, err, c.Error)
		}
	}
}

// Problems names every rule that blocks every path and every repository that
// names a rule that does not exist, sorted, for the start log.
func TestProblemsNameEveryRuleThatBlocksEverything(t *testing.T) {
	v := loadVectors(t)
	got := compiled(v).Problems()
	if len(got) != len(v.Problems) {
		t.Fatalf("problems\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(v.Problems, "\n"))
	}
	for i, want := range v.Problems {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("problem %d: %q, want it to start with %q", i, got[i], want)
		}
	}
	if p := routing.Compile(model.Routing{}).Problems(); len(p) != 0 {
		t.Fatalf("no rules: %v", p)
	}
}

// Without rules, and for a nil *Rules, every path is allowed.
func TestNoRulesAllowEverything(t *testing.T) {
	for name, rs := range map[string]*routing.Rules{"nil": nil, "empty": routing.Compile(model.Routing{})} {
		if !rs.Allowed("demo-proxy", "/com/example/x.jar") {
			t.Errorf("%s: a path was blocked", name)
		}
		if len(rs.Problems()) != 0 {
			t.Errorf("%s: %v", name, rs.Problems())
		}
	}
}

// The compiled rules keep no reference to what they were compiled from.
func TestCompileCopiesItsInput(t *testing.T) {
	r := model.Routing{Rules: []model.RoutingRule{{Name: "r", Mode: "BLOCK", Matchers: []string{"^/blocked/.*"}}},
		Links: map[string]string{"demo-proxy": "r"}}
	rs := routing.Compile(r)
	r.Rules[0].Mode, r.Rules[0].Matchers[0], r.Links["demo-proxy"] = "ALLOW", "^/other/.*", "gone"
	if rs.Allowed("demo-proxy", "/blocked/a") || !rs.Allowed("demo-proxy", "/other/a") {
		t.Fatal("a change of the input changed the compiled rules")
	}
}

// Two rules of one name leave it open which one counts, so the name blocks
// every path and is named.
func TestDuplicateRuleNames(t *testing.T) {
	rs := routing.Compile(model.Routing{Rules: []model.RoutingRule{
		{Name: "r", Mode: "BLOCK", Matchers: []string{"^/a$"}}, {Name: "r", Mode: "ALLOW", Matchers: []string{"^/a$"}}},
		Links: map[string]string{"demo-proxy": "r"}})
	if rs.Allowed("demo-proxy", "/a") || rs.Allowed("demo-proxy", "/b") {
		t.Fatal("a rule of a name given twice allowed a path")
	}
	if p := rs.Problems(); len(p) != 1 || p[0] != `routing rule "r" blocks every path: the name is given to 2 rules` {
		t.Fatalf("%v", p)
	}
}

// Many goroutines ask at once under -race.
func TestRulesAreSafeForParallelUse(t *testing.T) {
	rs := compiled(loadVectors(t))
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				rs.Allowed("demo-proxy-internal", fmt.Sprintf("/com/example/internal/%d/%d.jar", g, i))
				rs.Allowed("demo-proxy-releases", fmt.Sprintf("/com/example/demo-lib/1.%d/x.jar", i))
			}
		}()
	}
	wg.Wait()
}
