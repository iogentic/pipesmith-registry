package rest

import (
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The routing rules (leg 6-system, contract S16 to S18): the list, one
// rule, create, save and delete through admin.ChangeRouting, the test of a
// rule and the preview. The server checks the permission and the request
// token before it calls a function here.

// routingRuleXO is one RoutingRuleXO.
type routingRuleXO struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Mode        string   `json:"mode"`
	Matchers    []string `json:"matchers"`
}

func ruleXO(r model.RoutingRule) routingRuleXO {
	matchers := slices.Clone(r.Matchers)
	if matchers == nil {
		matchers = []string{}
	}
	return routingRuleXO{Name: r.Name, Description: r.Description, Mode: r.Mode, Matchers: matchers}
}

// RoutingRules is GET /v1/routing-rules: every rule, sorted by name.
func RoutingRules(store admin.RoutingStore) (Answer, error) {
	rs, err := store.Routing()
	if err != nil {
		return Answer{}, err
	}
	out := make([]routingRuleXO, 0, len(rs.Rules))
	for _, r := range rs.Rules {
		out = append(out, ruleXO(r))
	}
	slices.SortFunc(out, func(a, b routingRuleXO) int { return strings.Compare(a.Name, b.Name) })
	return ok(out)
}

// RoutingRule is GET /v1/routing-rules/{name}: the rule, or 404.
func RoutingRule(store admin.RoutingStore, name string) (Answer, error) {
	rs, err := store.Routing()
	if err != nil {
		return Answer{}, err
	}
	for _, r := range rs.Rules {
		if r.Name == name {
			return ok(ruleXO(r))
		}
	}
	return refused(&admin.Refusal{Code: admin.CodeNotFound, Message: "no routing rule named " + name})
}

// CreateRoutingRule is POST /v1/routing-rules: 204 after the create.
func CreateRoutingRule(store admin.RoutingStore, body []byte) (Answer, error) {
	rule, err := admin.DecodeRule(body)
	if err != nil {
		return refused(err)
	}
	err = admin.ChangeRouting(store, func(rs *model.Routing) error {
		if err := admin.CheckRuleCreate(*rs, rule); err != nil {
			return err
		}
		rs.Rules = append(rs.Rules, rule)
		return nil
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}

// SaveRoutingRule is PUT /v1/routing-rules/{name}: 204 after the save.
func SaveRoutingRule(store admin.RoutingStore, name string, body []byte) (Answer, error) {
	rule, err := admin.DecodeRule(body)
	if err != nil {
		return refused(err)
	}
	if err := admin.ChangeRouting(store, func(rs *model.Routing) error { return admin.SaveRule(rs, name, rule) }); err != nil {
		return refused(err)
	}
	return noContent()
}

// DeleteRoutingRule is DELETE /v1/routing-rules/{name}: 204 after the
// delete, 409 while a repository names the rule.
func DeleteRoutingRule(store admin.RoutingStore, name string) (Answer, error) {
	if err := admin.ChangeRouting(store, func(rs *model.Routing) error { return admin.DeleteRule(rs, name) }); err != nil {
		return refused(err)
	}
	return noContent()
}

type ruleTestXO struct {
	Allowed bool `json:"allowed"`
	Matcher *int `json:"matcher"`
}

// TestRoutingRule is POST /api/routing-rules/test: {allowed, matcher},
// matcher the index of the first matcher that matched or null.
func TestRoutingRule(body []byte) (Answer, error) {
	t, err := admin.DecodeRuleTest(body)
	if err != nil {
		return refused(err)
	}
	e, err := admin.CheckRuleTest(t)
	if err != nil {
		return refused(err)
	}
	out := ruleTestXO{Allowed: e.Allowed}
	if e.Matcher >= 0 {
		out.Matcher = &e.Matcher
	}
	return ok(out)
}

// Verdict gives for a proxy repository and a path the rule it names, empty
// for none, and whether the path may go to the remote; a refusal of admin
// when the rules cannot be read.
type Verdict func(repository, path string) (rule string, allowed bool, err error)

type previewItemXO struct {
	Repository string  `json:"repository"`
	Type       string  `json:"type"`
	Format     string  `json:"format"`
	Rule       *string `json:"rule"`
	Allowed    bool    `json:"allowed"`
}

type previewXO struct {
	Items []previewItemXO `json:"items"`
}

// PreviewRouting is POST /api/routing-rules/preview: the verdict of every
// repository of the scope that the subject may browse, sorted by name
// (ruling SY2). A rule decides for a proxy only, so a hosted repository
// and a group have no rule and allow every path. No content is read.
func PreviewRouting(env *Env, body []byte, verdict Verdict) (Answer, error) {
	p, err := admin.DecodePreview(body)
	if err != nil {
		return refused(err)
	}
	out := previewXO{Items: []previewItemXO{}}
	for _, r := range env.Repositories.All() {
		if p.Scope != "all" && r.Type != p.Scope || !env.permits(ViewPermission(r.Format, r.Name, "browse")) {
			continue
		}
		item := previewItemXO{Repository: r.Name, Type: r.Type, Format: r.Format, Allowed: true}
		if r.Type == "proxy" {
			rule, allowed, err := verdict(r.Name, *p.Path)
			if err != nil {
				return refused(err)
			}
			if rule != "" {
				item.Rule, item.Allowed = &rule, allowed
			}
		}
		out.Items = append(out.Items, item)
	}
	slices.SortFunc(out.Items, func(a, b previewItemXO) int { return strings.Compare(a.Repository, b.Repository) })
	return ok(out)
}
