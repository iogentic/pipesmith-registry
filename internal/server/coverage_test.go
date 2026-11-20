package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// coverageFile is written by tools/ui_coverage.py from docs/ui/API.md and
// docs/ui/INVENTORY.md (task 1 of the phase 6 plan).
const coverageFile = "../../testdata/ui/coverage.json"

// coverageSources are the files the generator reads and the generator
// itself, as coverage.json names them, by their path from this folder.
var coverageSources = map[string]string{
	"docs/ui/API.md":       "../../docs/ui/API.md",
	"docs/ui/INVENTORY.md": "../../docs/ui/INVENTORY.md",
	"tools/ui_coverage.py": "../../tools/ui_coverage.py",
}

type coverage struct {
	Sources map[string]string `json:"sources"`
	Digest  string            `json:"digest"`
	Rows    []struct {
		ID      string   `json:"id"`
		Capture string   `json:"capture"`
		Status  string   `json:"status"`
		Screens []string `json:"screens"`
		Groups  []string `json:"groups"`
	} `json:"rows"`
	Groups         map[string][]string `json:"groups"`
	Counts         map[string]int      `json:"counts"`
	Total          int                 `json:"total"`
	Served         map[string][]string `json:"served"`
	Contradictions []struct {
		ID     string `json:"id"`
		Source string `json:"source"`
		Text   string `json:"text"`
	} `json:"contradictions"`
}

var coverageDigest = regexp.MustCompile(`"digest": "([0-9a-f]{64})"`)

func readCoverage(t *testing.T) (coverage, []byte) {
	t.Helper()
	data, err := os.ReadFile(coverageFile)
	if err != nil {
		t.Fatalf("testdata/ui/coverage.json: %v", err)
	}
	var c coverage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("testdata/ui/coverage.json: %v", err)
	}
	return c, data
}

// servedByTable are the requests whose first entry of the route table, by
// method and pattern, is not a refusal: the requests the table serves.
func servedByTable(routes []RouteInfo, requests map[string]bool) map[string]bool {
	served := map[string]bool{}
	for key := range requests {
		method, path, _ := strings.Cut(key, " ")
		target := filled(path)
		for _, r := range routes {
			if slices.Contains(r.Methods, method) && regexp.MustCompile(r.Pattern).MatchString(target) {
				if r.Table != "refusals" {
					served[key] = true
				}
				break
			}
		}
	}
	return served
}

// coverage.json is the output of the generator over the files taken at
// docs/ui: the digest of the file and of each source is what it records.
func TestCoverageIsTheOutputOfTheGenerator(t *testing.T) {
	c, data := readCoverage(t)
	m := coverageDigest.FindSubmatchIndex(data)
	if m == nil {
		t.Fatal("coverage.json holds no digest")
	}
	zeroed := slices.Concat(data[:m[2]], bytes.Repeat([]byte("0"), 64), data[m[3]:])
	if sum := sha256.Sum256(zeroed); hex.EncodeToString(sum[:]) != c.Digest {
		t.Errorf("coverage.json has the digest %s, its content %s: run python3 tools/ui_coverage.py", c.Digest, hex.EncodeToString(sum[:]))
	}
	if len(c.Sources) != len(coverageSources) {
		t.Errorf("coverage.json names %d sources, want %d", len(c.Sources), len(coverageSources))
	}
	for name, file := range coverageSources {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(source); hex.EncodeToString(sum[:]) != c.Sources[name] {
			t.Errorf("%s changed since coverage.json was written: run python3 tools/ui_coverage.py", name)
		}
	}
}

