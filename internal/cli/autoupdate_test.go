package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/channel"
	"github.com/iogentic/pipesmith-registry/internal/release"
)

// setKey sets the member key of update.json of the fixture to the JSON text.
func (f *fetchFixture) setKey(t *testing.T, key, value string) {
	t.Helper()
	path := filepath.Join(f.root, release.ConfigFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg[key] = json.RawMessage(value)
	data, _ = json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fetchFixture) lastRun(t *testing.T) *release.LastRun {
	t.Helper()
	r, err := release.ReadLastRun(f.root)
	if err != nil || r == nil {
		t.Fatalf("scheduled.json: %v %v", r, err)
	}
	return r
}

func (f *fetchFixture) history(t *testing.T) []release.HistoryEntry {
	t.Helper()
	h, err := release.History(filepath.Join(f.root, release.ReleasesDir), 50)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// at sets the clock of the scheduled run.
func at(t *testing.T, now time.Time) {
	t.Helper()
	old := scheduledNow
	scheduledNow = func() time.Time { return now }
	t.Cleanup(func() { scheduledNow = old })
}

func TestScheduledRunRecordsItsOutcome(t *testing.T) {
	f, svc := newScheduledFixture(t, "true", "0.2.0")
	code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 0 || svc.restarts != 1 {
		t.Fatalf("%d %q\n%s", code, errOut, out)
	}
	if r := f.lastRun(t); r.Result != release.RunSwitched || r.Version != "0.2.0" || r.Time == "" {
		t.Fatalf("last run %+v", r)
	}
	h := f.history(t)
	if len(h) != 1 || h[0].Result != release.ResultOK || !h[0].Auto {
		t.Fatalf("history %+v", h)
	}
	code, out, _ = run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if r := f.lastRun(t); code != 0 || r.Result != release.RunNothing {
		t.Fatalf("second run %d %+v\n%s", code, r, out)
	}
}

func TestScheduledFailedSwitchGoesBackByItself(t *testing.T) {
	f, svc := newScheduledFixture(t, "true", "0.2.0")
	svc.sick["0.2.0"] = true
	code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 1 || f.current(t) != "releases/0.1.0" {
		t.Fatalf("%d %q, current %s\n%s", code, errOut, f.current(t), out)
	}
	r := f.lastRun(t)
	if r.Result != release.RunBack || r.Version != "0.2.0" || r.Message == "" {
		t.Fatalf("last run %+v", r)
	}
	h := f.history(t)
	if len(h) == 0 || h[0].Result != release.ResultBack || !h[0].Auto {
		t.Fatalf("history %+v", h)
	}
}

func TestScheduledSwitchWaitsForTheWindow(t *testing.T) {
	f, svc := newScheduledFixture(t, "true", "0.2.0")
	f.setKey(t, "window", `"02:00-04:00"`)
	at(t, time.Date(2026, 11, 21, 13, 0, 0, 0, time.Local))
	code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 0 || svc.restarts != 0 || !f.staged(t, "0.2.0") || f.current(t) != "releases/0.1.0" {
		t.Fatalf("%d %q, %d restarts\n%s", code, errOut, svc.restarts, out)
	}
	if !strings.Contains(out, "window: 13:00 is outside 02:00-04:00, nothing restarted") {
		t.Fatalf("output:\n%s", out)
	}
	if r := f.lastRun(t); r.Result != release.RunWaiting || r.Version != "0.2.0" {
		t.Fatalf("last run %+v", r)
	}
	at(t, time.Date(2026, 11, 22, 3, 10, 0, 0, time.Local))
	code, out, errOut = run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 0 || svc.restarts != 1 || f.current(t) != "releases/0.2.0" {
		t.Fatalf("inside the window: %d %q, %d restarts\n%s", code, errOut, svc.restarts, out)
	}
	if r := f.lastRun(t); r.Result != release.RunSwitched {
		t.Fatalf("last run %+v", r)
	}
}

func TestScheduledRunRefusesABadWindow(t *testing.T) {
	f, svc := newScheduledFixture(t, "true", "0.2.0")
	f.setKey(t, "window", `"2-4"`)
	code, _, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 1 || !strings.Contains(errOut, "window") || svc.restarts != 0 || f.staged(t, "0.2.0") {
		t.Fatalf("%d %q", code, errOut)
	}
	if r := f.lastRun(t); r.Result != release.RunFailed {
		t.Fatalf("last run %+v", r)
	}
}

func TestScheduledRunWithTheSwitchOffStages(t *testing.T) {
	f, _ := newScheduledFixture(t, "false", "0.2.0")
	if code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root); code != 0 {
		t.Fatalf("%d %q\n%s", code, errOut, out)
	}
	if r := f.lastRun(t); r.Result != release.RunStaged || r.Version != "0.2.0" {
		t.Fatalf("last run %+v", r)
	}
}

func TestFetchInstall(t *testing.T) {
	f, svc := newScheduledFixture(t, "", "0.2.0", "0.3.0")
	code, out, errOut := run(t, "update", "--fetch", "--install", "--version", "0.2.0", "--root", f.root)
	if code != 0 || svc.restarts != 1 || f.current(t) != "releases/0.2.0" {
		t.Fatalf("named: %d %q, %d restarts\n%s", code, errOut, svc.restarts, out)
	}
	code, out, errOut = run(t, "update", "--fetch", "--install", "--version", "latest", "--root", f.root)
	if code != 0 || svc.restarts != 2 || f.current(t) != "releases/0.3.0" {
		t.Fatalf("latest: %d %q, %d restarts\n%s", code, errOut, svc.restarts, out)
	}
	h := f.history(t)
	if len(h) != 2 || h[0].Auto || h[0].To != "0.3.0" {
		t.Fatalf("history %+v", h)
	}
	if _, err := release.ReadLastRun(f.root); err != nil {
		t.Fatal(err)
	} else if r, _ := release.ReadLastRun(f.root); r != nil {
		t.Fatalf("a run by hand wrote scheduled.json: %+v", r)
	}
	code, out, _ = run(t, "update", "--fetch", "--install", "--root", f.root)
	if code != 0 || svc.restarts != 2 || !strings.Contains(out, "nothing to do") {
		t.Fatalf("nothing: %d %d\n%s", code, svc.restarts, out)
	}
}

func TestFetchInstallGoesBack(t *testing.T) {
	f, svc := newScheduledFixture(t, "", "0.2.0")
	svc.sick["0.2.0"] = true
	code, out, errOut := run(t, "update", "--fetch", "--install", "--root", f.root)
	if code != 1 || f.current(t) != "releases/0.1.0" {
		t.Fatalf("%d %q, current %s\n%s", code, errOut, f.current(t), out)
	}
	if h := f.history(t); len(h) == 0 || h[0].Result != release.ResultBack || h[0].Auto {
		t.Fatalf("history %+v", h)
	}
}

func TestFetchInstallUsage(t *testing.T) {
	dataDir(t)
	for args, want := range map[string]string{
		"update --install dir":                 "--install needs --fetch",
		"update --fetch --install --scheduled": "--install and --scheduled",
	} {
		code, _, errOut := run(t, strings.Fields(args)...)
		if code != 2 || !strings.Contains(errOut, want) {
			t.Errorf("%s: %d %q", args, code, errOut)
		}
	}
}

func TestFetchInstallTakesOverTheDueMarkAndMarksAFailure(t *testing.T) {
	f, svc := newScheduledFixture(t, "true", "0.2.0")
	f.setKey(t, "window", `"02:00-04:00"`)
	at(t, time.Date(2026, 11, 21, 13, 0, 0, 0, time.Local))
	if code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root); code != 0 || f.lastRun(t).Result != release.RunWaiting {
		t.Fatalf("%d %q\n%s", code, errOut, out)
	}
	svc.sick["0.2.0"] = true
	if code, out, errOut := run(t, "update", "--fetch", "--install", "--root", f.root); code != 1 {
		t.Fatalf("install: %d %q\n%s", code, errOut, out)
	}
	if due, err := channel.SwitchDue(f.root, "0.2.0"); due || err != nil {
		t.Fatalf("the due mark stayed: %v %v", due, err)
	}
	if failed, _, err := channel.SwitchFailed(f.root, "0.2.0"); !failed || err != nil {
		t.Fatalf("no mark of the failed switch: %v %v", failed, err)
	}
	svc.sick["0.2.0"] = false
	at(t, time.Date(2026, 11, 22, 3, 0, 0, 0, time.Local))
	code, out, _ := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 1 || f.current(t) != "releases/0.1.0" || !strings.Contains(out+f.lastRun(t).Message, "does not try it again") {
		t.Fatalf("the timer tried the failed release again: %d, current %s\n%s", code, f.current(t), out)
	}
}

func TestScheduledRunNamesNoVersionWithNothingNew(t *testing.T) {
	f, _ := newScheduledFixture(t, "false", "0.2.0")
	if code, _, _ := run(t, "update", "--fetch", "--scheduled", "--root", f.root); code != 0 {
		t.Fatal(code)
	}
	if code, _, _ := run(t, "update", "--fetch", "--scheduled", "--root", f.root); code != 0 {
		t.Fatal(code)
	}
	if r := f.lastRun(t); r.Result != release.RunNothing || r.Version != "" {
		t.Fatalf("second run %+v", r)
	}
}
