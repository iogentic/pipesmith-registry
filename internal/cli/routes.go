package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/server"
)

// routes is a hidden command: it prints the route table of a server with
// security on, each entry with its rule and example requests, for the walk
// over every route (task 19 of the phase 3 plan, tests/identity); with
// --writes the table of a server with writes on. The help does not name it
// and the parser of the other commands does not know it, so their texts stay
// those of the reference. It reads no data directory.

const routesHelp = "usage: pipesmith-registry routes [-h] [--json] [--writes]\n" +
	"\n" +
	"options:\n" +
	"  -h, --help  show this help message and exit\n" +
	"  --json\n" +
	"  --writes\n"

func routesCommand(args []string, stdout, stderr io.Writer) int {
	p := newParser(program+" routes", true, routesHelp)
	p.add(&action{dest: "json", options: []string{"--json"}, nargs: "0", flag: true})
	p.add(&action{dest: "writes", options: []string{"--writes"}, nargs: "0", flag: true})
	values, err := p.parseArgs(args)
	if err != nil {
		var help *helpExit
		if errors.As(err, &help) {
			_, _ = io.WriteString(stdout, help.text)
			return 0
		}
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 2
	}
	list := server.Routes(values["writes"] == true)
	if values["json"] == true {
		data, err := json.MarshalIndent(struct {
			Placeholders map[string]string  `json:"placeholders"`
			Rules        map[string]string  `json:"rules"`
			Routes       []server.RouteInfo `json:"routes"`
		}{server.RoutePlaceholders, server.RouteRules, list}, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s\n", data)
		return 0
	}
	rows := [][]string{{"TABLE", "RULE", "METHODS", "EXAMPLE"}}
	for _, r := range list {
		rows = append(rows, []string{r.Table, r.Rule, strings.Join(r.Methods, ","), strings.Join(r.Examples, " ")})
	}
	if err := table(stdout, rows); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}
