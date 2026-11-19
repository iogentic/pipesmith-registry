package main

import (
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// NOTICE names how many third party modules each binary of tools/go.sh build
// links (task 13 of the phase 3 plan). The count there is the number of dep
// lines of go version -m; this test counts again with go list -deps, which
// names the modules of the packages the binary is built from, and compares.

var noticeCount = regexp.MustCompile(`(?m)^  (darwin|linux)/(amd64|arm64)  (\d+)$`)

func TestNoticeCountsTheLinkedModules(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command; tools/go.sh check runs this test in its container")
	}
	data, err := os.ReadFile("../../NOTICE")
	if err != nil {
		t.Fatal(err)
	}
	written := map[string]int{}
	for _, m := range noticeCount.FindAllStringSubmatch(string(data), -1) {
		n, _ := strconv.Atoi(m[3])
		written[m[1]+"/"+m[2]] = n
	}
	for _, target := range []string{"darwin/arm64", "linux/amd64", "linux/arm64"} {
		goos, goarch, _ := strings.Cut(target, "/")
		cmd := exec.Command("go", "list", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}", ".")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list for %s: %v", target, err)
		}
		var modules []string
		for _, line := range strings.Fields(string(out)) {
			if !slices.Contains(modules, line) {
				modules = append(modules, line)
			}
		}
		if got, ok := written[target]; !ok || got != len(modules) {
			t.Errorf("NOTICE names %d modules for %s (found: %v), go list -deps counts %d: %s", got, target, ok, len(modules),
				strings.Join(modules, " "))
		}
	}
}
