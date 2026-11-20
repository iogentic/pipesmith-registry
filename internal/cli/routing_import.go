package cli

import (
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/importer"
)

// reportRouting prints what an import did with the routing rules of its
// export (task 4 of the phase 5 plan).
func (c *command) reportRouting(r *importer.Result) {
	if r.Routing != nil && r.RoutingKept {
		c.print("replaced in %s: routing rules and their links", r.StatePath)
	}
	if len(r.RoutingWithoutRules) > 0 {
		c.print("note: %d repositories name a routing rule, but the export holds no routing rules, so every path of them"+
			" is blocked; export the content set again with the current tools/h2export.sh: %s", len(r.RoutingWithoutRules),
			strings.Join(r.RoutingWithoutRules, ", "))
	}
}
