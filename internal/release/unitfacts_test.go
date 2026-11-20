//go:build linux || darwin

package release

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// factsService is the fake service manager with the settings of its unit.
type factsService struct {
	*fakeService
	facts UnitFacts
}

func (f *factsService) UnitFacts(context.Context, string) (UnitFacts, error) { return f.facts, nil }

// unitRig is a rig whose service manager reports facts.
func unitRig(t *testing.T, facts UnitFacts) *rig {
	t.Helper()
	r := newRig(t, "0.1.0")
	r.u.Service = &factsService{fakeService: r.svc, facts: facts}
	return r
}

const showAnswer = `Environment=PIPESMITH_REGISTRY_DATA=/var/lib/pipesmith-registry PIPESMITH_REGISTRY_DRAIN=300 "X=a b"
ExecStart={ path=/opt/pipesmith-registry/current/pipesmith-registry ; argv[]=/opt/pipesmith-registry/current/pipesmith-registry serve --port 14193 ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }
TimeoutStopUSec=5min 30s
`

func TestParseShow(t *testing.T) {
	f, err := parseShow(showAnswer)
	if err != nil {
		t.Fatal(err)
	}
	want := UnitFacts{Env: map[string]string{"PIPESMITH_REGISTRY_DATA": "/var/lib/pipesmith-registry", "PIPESMITH_REGISTRY_DRAIN": "300", "X": "a b"},
		Args:        []string{"/opt/pipesmith-registry/current/pipesmith-registry", "serve", "--port", "14193"},
		StopTimeout: 330 * time.Second}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("facts %+v", f)
	}
	for text, d := range map[string]time.Duration{"infinity": 0, "1h 2min 3s 4ms": time.Hour + 2*time.Minute + 3*time.Second + 4*time.Millisecond} {
		if got, err := parseSpan(text); err != nil || got != d {
			t.Errorf("%q: %v %v", text, got, err)
		}
	}
	if _, err := parseShow("TimeoutStopUSec=5 parsecs\n"); err == nil {
		t.Fatal("a bad span passed")
	}
}

func TestSystemctlUnitFacts(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$*\" = 'show --property=Environment --property=ExecStart --property=TimeoutStopUSec -- demo.service' ] || exit 1\ncat <<'EOF'\n" + showAnswer + "EOF\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	f, err := (Systemctl{}).UnitFacts(context.Background(), "demo.service")
	if err != nil || f.StopTimeout != 330*time.Second || f.Env["PIPESMITH_REGISTRY_DRAIN"] != "300" {
		t.Fatalf("facts %+v %v", f, err)
	}
}

// A unit that stops before the drain time of serve ends is refused before
// any change (finding R-15).
func TestUnitStopsBeforeTheDrain(t *testing.T) {
	r := unitRig(t, UnitFacts{Env: map[string]string{"PIPESMITH_REGISTRY_DRAIN": "300"}, StopTimeout: 60 * time.Second})
	dir := r.release(versionScript("0.2.0"))
	err := r.unchanged(ErrConfig, func() error { return r.u.Update(dir, false) })
	if !strings.Contains(err.Error(), "before the drain of 300 s ends") {
		t.Fatalf("message %q", err)
	}
	r.unchanged(ErrConfig, r.u.Rollback)
	r.u.Service.(*factsService).facts.StopTimeout = 330 * time.Second
	if err := r.u.Update(dir, false); err != nil {
		t.Fatalf("a unit that stops after the drain: %v", err)
	}
}

// drain_seconds of update.json and PIPESMITH_REGISTRY_DRAIN of the unit must
// name the same time (finding A-11).
func TestDrainOfUnitAndConfig(t *testing.T) {
	r := unitRig(t, UnitFacts{Env: map[string]string{"PIPESMITH_REGISTRY_DRAIN": "300"}})
	r.config(`, "drain_seconds": 60`)
	dir := r.release(versionScript("0.2.0"))
	err := r.unchanged(ErrConfig, func() error { return r.u.Update(dir, false) })
	for _, w := range []string{"drain_seconds 60", "PIPESMITH_REGISTRY_DRAIN=300"} {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("message %q lacks %q", err, w)
		}
	}
	r.config(`, "drain_seconds": 300`)
	if err := r.u.Update(dir, false); err != nil {
		t.Fatalf("equal drain times: %v", err)
	}
}

// data_dir of update.json must be the data directory of the unit, so the
// schema plan reads the files the server opens (finding A-17).
func TestDataDirOfUnitAndConfig(t *testing.T) {
	r := unitRig(t, UnitFacts{Env: map[string]string{"PIPESMITH_REGISTRY_DATA": t.TempDir()}})
	dir := r.release(versionScript("0.2.0"))
	err := r.unchanged(ErrConfig, func() error { return r.u.Update(dir, false) })
	if !strings.Contains(err.Error(), "PIPESMITH_REGISTRY_DATA") || !strings.Contains(err.Error(), r.data) {
		t.Fatalf("message %q", err)
	}
	r.unchanged(ErrConfig, r.u.Rollback)
	link := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(r.data, link); err != nil {
		t.Fatal(err)
	}
	r.u.Service.(*factsService).facts.Env["PIPESMITH_REGISTRY_DATA"] = link + "/"
	if err := r.u.Update(dir, false); err != nil {
		t.Fatalf("the same directory through a link: %v", err)
	}
}

// The health wait asks the port the unit gives serve: a port of
// health_address or config.json that differs from --port or
// PIPESMITH_REGISTRY_PORT of the unit is refused (finding R-11).
func TestHealthPortOfTheUnit(t *testing.T) {
	r := unitRig(t, UnitFacts{Env: map[string]string{}})
	port := r.srv.URL[strings.LastIndex(r.srv.URL, ":")+1:]
	dir := r.release(versionScript("0.2.0"))
	facts := &r.u.Service.(*factsService).facts
	for _, c := range []struct {
		args []string
		env  string
	}{{[]string{"/x/pipesmith-registry", "serve", "--port", "1"}, ""}, {[]string{"/x/pipesmith-registry", "serve", "--port=2"}, ""}, {nil, "3"}} {
		facts.Args, facts.Env["PIPESMITH_REGISTRY_PORT"] = c.args, c.env
		if c.env == "" {
			delete(facts.Env, "PIPESMITH_REGISTRY_PORT")
		}
		err := r.unchanged(ErrConfig, func() error { return r.u.Update(dir, false) })
		if !strings.Contains(err.Error(), "port "+port) {
			t.Fatalf("%v: message %q", c, err)
		}
	}
	facts.Args = []string{"/x/pipesmith-registry", "serve", "--port", port}
	facts.Env["PIPESMITH_REGISTRY_PORT"] = "1"
	if err := r.u.Update(dir, false); err != nil {
		t.Fatalf("the port of --port: %v", err)
	}
}
