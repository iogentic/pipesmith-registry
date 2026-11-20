package importer

import (
	"fmt"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The routing rules of the content set (task 4 of the phase 5 plan). The
// table and its columns follow the public source of the old product, tag
// release-3.87.2-01, read 2026-11-19: RoutingRuleDAO.xml creates
// repository_routing_rule with id, name, mode, description and matchers
// (JSON), and ConfigurationDAO.xml gives each repository the column
// routing_rule_id, without a foreign key. The rules are no record kind of
// state.Kinds: state keeps them in tables of their own (decision P5-1).

// routingTable is the table of the routing rules. An export program of
// release 0.1.1 did not write it, so a manifest of the content set may leave
// it out at its place.
const routingTable = "REPOSITORY_ROUTING_RULE"

// optionalTables are the tables an export program of an earlier release
// did not write: the routing rules, and the cleanup policies, capabilities
// and tasks (administration.go).
var optionalTables = []string{routingTable, policyTable, capabilityTable, jobTable, triggerTable}

// listedTables are the tables of a set that the files of a manifest can
// hold: all of them, without each optional table no file names.
func listedTables(tables []table, files []manifestFile) []table {
	return slices.DeleteFunc(slices.Clone(tables), func(t table) bool {
		return slices.Contains(optionalTables, t.name) &&
			!slices.ContainsFunc(files, func(f manifestFile) bool { return f.Table == t.name })
	})
}

// mapRouting reads the routing rules and the rule each repository names by
// its routing_rule_id, which the importer of 0.1.1 dropped. An export
// without the routing table holds no rules: each repository that names a
// rule is listed in RoutingWithoutRules and keeps a link to its rule id,
// which no rule has, so that the routing package blocks every path of it
// until an export with the table is imported (fail closed, finding 5-3 of
// review 5); without such a repository Routing stays nil. A rule id that
// the export does not hold is refused with the repository named.
func mapRouting(export *Export, data map[string][]row) error {
	repositories, withRepositories := data["REPOSITORY"]
	if !withRepositories {
		return nil
	}
	file := strings.ToLower(routingTable) + ".jsonl"
	ruleRows, withRules := data[routingTable]
	names := map[string]string{} // rule id to rule name
	var routing *model.Routing
	if withRules {
		routing = &model.Routing{Rules: []model.RoutingRule{}, Links: map[string]string{}}
		err := eachRow(file, ruleRows, func(rd *reader) error {
			id := rd.text("id")
			r := model.RoutingRule{Name: rd.text("name"), Description: rd.text("description"), Mode: rd.text("mode"),
				Matchers: rd.names("matchers")}
			switch {
			case rd.err != nil:
				return nil // eachRow returns the field error
			case id == "":
				return fmt.Errorf("%s: row %d has no id", rd.file, rd.row)
			case r.Name == "":
				return fmt.Errorf("%s: row %d has no name", rd.file, rd.row)
			case slices.ContainsFunc(routing.Rules, func(x model.RoutingRule) bool { return x.Name == r.Name }):
				return fmt.Errorf("%s: routing rule %q is given twice", rd.file, r.Name)
			}
			names[id] = r.Name
			routing.Rules = append(routing.Rules, r)
			return nil
		})
		if err != nil {
			return err
		}
		slices.SortFunc(routing.Rules, func(a, b model.RoutingRule) int { return strings.Compare(a.Name, b.Name) })
	}
	err := eachRow("repository.jsonl", repositories, func(rd *reader) error {
		id, name := rd.text("routing_rule_id"), rd.text("name")
		switch {
		case id == "":
		case !withRules:
			export.RoutingWithoutRules = append(export.RoutingWithoutRules, name)
			if routing == nil {
				routing = &model.Routing{Rules: []model.RoutingRule{}, Links: map[string]string{}}
			}
			routing.Links[name] = id
		case names[id] == "":
			return fmt.Errorf("%s: repository %q names the routing rule %s, which %s does not hold", rd.file, name, id, file)
		default:
			routing.Links[name] = names[id]
		}
		return nil
	})
	slices.Sort(export.RoutingWithoutRules)
	export.Routing = routing
	return err
}

// writeRouting writes the routing rules of an import into the tables of
// schema step 3 and reports whether there were any to keep.
func writeRouting(st *state.Store, routing *model.Routing) (bool, error) {
	if routing == nil {
		return false, nil
	}
	if err := st.WriteRouting(*routing); err != nil {
		return false, fmt.Errorf("state.sqlite holds the import, but its routing rules were not written: %w", err)
	}
	return true, nil
}
