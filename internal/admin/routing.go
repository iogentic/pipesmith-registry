package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/routing"
)

// The rules of the routing rules (leg 6-system, contract S16 to S18): the
// body of a rule, its create, save and delete, the test of a rule against
// one path and the body of a preview. The bounds are those of ruling SY2.

// The bounds of a routing rule body and of a test path (SY2). Characters
// are counted as Unicode code points.
const (
	MaxRuleBody     = 64 << 10
	MaxMatchers     = 100
	MaxMatcherChars = 1024
	MaxTestPath     = 4096
)

// RoutingStore is what a change of the routing rules reads and writes:
// the identity store and the routing rules of state.sqlite with the link
// of each repository.
type RoutingStore interface {
	IdentityStore
	// Routing reads the rules and the links.
	Routing() (model.Routing, error)
	// UpdateRouting replaces the rules and the links with what fn makes of
	// them, in one transaction that raises the generation; an error of fn
	// changes nothing.
	UpdateRouting(fn func(r *model.Routing) error) error
}

// ChangeRouting applies one change of the routing rules: fn changes the
// rules read in the transaction of UpdateRouting. After a write the service
// is marked stale as after an identity change (committed), so the running
// server takes the new rules at its next request (ruling SY3).
func ChangeRouting(store RoutingStore, fn func(r *model.Routing) error) error {
	if err := store.UpdateRouting(fn); err != nil {
		return err
	}
	committed(store)
	return nil
}

// ruleBody is the body of a create or a save (RoutingRuleXO).
type ruleBody struct {
	Name        *string  `json:"name"`
	Description *string  `json:"description"`
	Mode        *string  `json:"mode"`
	Matchers    []string `json:"matchers"`
}

