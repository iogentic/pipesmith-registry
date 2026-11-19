//go:build linux || darwin

package release

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSystemctl puts a systemctl on PATH that appends its arguments to a
// file and fails for the verb fail.
func fakeSystemctl(t *testing.T) string { return fakeSystemctlFailing(t, "fail") }

// fakeSystemctlFailing fails for the verb given.
func fakeSystemctlFailing(t *testing.T, verb string) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\n[ \"$1\" != " + verb + " ]\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func readCalls(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A restart of an update or a rollback is not refused by the start limit
// that earlier restarts or a failed release left: it resets the failed
// state of the unit first.
func TestSystemctlRestartResetsTheStartLimit(t *testing.T) {
	calls := fakeSystemctl(t)
	if err := (Systemctl{}).Restart(context.Background(), "demo.service"); err != nil {
		t.Fatal(err)
	}
	if got := readCalls(t, calls); got != "reset-failed -- demo.service\nrestart -- demo.service\n" {
		t.Fatalf("calls:\n%s", got)
	}
}

// A unit that is stopped and not failed is not loaded, and reset-failed
// fails for it; the restart still runs.
func TestSystemctlRestartAfterAFailedReset(t *testing.T) {
	calls := fakeSystemctlFailing(t, "reset-failed")
	if err := (Systemctl{}).Restart(context.Background(), "demo.service"); err != nil {
		t.Fatal(err)
	}
	if got := readCalls(t, calls); got != "reset-failed -- demo.service\nrestart -- demo.service\n" {
		t.Fatalf("calls:\n%s", got)
	}
	calls = fakeSystemctlFailing(t, "restart")
	if err := (Systemctl{}).Restart(context.Background(), "demo.service"); err == nil || !strings.Contains(err.Error(), "systemctl restart demo.service") {
		t.Fatalf("a failed restart: %v", err)
	}
}

func TestSystemctlStop(t *testing.T) {
	calls := fakeSystemctl(t)
	if err := (Systemctl{}).Stop(context.Background(), "demo.service"); err != nil {
		t.Fatal(err)
	}
	if got := readCalls(t, calls); got != "stop -- demo.service\n" {
		t.Fatalf("calls:\n%s", got)
	}
	if err := (Systemctl{}).Restart(context.Background(), "bad unit"); err == nil || !strings.Contains(err.Error(), "not a plain service unit name") {
		t.Fatalf("bad unit: %v", err)
	}
}
