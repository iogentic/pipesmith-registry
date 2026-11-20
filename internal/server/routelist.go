package server

import (
	"cmp"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// The route table of a server with security on, as a list for the walk over
// every route (task 19 of the phase 3 plan): each entry with its rule and
// requests that reach it, with writes off or on. The hidden command
// `routes --json [--writes]` prints it, so the walk reads the routes from the
// binary it walks. The examples come from the patterns themselves;
// placeholders stand where the rule reads a repository.

// RouteInfo is one entry of the table, in the order of the table.
type RouteInfo struct {
	Table       string   `json:"table"`   // security, proxies, stores, writes, refusals or step 2
	Pattern     string   `json:"pattern"` // the regular expression of the decoded path
	Methods     []string `json:"methods"`
	Rule        string   `json:"rule"`                  // a key of RouteRules
	Permissions []string `json:"permissions,omitempty"` // application: any one of them
	Action      string   `json:"action,omitempty"`      // view, admin, asset, client, content: the action of the permission
	// Actions are the actions of a write below /repository/ with writes on,
	// by method (writeAction): add for a path the repository does not hold,
	// edit for one it holds, delete for DELETE. An npm or Docker path that
	// changes another stored path asks what ociAction names for it.
	Actions map[string]MethodAction `json:"actions,omitempty"`
	// Format is the format the repository of an example must have, for a
	// route whose handler answers that format only and hands any other to
	// the routes of step 2.
	Format string `json:"format,omitempty"`
	// Headers are what a request of the route needs besides credentials to
	// be one its handler takes, as the header of POST /api/scan.
	Headers map[string]string `json:"headers,omitempty"`
	// Refuses is a permission with * parts: a subject that holds it for no
	// value of the * parts gets 403 from the handler after the rule passed,
	// as the contract of the interface asks of the settings of repositories.
	Refuses  string   `json:"refuses,omitempty"`
	Examples []string `json:"examples"` // paths that reach the entry, one per alternative of the pattern
}

// MethodAction is the action a write asks for a path the repository does
// not hold (New) and for one it holds (Held).
type MethodAction struct {
	New  string `json:"new"`
	Held string `json:"held"`
}

// RouteRules say what each rule asks of the caller.
var RouteRules = map[string]string{
	"public":      "nothing; a handler may still check credentials it is given",
	"signed-in":   "a subject; anonymous access does not count",
	"application": "a subject that holds one of the permissions",
	"view":        "a subject that holds nexus:repository-view:<format>:<repository>:<action>",
	"admin":       "a subject that holds nexus:repository-admin:<format>:<repository>:<action>",
	"asset":       "a subject that holds nexus:repository-view:<format>:<repository of the asset id>:<action>",
	"client":      "nexus:repository-view:<format>:<repository>:<action>; without credentials the rights of the anonymous user while anonymous access is on",
	"content":     "a subject that holds nexus:repository-view:<format>:<repository of the id, or of the query parameter repository>:<action>",
	"create":      "a subject that holds nexus:repository-admin:<format of the segment>:*:add",
	"listing":     "a subject that holds nexus:repository-view:<format>:<repository of the id, or of the query parameter repository>:browse",
}

// RoutePlaceholders explain the placeholders of the examples.
var RoutePlaceholders = map[string]string{
	"{repository}": "the name of a repository, of the format of the entry when it names one",
	"{asset}":      "an asset id: URL safe Base64 without padding of <repository>:<path>",
	"{path}":       "a path inside the repository",
	"{token}":      "a token in the wire form <prefix>.<public half>.<secret half>",
}

var ruleNames = map[ruleKind]string{rulePublic: "public", ruleSignedIn: "signed-in", ruleApp: "application", ruleView: "view",
	ruleAdmin: "admin", ruleAssetID: "asset", ruleClient: "client", ruleContent: "content", ruleCreate: "create",
	ruleListing: "listing"}

// hint is what the pattern and the rule of an entry do not say and its
// handler checks: the format of the repository of a group and what other
// groups name, a header the request needs, a refusal after the rule.
type hint struct {
	format  string
	groups  map[int]string
	headers map[string]string
	refuses string
}

// routeHints are those checks of the handlers, by the pattern of the entry.
// The routes of tokens (tokens.go) check the format of the repository of
// group 1 before anything else; POST /api/scan needs the header of the page
// (api.go, postRefusal); the settings of repositories answer 403 to a subject
// that may administer none (docs/ui/API.md, the read of the repositories).
var routeHints = map[string]hint{
	exact(repositoryPrefix + segment + `/-/user/org\.couchdb\.user:(.+)`).String(): {format: "npm", groups: map[int]string{1: "{repository}"}},
	exact(repositoryPrefix + segment + `/-/whoami`).String():                       {format: "npm", groups: map[int]string{1: "{repository}"}},
	exact(repositoryPrefix + segment + npmTokenPath + `(.+)`).String(): {format: "npm",
		groups: map[int]string{1: "{repository}", 2: "{token}"}},
	exact(repositoryPrefix + segment + `/v2/token`).String(): {format: "docker", groups: map[int]string{1: "{repository}"}},
	`^/api/scan\n?$`: {headers: map[string]string{postHeader: "1"}},
	exact(restBase + "/repositorySettings").String(): {refuses: rest.AdminPermission("*", "*", "read")},
}

// Routes is the table with security on (Server.table): with writes off the
// routes of security, the refusals with those of the REST writes, then the
// routes of step 2 (securedRoutes); with writes on the REST writes in place
// of their refusals (writingRoutes).
func Routes(writes bool) []RouteInfo {
	type part struct {
		name string
		list []route
	}
	parts := []part{{"security", slices.Concat(secureRoutes, identityRoutes)}, {"system", systemRoutes}, {"status", statusAreaRoutes}, {"figures", figureRoutes}, {"logs", logRoutes}, {"proxies", proxyRoutes}, {"stores", storeRoutes},
		{"refusals", slices.Concat(routesOf(writeRefusals), refusalRoutes)},
		{"step 2", routes}}
	if writes {
		parts = []part{{"security", slices.Concat(secureRoutes, identityRoutes)}, {"system", systemRoutes}, {"status", statusAreaRoutes}, {"figures", figureRoutes}, {"logs", logRoutes}, {"proxies", proxyRoutes}, {"stores", storeRoutes}, {"writes", contentWriteRoutes}, {"refusals", refusalRoutes},
			{"step 2", routes}}
	}
	var out []RouteInfo
	for _, p := range parts {
		for _, rt := range p.list {
			out = append(out, routeInfo(p.name, rt, writes))
		}
	}
	return out
}

func routeInfo(table string, rt route, writes bool) RouteInfo {
	info := RouteInfo{Table: table, Pattern: rt.Pattern.String(), Methods: slices.Clone(rt.Methods), Rule: ruleNames[rt.Rule.kind]}
	groups := map[int]string{}
	query := ""
	switch rt.Rule.kind {
	case ruleApp:
		info.Permissions = slices.Clone(rt.Rule.permissions)
	case ruleView:
		info.Action, groups[rt.Rule.group] = rt.Rule.action, "{repository}"
	case ruleAdmin:
		info.Action, groups[rt.Rule.group] = cmp.Or(rt.Rule.action, "read"), "{repository}"
	case ruleAssetID:
		info.Action, groups[rt.Rule.group] = "browse", "{asset}"
	case ruleCreate:
		info.Action = "add"
	case ruleClient:
		// allowClient: browse below the browse route, read below /repository/;
		// with writes on a write below /repository/ asks the action of
		// writeAction for its method
		info.Action = "read"
		if strings.HasPrefix(info.Pattern, "^"+regexp.QuoteMeta(browsePrefix)) {
			info.Action = "browse"
		} else if writes && slices.ContainsFunc(rt.Methods, writeMethod) {
			info.Action, info.Actions = "add", map[string]MethodAction{}
			for _, method := range rt.Methods {
				info.Actions[method] = MethodAction{"add", "edit"}
				if method == "DELETE" {
					info.Actions[method] = MethodAction{"delete", "delete"}
				}
			}
		}
		groups[1] = "{repository}/{path}"
	case ruleContent, ruleListing:
		// allowContent: the repository of the id in the path, or of the
		// query parameter repository of an upload
		info.Action = rt.Rule.action
		if rt.Rule.group > 0 {
			groups[rt.Rule.group] = "{asset}"
		} else {
			query = "?repository={repository}"
		}
	}
	if h, ok := routeHints[info.Pattern]; ok {
		info.Format, info.Refuses = h.format, h.refuses
		if len(h.headers) > 0 {
			info.Headers = h.headers
		}
		for group, text := range h.groups {
			groups[group] = text
		}
	}
	info.Examples = examplesOf(rt.Pattern, groups)
	for i := range info.Examples {
		info.Examples[i] += query
	}
	return info
}

// examplesOf are the paths a pattern matches, one per alternative, with the
// groups named in groups replaced by their text.
func examplesOf(re *regexp.Regexp, groups map[int]string) []string {
	tree, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return nil // the patterns of the table compiled already; the test of the list fails on an entry without examples
	}
	return expand(tree, groups)
}

