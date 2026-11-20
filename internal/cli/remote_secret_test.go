package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The command repos remote-secret <name> (decisions 5.12 and 5.13 of the
// phase 5 plan).

const canarySecret = "CANARY-COMMAND-SECRET-5b10"

// proxyData is a data directory with a proxy, a hosted repository and a
// group in repositories.json.
func proxyData(t *testing.T) string {
	t.Helper()
	dir := dataDir(t)
	settings := writeSettings(t, filepath.Join(filepath.Dir(dir), "settings.json"),
		map[string]any{"name": "demo-raw-proxy", "format": "raw", "type": "proxy", "proxy": map[string]any{"remoteUrl": "https://repo.example.com/raw/"}},
		map[string]any{"name": "demo-raw", "format": "raw", "type": "hosted"},
		map[string]any{"name": "demo-raw-group", "format": "raw", "type": "group", "group": map[string]any{"memberNames": []string{"demo-raw"}}},
	)
	if code, _, errOut := run(t, "import-settings", settings); code != 0 {
		t.Fatalf("import-settings: %d %s", code, errOut)
	}
	return dir
}

// The command reads the secret from standard input, writes it with mode
// 0600 into secrets/remote/<name> below folders of mode 0700, and prints
// nothing of it; a second run replaces it.
func TestReposRemoteSecret(t *testing.T) {
	dir := proxyData(t)
	path := filepath.Join(dir, "secrets", "remote", "demo-raw-proxy")
	for _, input := range []string{canarySecret + "\n", canarySecret + "-2\r\n", canarySecret + "-3"} {
		withStdin(t, input)
		code, out, errOut := run(t, "repos", "remote-secret", "demo-raw-proxy")
		if code != 0 || !strings.Contains(out, path) || strings.Contains(out+errOut, canarySecret) {
			t.Fatalf("%q: %d %q %q", input, code, out, errOut)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != strings.TrimRight(input, "\r\n") {
			t.Fatalf("%q: the file holds %q %v", input, data, err)
		}
	}
	for name, mode := range map[string]os.FileMode{filepath.Join(dir, "secrets"): 0o700, filepath.Join(dir, "secrets", "remote"): 0o700, path: 0o600} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v, want %o", name, info, err, mode)
		}
	}
}

// A name that is no proxy repository, an empty secret and a secret given as
// an argument are refused, and no file is written.
func TestReposRemoteSecretRefusals(t *testing.T) {
	dir := proxyData(t)
	for _, name := range []string{"demo-raw", "demo-raw-group", "demo-missing", "../demo-raw-proxy", "demo-raw-proxy/../x"} {
		withStdin(t, canarySecret+"\n")
		code, out, errOut := run(t, "repos", "remote-secret", name)
		if code != 1 || !strings.Contains(errOut, "no proxy repository") || strings.Contains(out+errOut, canarySecret) {
			t.Errorf("%q: %d %q %q", name, code, out, errOut)
		}
	}
	for _, input := range []string{"", "\n", "\r\n"} {
		withStdin(t, input)
		if code, out, errOut := run(t, "repos", "remote-secret", "demo-raw-proxy"); code != 1 || !strings.Contains(errOut, "no secret") {
			t.Errorf("%q: %d %q %q", input, code, out, errOut)
		}
	}
	withStdin(t, "")
	if code, _, _ := run(t, "repos", "remote-secret", "demo-raw-proxy", canarySecret); code != 2 {
		t.Errorf("a secret as an argument: %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", "remote", "demo-raw-proxy")); err == nil {
		t.Fatal("a refused run wrote the secret")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "secrets", "remote"))
	if len(entries) != 0 {
		t.Fatalf("files below secrets/remote: %v", entries)
	}
}

func TestReposRemoteSecretHelp(t *testing.T) {
	dataDir(t)
	code, out, _ := run(t, "repos", "remote-secret", "--help")
	want := "usage: pipesmith-registry repos remote-secret [-h] name\n\npositional arguments:\n  name\n\noptions:\n" +
		"  -h, --help  show this help message and exit\n"
	if code != 0 || out != want {
		t.Fatalf("%d %q", code, out)
	}
	if code, out, _ := run(t, "help"); code != 0 || !strings.Contains(out, "  repos remote-secret <name>") {
		t.Fatalf("%d %q", code, out)
	}
}