// decodeStrict decodes one JSON object into v: the body at most
// MaxRuleBody bytes (400 rule-bound at *), no unknown key, no value of
// another type and nothing after the object (400 body-invalid at *).
func decodeStrict(kind string, body []byte, v any) error {
	if len(body) > MaxRuleBody {
		return &Refusal{Code: CodeRuleBound, Field: "*", Message: fmt.Sprintf(
			"the body has %d bytes; %s has at most %d", len(body), kind, MaxRuleBody)}
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return &Refusal{Code: CodeBodyInvalid, Field: "*", Message: "the body is no JSON object"}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &Refusal{Code: CodeBodyInvalid, Field: "*", Message: "the body cannot be read: " + oneLine(err.Error())}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return &Refusal{Code: CodeBodyInvalid, Field: "*", Message: "the body holds more than one JSON value"}
	}
	return nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// DecodeRule reads the body of a create or a save of a routing rule. An
// absent name is empty; absent matchers are none.
func DecodeRule(body []byte) (model.RoutingRule, error) {
	var b ruleBody
	if err := decodeStrict("a routing rule body", body, &b); err != nil {
		return model.RoutingRule{}, err
	}
	r := model.RoutingRule{Matchers: slices.Clone(b.Matchers)}
	if b.Name != nil {
		r.Name = *b.Name
	}
	if b.Description != nil {
		r.Description = *b.Description
	}
	if b.Mode != nil {
		r.Mode = *b.Mode
	}
	return r, nil
}

// CheckRule checks a rule of a create: the name pattern, the mode, at
// least one and at most MaxMatchers matchers, each at most
// MaxMatcherChars characters and compiled by the engine of the server. The
// refusals travel together.
func CheckRule(r model.RoutingRule) error {
	var list Refusals
	if err := CheckName(KindRoutingRule, r.Name); err != nil {
		list = append(list, err.(*Refusal))
	}
	list = append(list, checkMode(r.Mode)...)
	list = append(list, checkMatchers(r.Matchers, true)...)
	return list.err()
}

func checkMode(mode string) Refusals {
	if mode == routing.ModeAllow || mode == routing.ModeBlock {
		return nil
	}
	return Refusals{{Code: CodeRuleMode, Field: "mode", Message: fmt.Sprintf(
		"the mode %q is neither %s nor %s", mode, routing.ModeAllow, routing.ModeBlock)}}
}

// checkMatchers refuses more than MaxMatchers matchers at matchers, and
// each matcher above MaxMatcherChars characters or that the engine cannot
// use at matchers[<index>]. A rule needs one matcher when one is true.
func checkMatchers(matchers []string, one bool) Refusals {
	switch {
	case len(matchers) == 0 && one:
		return Refusals{{Code: CodeMatcherInvalid, Field: "matchers", Message: "a routing rule needs at least one matcher"}}
	case len(matchers) > MaxMatchers:
		return Refusals{{Code: CodeRuleBound, Field: "matchers", Message: fmt.Sprintf(
			"%d matchers; a routing rule has at most %d", len(matchers), MaxMatchers)}}
	}
	faults := map[int]string{}
	for _, f := range routing.Explain(model.RoutingRule{Mode: routing.ModeBlock, Matchers: matchers}, "/").Faults {
		faults[f.Index] = f.Message
	}
	var list Refusals
	for i, m := range matchers {
		field := fmt.Sprintf("matchers[%d]", i)
		if n := utf8.RuneCountInString(m); n > MaxMatcherChars {
			list = append(list, &Refusal{Code: CodeRuleBound, Field: field, Message: fmt.Sprintf(
				"the matcher has %d characters; a matcher has at most %d", n, MaxMatcherChars)})
		} else if fault, ok := faults[i]; ok {
			list = append(list, &Refusal{Code: CodeMatcherInvalid, Field: field, Message: fault})
		}
	}
	return list
}

func ruleAt(rs model.Routing, name string) (int, error) {
	at := slices.IndexFunc(rs.Rules, func(r model.RoutingRule) bool { return r.Name == name })
	if at < 0 {
		return -1, &Refusal{Code: CodeNotFound, Message: "no routing rule named " + name}
	}
	return at, nil
}

// CheckRuleCreate checks the create of r: CheckRule, then a name that a
// stored rule holds without regard to letter case (409, ruling ST14).
func CheckRuleCreate(rs model.Routing, r model.RoutingRule) error {
	if err := CheckRule(r); err != nil {
		return err
	}
	names := make([]string, len(rs.Rules))
	for i, x := range rs.Rules {
		names[i] = x.Name
	}
	return CheckCreate(KindRoutingRule, r.Name, Taken(names, r.Name))
}

// SaveRule replaces the stored rule of the name with r: 404 for no such
// rule, 400 for another name in the body (a link names the rule by its
// name), and the checks of CheckRule but the name pattern, which the stored
// name passes (R6). An empty name in r is the stored one.
func SaveRule(rs *model.Routing, name string, r model.RoutingRule) error {
	at, err := ruleAt(*rs, name)
	if err != nil {
		return err
	}
	if r.Name == "" {
		r.Name = name
	}
	var list Refusals
	if err := CheckRename(KindRoutingRule, name, &r.Name); err != nil {
		list = append(list, err.(*Refusal))
	}
	list = append(list, checkMode(r.Mode)...)
	list = append(list, checkMatchers(r.Matchers, true)...)
	if err := list.err(); err != nil {
		return err
	}
	rs.Rules = slices.Clone(rs.Rules)
	r.Matchers = slices.Clone(r.Matchers)
	rs.Rules[at] = r
	return nil
}

// DeleteRule removes the rule of the name: 404 for no such rule, 409 while
// a repository names it (CheckRuleDelete).
func DeleteRule(rs *model.Routing, name string) error {
	at, err := ruleAt(*rs, name)
	if err != nil {
		return err
	}
	if err := CheckRuleDelete(name, RuleUsers(*rs, name)); err != nil {
		return err
	}
	rs.Rules = slices.Delete(slices.Clone(rs.Rules), at, at+1)
	return nil
}

// RuleUsers are the repositories whose link names the rule, sorted.
func RuleUsers(rs model.Routing, name string) []string {
	var names []string
	for repository, rule := range rs.Links {
		if rule == name {
			names = append(names, repository)
		}
	}
	slices.Sort(names)
	return names
}

// RuleTest is the body of the test of a rule against one path.
type RuleTest struct {
	Mode     string   `json:"mode"`
	Matchers []string `json:"matchers"`
	Path     *string  `json:"path"`
}

// DecodeRuleTest reads the body of a test; a test needs a path.
func DecodeRuleTest(body []byte) (RuleTest, error) {
	var t RuleTest
	if err := decodeStrict("a routing rule test", body, &t); err != nil {
		return RuleTest{}, err
	}
	if t.Path == nil {
		return RuleTest{}, &Refusal{Code: CodeBodyInvalid, Field: "path", Message: "the test needs a path"}
	}
	return t, nil
}

// CheckRuleTest checks the mode, the matchers and the path of a test and
// answers it with the engine of the server. A test may have no matcher.
func CheckRuleTest(t RuleTest) (routing.Explanation, error) {
	list := checkMode(t.Mode)
	list = append(list, checkMatchers(t.Matchers, false)...)
	path := ""
	if t.Path != nil {
		path = *t.Path
	}
	list = append(list, checkPath(path)...)
	if err := list.err(); err != nil {
		return routing.Explanation{}, err
	}
	return routing.Explain(model.RoutingRule{Mode: t.Mode, Matchers: t.Matchers}, path), nil
}

func checkPath(path string) Refusals {
	if n := utf8.RuneCountInString(path); n > MaxTestPath {
		return Refusals{{Code: CodeRuleBound, Field: "path", Message: fmt.Sprintf(
			"the path has %d characters; a test path has at most %d", n, MaxTestPath)}}
	}
	return nil
}

// The scopes of a preview (contract S18).
var previewScopes = []string{"all", "group", "proxy"}

// RulePreview is the body of a preview.
type RulePreview struct {
	Scope string  `json:"scope"`
	Path  *string `json:"path"`
}

// DecodePreview reads the body of a preview: a scope of previewScopes and a
// path of at most MaxTestPath characters. The refusals travel together.
func DecodePreview(body []byte) (RulePreview, error) {
	var p RulePreview
	if err := decodeStrict("a routing preview", body, &p); err != nil {
		return RulePreview{}, err
	}
	var list Refusals
	if !slices.Contains(previewScopes, p.Scope) {
		list = append(list, &Refusal{Code: CodePreviewScope, Field: "scope", Message: fmt.Sprintf(
			"the scope %q is not known; use one of %s", p.Scope, strings.Join(previewScopes, ", "))})
	}
	if p.Path == nil {
		list = append(list, &Refusal{Code: CodeBodyInvalid, Field: "path", Message: "the preview needs a path"})
	} else {
		list = append(list, checkPath(*p.Path)...)
	}
	return p, list.err()
}
