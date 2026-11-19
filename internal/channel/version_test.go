package channel

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

func (r *rig) fetchWith(opt Options) (*Result, error) {
	r.t.Helper()
	return r.f.Fetch(opt)
}

func TestVersionOptionRefusesSameAndLower(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.1.0"})
	r.publish(published{version: "0.0.9"})
	r.start()
	for _, v := range []string{"0.1.0", "v0.1.0", "0.0.9", "v0.0.1"} {
		_, err := r.fetchWith(Options{Version: v})
		if !errors.Is(err, release.ErrVersion) || !strings.Contains(err.Error(), "--allow-downgrade") {
			t.Errorf("%s: %v", v, err)
		}
	}
	if n := len(r.stub.Requests()); n != 0 {
		t.Fatalf("%d requests for refused versions", n)
	}
	if got := strings.Join(r.releases(), ","); got != "0.1.0" {
		t.Fatalf("releases %s", got)
	}
}

func TestAllowDowngrade(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.1.0"})
	r.publish(published{version: "0.0.9"})
	r.start()
	// The running release holds the checked files of 0.1.0, as one the
	// fetch stage or the update made.
	entries, err := os.ReadDir(filepath.Join(r.stubDir, "0.1.0"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(r.stubDir, "0.1.0", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.root, release.ReleasesDir, "0.1.0", e.Name()), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	res, err := r.fetchWith(Options{Version: "0.0.9", AllowDowngrade: true})
	if err != nil || res.Version != "0.0.9" || res.Existed {
		t.Fatalf("%+v %v\n%s", res, err, r.out.String())
	}
	res, err = r.fetchWith(Options{Version: "v0.1.0", AllowDowngrade: true})
	if err != nil || !res.Existed || res.Version != "0.1.0" {
		t.Fatalf("the running version: %+v %v", res, err)
	}
	if got := strings.Join(r.releases(), ","); got != "0.0.9,0.1.0" {
		t.Fatalf("releases %s", got)
	}
}

func TestVersionOption(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.publish(published{version: "0.3.0"})
	r.publish(published{version: "0.5.0", noPub: true})
	r.start()
	res, err := r.fetchWith(Options{Version: "v0.2.0"})
	if err != nil || res.Version != "0.2.0" {
		t.Fatalf("%+v %v", res, err)
	}
	for _, path := range r.requested() {
		if strings.HasPrefix(path, "v0.3.0/") || strings.HasPrefix(path, "v0.5.0/") {
			t.Fatalf("requested %s", path)
		}
	}
	if _, err := r.fetchWith(Options{Version: "0.4.0"}); !errors.Is(err, ErrList) || !strings.Contains(err.Error(), "v0.4.0 is not listed") {
		t.Fatalf("unlisted: %v", err)
	}
	if _, err := r.fetchWith(Options{Version: "0.5.0"}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("incomplete: %v", err)
	}
	for _, v := range []string{"0.2", "latest", "vv0.2.0", "0.02.0", ""} {
		if v == "" {
			continue
		}
		if _, err := r.fetchWith(Options{Version: v}); !errors.Is(err, release.ErrVersion) {
			t.Errorf("%q: %v", v, err)
		}
	}
	if got := strings.Join(r.releases(), ","); got != "0.1.0,0.2.0" {
		t.Fatalf("releases %s", got)
	}
}

// TestOldReleaseUnderANewName serves a signed release whose binary is 0.1.5
// under the version name v0.3.0: the version answer refuses it before the
// rename, and current and previous stay as they were.
func TestOldReleaseUnderANewName(t *testing.T) {
	r := newRig(t)
	if err := os.MkdirAll(filepath.Join(r.root, "releases", "0.0.9"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/0.0.9", filepath.Join(r.root, release.PreviousLink)); err != nil {
		t.Fatal(err)
	}
	r.publish(published{version: "0.3.0", answer: "0.1.5"})
	r.publish(published{version: "0.2.0"})
	r.start()
	err := r.refused(ErrAnswer)
	if !strings.Contains(err.Error(), "0.1.5") || !strings.Contains(err.Error(), "v0.3.0") {
		t.Fatalf("message %v", err)
	}
	for link, want := range map[string]string{release.CurrentLink: "releases/0.1.0", release.PreviousLink: "releases/0.0.9"} {
		if got, _ := os.Readlink(filepath.Join(r.root, link)); got != want {
			t.Fatalf("%s -> %s, want %s", link, got, want)
		}
	}
	for _, path := range r.requested() {
		if strings.HasPrefix(path, "v0.2.0/") {
			t.Fatalf("a lower version was requested: %s", path)
		}
	}
}

func TestVersionAnswerMalformed(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0", answer: "0.2"})
	r.start()
	err := r.refused(ErrAnswer)
	if !errors.Is(err, release.ErrVersion) {
		t.Fatalf("%v", err)
	}
}
