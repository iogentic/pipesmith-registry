package server

import (
	"slices"
	"strings"
	"testing"
)

// restOperations are the pairs of method and pattern of the REST entries of
// a table, below /service/rest/, in table order.
func restOperations(table []route) []string {
	var out []string
	for _, rt := range table {
		if !strings.HasPrefix(rt.Pattern.String(), "^"+restBase) && !strings.HasPrefix(rt.Pattern.String(), "^/service/rest/") {
			continue
		}
		for _, m := range rt.Methods {
			out = append(out, m+" "+rt.Pattern.String())
		}
	}
	return out
}

// Task 2 of the phase 1c plan, as far as the repository knows the
// operations: the REST operations of the two tables with security, counted,
// each pair of method and pattern once, each with a rule (route.New refuses
// an entry without one). The 195 paths and 287 operations of the old product
// are not in the repository (PSR_REST_ORACLE); the counts here are those of
// the tables and change with every served or refused operation.
func TestTheRESTOperationsOfTheTables(t *testing.T) {
	for _, c := range []struct {
		name  string
		table []route
		want  int
	}{
		{"writes off", securedRoutes, restOperationsWritesOff},
		{"writes on", writingRoutes, restOperationsWritesOn},
	} {
		ops := restOperations(c.table)
		seen := map[string]bool{}
		for _, op := range ops {
			if seen[op] {
				t.Errorf("%s: %s twice", c.name, op)
			}
			seen[op] = true
		}
		if len(ops) != c.want {
			t.Errorf("%s: %d REST operations, want %d", c.name, len(ops), c.want)
		}
		for _, rt := range c.table {
			if rt.Rule.Empty() {
				t.Errorf("%s: %s has no rule", c.name, rt.Pattern)
			}
		}
	}
	// the listings and the searches of leg F2 are among them, with writes on and off
	for _, op := range []string{"GET " + exact(restBase+"/assets").String(), "GET " + exact(restBase+"/components").String(),
		"GET " + exact(restBase+"/components/"+segment).String(), "GET " + exact(restBase+"/search/assets").String(),
		"GET " + exact(restBase+"/search/assets/download").String()} {
		if !slices.Contains(restOperations(securedRoutes), op) || !slices.Contains(restOperations(writingRoutes), op) {
			t.Errorf("%s is not served", op)
		}
	}
}

// The counts of TestTheRESTOperationsOfTheTables (2026-11-20, leg F2).
const (
	restOperationsWritesOff = 76
	restOperationsWritesOn  = 77
)
