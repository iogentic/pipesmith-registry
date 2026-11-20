//go:build linux || darwin

package release

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// signalAt sends sig to this process at the hook point, as Ctrl-C, a dropped
// SSH session or a stop of the service manager does, and lets the run go on.
func signalAt(t *testing.T, point string, sig syscall.Signal) func(string) error {
	return func(p string) error {
		if p == point {
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Error(err)
			}
			time.Sleep(50 * time.Millisecond) // the signal arrives before the run goes on
		}
		return nil
	}
}

// SIGINT, SIGHUP and SIGTERM between the record of a switch and its removal
// do not end update: the switch ends, the record goes, and the output says
// which signal came (finding R3-1). Without the hold the test process dies.
func TestSignalsDuringTheSwitch(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			r := newRig(t, "0.1.0")
			r.u.hook = signalAt(t, "kill-after-switch", sig)
			if err := r.u.Update(r.release(versionScript("0.2.0")), false); err != nil {
				t.Fatalf("update: %v\n%s", err, r.out.String())
			}
			if r.link(CurrentLink) != "releases/0.2.0" || r.svc.running != "0.2.0" {
				t.Fatalf("current %q running %q", r.link(CurrentLink), r.svc.running)
			}
			r.noPending()
			if !strings.Contains(r.out.String(), "received "+sig.String()+" during the switch") {
				t.Fatalf("output:\n%s", r.out.String())
			}
		})
	}
}

// The way back of a failed rollback runs to its end under a signal too.
func TestSignalDuringTheWayBack(t *testing.T) {
	r := rollbackRig(t)
	r.svc.unhealthy["0.1.0"] = true
	r.u.hook = signalAt(t, "swap-current", syscall.SIGTERM)
	err := r.u.Rollback()
	if err == nil || !strings.Contains(err.Error(), "went back to releases/0.2.0") {
		t.Fatalf("rollback: %v\n%s", err, r.out.String())
	}
	if r.link(CurrentLink) != "releases/0.2.0" || r.svc.running != "0.2.0" {
		t.Fatalf("current %q running %q", r.link(CurrentLink), r.svc.running)
	}
	r.noPending()
}

// systemctl and the release binary run in a process group of their own, so a
// Ctrl-C at the terminal reaches only update, which holds it, and not the
// restart that systemctl waits for.
func TestChildrenRunInTheirOwnGroup(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc on this system")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "group")
	script := "#!/bin/sh\ncut -d' ' -f5 /proc/$$/stat >> '" + out + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := (Systemctl{}).Stop(context.Background(), "demo.service"); err != nil {
		t.Fatal(err)
	}
	if _, err := (Systemctl{}).Active(context.Background(), "demo.service"); err != nil {
		t.Fatal(err)
	}
	if _, err := (Systemctl{}).UnitFacts(context.Background(), "demo.service"); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "release")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncut -d' ' -f5 /proc/$$/stat >> '"+out+"'\necho 'pipesmith-registry 1.0.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RunVersion(bin, 0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	groups := strings.Fields(string(data))
	if len(groups) != 4 {
		t.Fatalf("groups %q", groups)
	}
	for _, g := range groups {
		if g == strconv.Itoa(syscall.Getpgrp()) {
			t.Fatalf("a child ran in the group of the test process %s: %q", g, groups)
		}
	}
}
