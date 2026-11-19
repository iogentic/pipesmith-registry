package manifest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/manifest"
)

// fakeZstd writes a zstd script into dir that writes its environment to
// report and prints nothing.
func fakeZstd(t *testing.T, dir, report string) {
	t.Helper()
	script := "#!/bin/sh\n/usr/bin/env > '" + report + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "zstd"), []byte(script), 0o755); err != nil { // #nosec G306 -- the fake program must be executable.
		t.Fatal(err)
	}
}

func zstdPart(t *testing.T, base string) string {
	t.Helper()
	part := filepath.Join(base, "m", "part-1.tsv.zst")
	if err := os.MkdirAll(filepath.Dir(part), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, []byte("not read"), 0o666); err != nil {
		t.Fatal(err)
	}
	return part
}

func TestZstdThroughARelativePathEntryIsNotRun(t *testing.T) {
	base := t.TempDir()
	part := zstdPart(t, base)
	work := filepath.Join(base, "work")
	if err := os.Mkdir(work, 0o777); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(base, "ran.txt")
	fakeZstd(t, work, report)
	t.Chdir(work)
	for _, path := range []string{".", "/nonexistent:.", "/nonexistent::/usr/local/nothing"} {
		t.Setenv("PATH", path)
		err := manifest.ReadRows(part, func(manifest.Row) error { return nil })
		if _, statErr := os.Stat(report); statErr == nil {
			t.Fatalf("PATH %q: the zstd of the working directory ran", path)
		}
		if err == nil || !strings.Contains(err.Error(), "zstd") || !strings.Contains(err.Error(), "PATH") {
			t.Errorf("PATH %q: error %v, want one that names zstd and the PATH entry rule", path, err)
		}
	}
}

func TestZstdGetsOnlyAFewVariables(t *testing.T) {
	base := t.TempDir()
	part := zstdPart(t, base)
	bin := filepath.Join(base, "bin")
	if err := os.Mkdir(bin, 0o777); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(base, "env.txt")
	fakeZstd(t, bin, report)
	t.Setenv("PATH", bin)
	t.Setenv("PIPESMITH_REGISTRY_LDAP_PASSWORD_DEMO", "demo-bind-secret")
	t.Setenv("LANG", "C.UTF-8")
	if err := manifest.ReadRows(part, func(manifest.Row) error { return nil }); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("the zstd of an absolute PATH entry did not run: %v", err)
	}
	env := string(data)
	if strings.Contains(env, "PIPESMITH_REGISTRY_LDAP_PASSWORD_DEMO") || strings.Contains(env, "demo-bind-secret") {
		t.Errorf("zstd sees the bind password: %s", env)
	}
	if !strings.Contains(env, "PATH="+bin+"\n") || !strings.Contains(env, "LANG=C.UTF-8\n") {
		t.Errorf("zstd misses PATH or LANG: %s", env)
	}
}
