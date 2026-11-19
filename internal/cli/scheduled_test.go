package cli

import (
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// newScheduledFixture is the fetch fixture with the fake service manager in
// updateService and, unless key is empty, the member scheduled_switch of
// update.json set to the JSON text key.
func newScheduledFixture(t *testing.T, key string, versions ...string) (*fetchFixture, *cliService) {
	t.Helper()
	f := newFetchFixture(t, versions...)
	svc := &cliService{root: f.root, running: "0.1.0", sick: map[string]bool{}}
	srv := httptest.NewServer(svc)
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.data, "config.json"), []byte(`{"bind": "127.0.0.1", "port": `+port+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	service, timeout := updateService, updateHealthTimeout
	updateService, updateHealthTimeout = svc, 2*time.Second
	t.Cleanup(func() { updateService, updateHealthTimeout = service, timeout })
	if key != "" {
		path := filepath.Join(f.root, release.ConfigFile)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]json.RawMessage
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		cfg["scheduled_switch"] = json.RawMessage(key)
		data, _ = json.Marshal(cfg)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return f, svc
}

func (f *fetchFixture) current(t *testing.T) string {
	t.Helper()
	target, err := os.Readlink(filepath.Join(f.root, release.CurrentLink))
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func (f *fetchFixture) staged(t *testing.T, v string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(f.root, "releases", v, release.SumsFile))
	return err == nil
}

func TestScheduledFetchSwitches(t *testing.T) {
	f, svc := newScheduledFixture(t, "true", "0.2.0")
	code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 0 || errOut != "" {
		t.Fatalf("scheduled fetch: %d %q\n%s", code, errOut, out)
	}
	if svc.restarts != 1 || f.current(t) != "releases/0.2.0" {
		t.Fatalf("%d restarts, current %s\n%s", svc.restarts, f.current(t), out)
	}
	if !strings.Contains(out, "staged: releases/0.2.0\n") || !strings.Contains(out, "scheduled_switch: switching to 0.2.0\n") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestScheduledFetchWithoutTheKeyRestartsNothing(t *testing.T) {
	for name, key := range map[string]string{"false": "false", "absent": ""} {
		t.Run(name, func(t *testing.T) {
			f, svc := newScheduledFixture(t, key, "0.2.0")
			code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
			if code != 0 || errOut != "" || !f.staged(t, "0.2.0") {
				t.Fatalf("scheduled fetch: %d %q\n%s", code, errOut, out)
			}
			if svc.restarts != 0 || f.current(t) != "releases/0.1.0" {
				t.Fatalf("%d restarts, current %s", svc.restarts, f.current(t))
			}
			if !strings.HasSuffix(out, "scheduled_switch: off, nothing restarted\n") {
				t.Fatalf("output:\n%s", out)
			}
		})
	}
}

func TestFetchWithoutTheFlagIgnoresTheKey(t *testing.T) {
	for name, key := range map[string]string{"true": "true", "false": "false", "not a boolean": `"yes"`} {
		t.Run(name, func(t *testing.T) {
			f, svc := newScheduledFixture(t, key, "0.2.0")
			code, out, errOut := run(t, "update", "--fetch", "--root", f.root)
			if code != 0 || errOut != "" || !f.staged(t, "0.2.0") {
				t.Fatalf("fetch: %d %q\n%s", code, errOut, out)
			}
			if svc.restarts != 0 || f.current(t) != "releases/0.1.0" || strings.Contains(out, "scheduled_switch") {
				t.Fatalf("%d restarts, current %s\n%s", svc.restarts, f.current(t), out)
			}
		})
	}
}

func TestScheduledFetchWithNothingStaged(t *testing.T) {
	t.Run("no higher version", func(t *testing.T) {
		f, svc := newScheduledFixture(t, "true")
		code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
		if code != 0 || errOut != "" || svc.restarts != 0 || f.current(t) != "releases/0.1.0" {
			t.Fatalf("%d %q, %d restarts, current %s\n%s", code, errOut, svc.restarts, f.current(t), out)
		}
		if !strings.HasSuffix(out, "scheduled_switch: nothing staged in this run, nothing restarted\n") {
			t.Fatalf("output:\n%s", out)
		}
	})
	t.Run("staged before", func(t *testing.T) {
		f, svc := newScheduledFixture(t, "true", "0.2.0")
		if code, out, errOut := run(t, "update", "--fetch", "--root", f.root); code != 0 {
			t.Fatalf("fetch: %d %q\n%s", code, errOut, out)
		}
		code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
		if code != 0 || errOut != "" || svc.restarts != 0 || f.current(t) != "releases/0.1.0" {
			t.Fatalf("%d %q, %d restarts, current %s\n%s", code, errOut, svc.restarts, f.current(t), out)
		}
		if !strings.HasSuffix(out, "scheduled_switch: nothing staged in this run, nothing restarted\n") {
			t.Fatalf("output:\n%s", out)
		}
	})
}

func TestScheduledFetchRefusals(t *testing.T) {
	for name, key := range map[string]string{"string": `"yes"`, "number": "1", "null": "null"} {
		t.Run(name, func(t *testing.T) {
			f, svc := newScheduledFixture(t, key, "0.2.0")
			code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
			if code != 1 || !strings.Contains(errOut, "scheduled_switch") || f.staged(t, "0.2.0") || svc.restarts != 0 {
				t.Fatalf("%d %q, staged %v, %d restarts\n%s", code, errOut, f.staged(t, "0.2.0"), svc.restarts, out)
			}
		})
	}
	dataDir(t)
	for _, args := range [][]string{{"update", "--scheduled"}, {"update", "--scheduled", "dir"}, {"update", "--scheduled", "--switch", "0.2.0"}} {
		code, out, errOut := run(t, args...)
		if code != 2 || out != "" || !strings.Contains(errOut, "--scheduled needs --fetch") {
			t.Errorf("%v: %d %q %q", args, code, out, errOut)
		}
	}
}

// The switch after a scheduled fetch runs once the lock of releases/ is free,
// under the lock of the root.
func TestScheduledSwitchAfterTheFetchLock(t *testing.T) {
	f, svc := newScheduledFixture(t, "true", "0.2.0")
	var fetchLock, rootLock error
	svc.during = func() {
		if h, err := release.HoldReleases(f.root); err == nil {
			h.Release()
		} else {
			fetchLock = err
		}
		if h, err := release.HoldRoot(f.root); err == nil {
			h.Release()
			rootLock = errors.New("the lock of the root was free")
		}
	}
	code, out, errOut := run(t, "update", "--fetch", "--scheduled", "--root", f.root)
	if code != 0 || svc.restarts != 1 || fetchLock != nil || rootLock != nil {
		t.Fatalf("%d %q, %d restarts, lock of releases/ %v, %v\n%s", code, errOut, svc.restarts, fetchLock, rootLock, out)
	}
}