// Rulings U1, U5 and U6: the 83 rows are S01 to S83 in order, each with its
// capture; every request of the contract is in exactly one group; the
// requests are those of ui-contract-requests.txt; each refused request is
// in the group of its area.
func TestCoverageGroupsEveryRequestOnce(t *testing.T) {
	c, _ := readCoverage(t)
	if len(c.Rows) != 83 {
		t.Errorf("%d rows, want 83", len(c.Rows))
	}
	captures := map[string]bool{}
	for i, row := range c.Rows {
		if want := fmt.Sprintf("S%02d", i+1); row.ID != want {
			t.Errorf("row %d has the id %s, want %s", i+1, row.ID, want)
		}
		if captures[row.Capture] || !regexp.MustCompile(`^[0-9]{2}$`).MatchString(row.Capture) {
			t.Errorf("%s: capture %q is not a new number of two digits", row.ID, row.Capture)
		}
		captures[row.Capture] = true
		for _, g := range row.Groups {
			if _, ok := c.Groups[g]; !ok {
				t.Errorf("%s names the group %s, which coverage.json does not hold", row.ID, g)
			}
		}
	}
	contract := contractRequests(t)
	count := map[string]int{}
	group := map[string]string{}
	names := make([]string, 0, len(c.Groups))
	for name, requests := range c.Groups {
		names = append(names, name)
		if c.Counts[name] != len(requests) {
			t.Errorf("group %s: count %d, %d requests", name, c.Counts[name], len(requests))
		}
		for _, r := range requests {
			count[r]++
			group[r] = name
		}
	}
	for r := range contract {
		if count[r] != 1 {
			t.Errorf("%s is in %d groups, want 1", r, count[r])
		}
	}
	for r := range count {
		if !contract[r] {
			t.Errorf("%s is in a group but not in ui-contract-requests.txt", r)
		}
	}
	if len(count) != len(contract) || c.Total != len(contract) {
		t.Errorf("%d requests in the groups, total %d, ui-contract-requests.txt %d", len(count), c.Total, len(contract))
	}
	for _, d := range slices.Concat(writeRefusals, refusalTable) {
		for _, method := range d.methods {
			for _, path := range d.paths() {
				if key := method + " " + path; contract[key] && group[key] != d.area {
					t.Errorf("%s is refused with the area %s but is in the group %s", key, d.area, group[key])
				}
			}
		}
	}
	sort.Strings(names)
	for _, name := range names {
		t.Logf("%-16s %3d", name, c.Counts[name])
	}
	t.Logf("%-16s %3d requests in %d groups, %d rows", "total", c.Total, len(names), len(c.Rows))
}

// Ruling U4: the served list of coverage.json is the set of contract
// requests the route table with writes on serves, and each served request
// is listed in its own group.
func TestCoverageServedIsTheRouteTable(t *testing.T) {
	c, _ := readCoverage(t)
	contract := contractRequests(t)
	byTable := servedByTable(Routes(true), contract)
	listed := map[string]bool{}
	for name, requests := range c.Served {
		for _, r := range requests {
			if listed[r] {
				t.Errorf("%s is served twice", r)
			}
			listed[r] = true
			if !slices.Contains(c.Groups[name], r) {
				t.Errorf("%s is served in the group %s, which does not hold it", r, name)
			}
			if !byTable[r] {
				t.Errorf("%s is listed as served, but no entry of Routes(true) serves it", r)
			}
		}
	}
	for r := range byTable {
		if !listed[r] {
			t.Errorf("%s is served by Routes(true), but coverage.json does not list it", r)
		}
	}
	names := make([]string, 0, len(c.Groups))
	for name := range c.Groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Logf("served %-16s %3d of %3d", name, len(c.Served[name]), c.Counts[name])
	}
	t.Logf("served %-16s %3d of %3d", "total", len(listed), len(contract))
	if len(listed) == 0 {
		t.Error("coverage.json lists no served request")
	}
}

// The list of contradictions is there and every entry names its source.
func TestCoverageListsItsContradictions(t *testing.T) {
	c, _ := readCoverage(t)
	if len(c.Contradictions) == 0 {
		t.Fatal("coverage.json lists no contradiction")
	}
	for i, k := range c.Contradictions {
		if k.ID != fmt.Sprintf("K%d", i+1) || k.Source == "" || k.Text == "" {
			t.Errorf("contradiction %d is %+v", i+1, k)
		}
	}
	t.Logf("%d contradictions", len(c.Contradictions))
}
