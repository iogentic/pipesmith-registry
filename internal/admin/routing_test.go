package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// grown builds the body of a case with grow: the matchers, the one long
// matcher, the long test path, or a description that brings the body to
// exactly the bytes asked.
func grown(t *testing.T, body map[string]any, grow map[string]int) []byte {
	t.Helper()
	b := maps.Clone(body)
	if n := grow["matchers"]; n > 0 {
		list := make([]string, n)
		for i := range list {
			list[i] = fmt.Sprintf("/m%d", i)
		}
		b["matchers"] = list
	}
	if n := grow["matcher"]; n > 0 {
		b["matchers"] = []string{"/" + strings.Repeat("a", n-1)}
	}
	if n := grow["path"]; n > 0 {
		b["path"] = "/" + strings.Repeat("p", n-1)
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if n := grow["body"]; n > 0 {
		b["description"] = ""
		base, _ := json.Marshal(b)
		b["description"] = strings.Repeat("d", n-len(base))
		data, _ = json.Marshal(b)
		if len(data) != n {
			t.Fatalf("a body of %d bytes, want %d", len(data), n)
		}
	}
	return data
}

func TestRoutingVectors(t *testing.T) {
	var file struct {
		Family string              `json:"family"`
		Note   string              `json:"note"`
		Rules  []model.RoutingRule `json:"rules"`
		Links  map[string]string   `json:"links"`
		Cases  []struct {
			Row      string         `json:"row"`
			Op       string         `json:"op"`
			Name     string         `json:"name"`
			Body     map[string]any `json:"body"`
			Raw      *string        `json:"raw"`
			Grow     map[string]int `json:"grow"`
			Allowed  bool           `json:"allowed"`
			Matcher  int            `json:"matcher"`
			Refusals *[]wantRefusal `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "routing", &file)
	counts := tally{}
	for i, c := range file.Cases {
		label := fmt.Sprintf("case %d (%s %s %s)", i, c.Row, c.Op, c.Name)
		data := grown(t, c.Body, c.Grow)
		if c.Raw != nil {
			data = []byte(*c.Raw)
		}
		rs := model.Routing{Rules: slices.Clone(file.Rules), Links: maps.Clone(file.Links)}
		var err error
		switch c.Op {
		case "create":
			var r model.RoutingRule
			if r, err = DecodeRule(data); err == nil {
				err = CheckRuleCreate(rs, r)
			}
		case "save":
			var r model.RoutingRule
			if r, err = DecodeRule(data); err == nil {
				err = SaveRule(&rs, c.Name, r)
			}
			if err == nil {
				at := slices.IndexFunc(rs.Rules, func(x model.RoutingRule) bool { return x.Name == c.Name })
				if at < 0 || rs.Rules[at].Mode != r.Mode || !slices.Equal(rs.Rules[at].Matchers, r.Matchers) {
					t.Errorf("%s: the save left %+v", label, rs.Rules)
				}
			}
		case "delete":
			err = DeleteRule(&rs, c.Name)
			if err == nil && slices.ContainsFunc(rs.Rules, func(x model.RoutingRule) bool { return x.Name == c.Name }) {
				t.Errorf("%s: the rule stays", label)
			}
		case "test":
			var rt RuleTest
			if rt, err = DecodeRuleTest(data); err == nil {
				got, terr := CheckRuleTest(rt)
				err = terr
				if err == nil && (got.Allowed != c.Allowed || got.Matcher != c.Matcher) {
					t.Errorf("%s: allowed %v at %d, want %v at %d", label, got.Allowed, got.Matcher, c.Allowed, c.Matcher)
				}
			}
		case "preview":
			_, err = DecodePreview(data)
		default:
			t.Fatalf("%s: unknown op", label)
		}
		if err != nil && c.Refusals != nil && len(*c.Refusals) == 0 {
			t.Errorf("%s: %v", label, err)
		}
		checkRefusals(t, label, err, c.Refusals)
		counts.add(c.Row, c.Refusals != nil && len(*c.Refusals) > 0)
		if err != nil && (c.Op == "save" || c.Op == "delete") {
			if !reflect.DeepEqual(rs, model.Routing{Rules: file.Rules, Links: file.Links}) {
				t.Errorf("%s: a refusal changed the rules", label)
			}
		}
	}
	counts.check(t, "routing", "body", "name", "mode", "matchers", "together", "SY2", "exists", "save", "delete", "test", "preview")
}

// The bounds of SY2 hold one past them and not at them.
func TestRuleBounds(t *testing.T) {
	rule := func(matchers []string) model.RoutingRule {
		return model.RoutingRule{Name: "demo-r", Mode: "BLOCK", Matchers: matchers}
	}
	many := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "/x"
		}
		return out
	}
	for _, c := range []struct {
		name  string
		err   error
		field string
	}{
		{"100 matchers", CheckRule(rule(many(MaxMatchers))), ""},
		{"101 matchers", CheckRule(rule(many(MaxMatchers + 1))), "matchers"},
		{"1024 characters", CheckRule(rule([]string{"/" + strings.Repeat("é", MaxMatcherChars-1)})), ""},
		{"1025 characters", CheckRule(rule([]string{"/" + strings.Repeat("é", MaxMatcherChars)})), "matchers[0]"},
		{"a long matcher after a short one", CheckRule(rule([]string{"/x", strings.Repeat("a", MaxMatcherChars+1)})), "matchers[1]"},
	} {
		var r *Refusal
		switch {
		case c.field == "" && c.err != nil:
			t.Errorf("%s: %v", c.name, c.err)
		case c.field != "" && (!errors.As(c.err, &r) || r.Code != CodeRuleBound || r.Field != c.field):
			t.Errorf("%s: %v, want rule-bound at %s", c.name, c.err, c.field)
		}
	}
	if MaxMatchers != 100 || MaxMatcherChars != 1024 || MaxTestPath != 4096 || MaxRuleBody != 65536 {
		t.Fatalf("the bounds are %d, %d, %d, %d", MaxMatchers, MaxMatcherChars, MaxTestPath, MaxRuleBody)
	}
}

// RuleUsers are the repositories whose link names the rule, sorted.
func TestRuleUsers(t *testing.T) {
	rs := model.Routing{Links: map[string]string{"b": "r", "a": "r", "c": "other"}}
	if got := RuleUsers(rs, "r"); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("%v", got)
	}
	if got := RuleUsers(rs, "none"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

// routingStore is RoutingStore over rules in memory.
type routingStore struct {
	fakeStore
	rules        model.Routing
	routingWrite int
}

func (f *routingStore) Routing() (model.Routing, error) { return f.rules, nil }
func (f *routingStore) UpdateRouting(fn func(*model.Routing) error) error {
	r := model.Routing{Rules: slices.Clone(f.rules.Rules), Links: maps.Clone(f.rules.Links)}
	if err := fn(&r); err != nil {
		return err
	}
	f.rules, f.routingWrite = r, f.routingWrite+1
	return nil
}

// ChangeRouting writes through the store and marks the service stale after
// a write; a refusal writes nothing and marks nothing.
func TestChangeRouting(t *testing.T) {
	store := &routingStore{fakeStore: fakeStore{snap: &model.Snapshot{}}}
	err := ChangeRouting(store, func(r *model.Routing) error {
		r.Rules = append(r.Rules, model.RoutingRule{Name: "demo-r", Mode: "BLOCK", Matchers: []string{"/x"}})
		return nil
	})
	if err != nil || store.routingWrite != 1 || store.stale != 1 || len(store.rules.Rules) != 1 {
		t.Fatalf("%v, %d writes, %d stale, %+v", err, store.routingWrite, store.stale, store.rules)
	}
	refused := &Refusal{Code: CodeRuleInUse, Message: "m"}
	err = ChangeRouting(store, func(r *model.Routing) error { r.Rules = nil; return refused })
	if !errors.Is(err, refused) || store.routingWrite != 1 || store.stale != 1 || len(store.rules.Rules) != 1 {
		t.Fatalf("%v, %d writes, %d stale", err, store.routingWrite, store.stale)
	}
}
