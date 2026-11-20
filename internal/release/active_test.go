//go:build linux || darwin

package release

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A healthy answer with the version of the new release does not count while
// the service manager says the unit is not active: another process may hold
// the port after the new release failed its start (finding A-6).
func TestHealthWaitAsksTheServiceManager(t *testing.T) {
	r := newRig(t, "0.1.0")
	r.svc.inactive = map[string]bool{"0.2.0": true}
	err := r.u.Update(r.release(versionScript("0.2.0")), false)
	if !errors.Is(err, ErrHealth) || !strings.Contains(err.Error(), "is not active") {
		t.Fatalf("error %v\n%s", err, r.out.String())
	}
	if r.link(CurrentLink) != "releases/0.1.0" {
		t.Fatalf("current %q", r.link(CurrentLink))
	}
	if !strings.Contains(err.Error(), "went back to releases/0.1.0, which answers again") {
		t.Fatalf("error %v", err)
	}
}

// Systemctl asks systemctl is-active for the unit: exit 0 is active, any
// other exit code is not.
func TestSystemctlActive(t *testing.T) {
	active, ok := any(Systemctl{}).(interface {
		Active(context.Context, string) (bool, error)
	})
	if !ok {
		t.Fatal("Systemctl has no Active")
	}
	calls := fakeSystemctlFailing(t, "is-active")
	got, err := active.Active(context.Background(), "demo.service")
	if err != nil || got {
		t.Fatalf("is-active fails: %v %v", got, err)
	}
	if c := readCalls(t, calls); c != "is-active --quiet -- demo.service\n" {
		t.Fatalf("calls:\n%s", c)
	}
	fakeSystemctl(t)
	if got, err := active.Active(context.Background(), "demo.service"); err != nil || !got {
		t.Fatalf("is-active passes: %v %v", got, err)
	}
	if _, err := active.Active(context.Background(), "bad unit"); err == nil {
		t.Fatal("bad unit passed")
	}
}
