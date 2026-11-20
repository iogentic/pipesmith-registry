package server

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// The listing of the route table for the walk over every route (task 19 of
// the phase 3 plan) holds every entry of the table with security on once,
// with writes off and with writes on, and each of its examples reaches that
// very entry.

// listed are the two tables the listing covers: writes off and writes on.
var listed = []struct {
	writes bool
	table  []route
}{{false, securedRoutes}, {true, writingRoutes}}

// fill puts sample values into the placeholders of an example.
func fill(example, format string) string {
	repo := map[string]string{"": "demo-maven", "npm": "demo-npm", "docker": "demo-docker"}[format]
	path := strings.NewReplacer("{repository}", repo, "{asset}", rest.AssetID(repo, "demo/file.txt"), "{path}", "demo/file.txt",
		"{token}", "NpmToken.AAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA").Replace(example)
	path, _, _ = strings.Cut(path, "?") // the router matches the path; a query names a repository
	return path
}

var examplePlaceholder = regexp.MustCompile(`\{[a-z]+\}`)

func TestRouteListCoversTheTable(t *testing.T) {
	for _, l := range listed {
		t.Run(fmt.Sprintf("writes %v", l.writes), func(t *testing.T) { coversTheTable(t, Routes(l.writes), l.table, l.writes) })
	}
}

func coversTheTable(t *testing.T, list []RouteInfo, table []route, writes bool) {
	if len(list) != len(table) {
		t.Fatalf("%d entries listed, the table holds %d", len(list), len(table))
	}
	for i, info := range list {
		rt := table[i]
		if info.Pattern != rt.Pattern.String() || !slices.Equal(info.Methods, rt.Methods) {
			t.Fatalf("entry %d is %s %v, the table holds %s %v", i, info.Pattern, info.Methods, rt.Pattern, rt.Methods)
		}
		if len(info.Examples) == 0 {
			t.Errorf("%s has no example", info.Pattern)
		}
		switch info.Rule {
		case "public", "signed-in":
		case "application":
			if len(info.Permissions) == 0 {
				t.Errorf("%s: an application rule without permissions", info.Pattern)
			}
		case "view", "admin", "asset", "client", "content", "create", "listing":
			if info.Action == "" {
				t.Errorf("%s: a %s rule without an action", info.Pattern, info.Rule)
			}
		default:
			t.Errorf("%s: rule %q", info.Pattern, info.Rule)
		}
		// with writes on every write below /repository/ names the actions
		// of writeAction, method by method; with writes off it asks read
		wantActions := map[string]MethodAction(nil)
		if writes && rt.Rule.kind == ruleClient && slices.Contains(rt.Methods, "PUT") {
			wantActions = map[string]MethodAction{"PUT": {"add", "edit"}, "POST": {"add", "edit"}, "PATCH": {"add", "edit"},
				"DELETE": {"delete", "delete"}}
		}
		if !reflect.DeepEqual(info.Actions, wantActions) {
			t.Errorf("%s %v: actions %v, want %v", info.Pattern, info.Methods, info.Actions, wantActions)
		}
		for _, example := range info.Examples {
			for _, name := range examplePlaceholder.FindAllString(example, -1) {
				if _, ok := RoutePlaceholders[name]; !ok {
					t.Errorf("%s: the placeholder %s of %s is not explained", info.Pattern, name, example)
				}
			}
			path := fill(example, info.Format)
			if !rt.Pattern.MatchString(path) {
				t.Errorf("the example %s of %s does not match it", path, info.Pattern)
				continue
			}
			// the first entry that takes the path and a method of this one is
			// this one, so a request of the example reaches its rule. An
			// earlier entry of the same rule may take it instead: the walk
			// then checks the same rule, and the log names the pair.
			for _, method := range info.Methods {
				first := slices.IndexFunc(table, func(r route) bool {
					return r.Pattern.MatchString(path) && slices.Contains(r.Methods, method)
				})
				switch {
				case first == i:
				case first >= 0 && first < i && reflect.DeepEqual(table[first].Rule, rt.Rule):
					t.Logf("%s %s reaches entry %d (%s) before entry %d (%s), under the same rule", method, path, first,
						table[first].Pattern, i, info.Pattern)
				default:
					t.Errorf("%s %s reaches entry %d, not %d (%s)", method, path, first, i, info.Pattern)
				}
			}
		}
	}
}