func expand(re *syntax.Regexp, groups map[int]string) []string {
	switch re.Op {
	case syntax.OpLiteral:
		return []string{string(re.Rune)}
	case syntax.OpCapture:
		if text, ok := groups[re.Cap]; ok {
			return []string{text}
		}
		return expand(re.Sub[0], groups)
	case syntax.OpConcat:
		out := []string{""}
		for _, sub := range re.Sub {
			var next []string
			for _, head := range out {
				for _, tail := range expand(sub, groups) {
					next = append(next, head+tail)
				}
			}
			out = next
		}
		return out
	case syntax.OpAlternate:
		var out []string
		for _, sub := range re.Sub {
			out = append(out, expand(sub, groups)...)
		}
		return out
	case syntax.OpPlus:
		return []string{word(re.Sub[0])}
	case syntax.OpRepeat:
		return []string{strings.Repeat(word(re.Sub[0]), max(re.Min, 1))[:max(re.Min, 1)]}
	case syntax.OpCharClass, syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		return []string{word(re)[:1]}
	}
	// empty matches, anchors, optional and repeated parts: nothing
	return []string{""}
}

// word is "demo" when each of its letters matches re, a single character
// class; else the first character re takes.
func word(re *syntax.Regexp) string {
	takes := func(r rune) bool {
		switch re.Op {
		case syntax.OpAnyChar:
			return true
		case syntax.OpAnyCharNotNL:
			return r != '\n'
		case syntax.OpLiteral:
			return slices.Contains(re.Rune, r)
		case syntax.OpCharClass:
			for i := 0; i+1 < len(re.Rune); i += 2 {
				if re.Rune[i] <= r && r <= re.Rune[i+1] {
					return true
				}
			}
		}
		return false
	}
	if takes('d') && takes('e') && takes('m') && takes('o') {
		return "demo"
	}
	switch re.Op {
	case syntax.OpLiteral:
		return string(re.Rune)
	case syntax.OpCharClass:
		if len(re.Rune) > 0 {
			return string(re.Rune[0])
		}
	}
	return "x"
}