// The rule of an entry is read from the table: its kind, permissions, group
// and action.
func TestRouteListReadsTheRules(t *testing.T) {
	for _, l := range listed {
		list := Routes(l.writes)
		for i, info := range list {
			r := l.table[i].Rule
			want := map[ruleKind]string{rulePublic: "public", ruleSignedIn: "signed-in", ruleApp: "application", ruleView: "view",
				ruleAdmin: "admin", ruleAssetID: "asset", ruleClient: "client", ruleContent: "content", ruleCreate: "create",
				ruleListing: "listing"}[r.kind]
			if info.Rule != want || !slices.Equal(info.Permissions, r.permissions) {
				t.Errorf("%s: rule %s %v, the table has %s %v", info.Pattern, info.Rule, info.Permissions, want, r.permissions)
			}
			if (r.kind == ruleView || r.kind == ruleContent || r.kind == ruleListing) && info.Action != r.action {
				t.Errorf("%s: action %s, the table has %s", info.Pattern, info.Action, r.action)
			}
			for _, example := range info.Examples {
				hasRepository := strings.Contains(example, "{repository}") || strings.Contains(example, "{asset}")
				if (r.kind == ruleView || r.kind == ruleAdmin || r.kind == ruleAssetID || r.kind == ruleClient || r.kind == ruleContent ||
					r.kind == ruleListing) !=
					hasRepository && info.Format == "" {
					t.Errorf("%s: the example %s names the repository of the rule %s wrongly", info.Pattern, example, info.Rule)
				}
			}
		}
		hinted := 0
		for _, info := range list {
			if info.Format != "" || info.Refuses != "" || len(info.Headers) > 0 {
				hinted++
			}
		}
		if hinted != len(routeHints) {
			t.Fatalf("writes %v: %d entries carry a hint, %d hints exist", l.writes, hinted, len(routeHints))
		}
	}
	// every hint names a pattern of the table, so a changed pattern cannot
	// leave its hint behind
	for pattern := range routeHints {
		if !slices.ContainsFunc(securedRoutes, func(r route) bool { return r.Pattern.String() == pattern }) {
			t.Errorf("the hint of %s names no pattern of the table", pattern)
		}
	}
}

// With writes on the REST writes are entries of their own with the rule of
// rest_write.go; the upload names its repository in the query.
func TestRouteListNamesTheRESTWrites(t *testing.T) {
	byKey := map[string]RouteInfo{}
	for _, info := range Routes(true) {
		if info.Rule == "content" {
			byKey[strings.Join(info.Methods, ",")+" "+info.Pattern] = info
		}
	}
	upload := byKey["POST "+exact(restBase+"/components").String()]
	if upload.Table != "writes" || upload.Action != "add" ||
		!slices.Equal(upload.Examples, []string{restBase + "/components?repository={repository}"}) {
		t.Errorf("upload %+v", upload)
	}
	for _, name := range []string{"assets", "components"} {
		del := byKey["DELETE "+exact(restBase+"/"+name+"/"+segment).String()]
		if del.Table != "writes" || del.Action != "delete" || !slices.Equal(del.Examples, []string{restBase + "/" + name + "/{asset}"}) {
			t.Errorf("%s delete %+v", name, del)
		}
	}
	if len(byKey) != 3 {
		t.Fatalf("%d entries of the content rule: %v", len(byKey), byKey)
	}
	for _, info := range Routes(false) {
		if info.Rule == "content" || len(info.Actions) > 0 {
			t.Errorf("writes off: %+v", info)
		}
	}
}
