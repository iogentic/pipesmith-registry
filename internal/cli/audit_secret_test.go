package cli

import (
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
)

// Task 7.17, the secret files: the commands that write below secrets/ (the
// install key of the imported keys, the secret of a proxy, the bind
// password of a directory, the secret of a log target) and a password
// change each write an audit line, and no line of audit.log holds what a
// secret file or standard input held.
func TestAuditHoldsNoSecret(t *testing.T) {
	dir := proxyData(t)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o750); err != nil { // as serve made it; the line of import-settings of proxyData went nowhere
		t.Fatal(err)
	}
	t.Setenv(ldapauth.PasswordVariable(fixtureLDAPID), "")
	canaries := map[string]string{
		"remote":     "demo-canary-remote-7d1f",
		"ldap":       "demo-canary-bind-52aa",
		"password":   "demo-canary-password-9e0c",
		"log-target": "demo-canary-target-3b6e",
		"npm key":    drawnKey(t),
	}
	steps := []struct {
		input string
		args  []string
	}{
		{"", []string{"import", exportWithKeys(t, map[string]string{"admin": canaries["npm key"]})}},
		{canaries["remote"] + "\n", []string{"repos", "remote-secret", "demo-raw-proxy"}},
		{canaries["ldap"] + "\n", []string{"ldap", "password", fixtureLDAPID}},
		{canaries["password"] + "\n", []string{"user", "password", "admin"}},
		{canaries["log-target"] + "\n", []string{"log-target", "add", "demo-target", "--kind", "http-json", "--address",
			"http://127.0.0.1:14751/ingest", "--secret-stdin"}},
	}
	for _, step := range steps {
		withStdin(t, step.input)
		if code, out, errOut := run(t, step.args...); code != 0 {
			t.Fatalf("%v: %d %q %q", step.args, code, out, errOut)
		}
	}
	lines, text := auditLines(t, dir)
	if len(lines) != len(steps) {
		t.Fatalf("%d audit lines, want %d:\n%s", len(lines), len(steps), text)
	}
	for name, canary := range canaries {
		if strings.Contains(text, canary) {
			t.Errorf("audit.log holds the %s: %s", name, text)
		}
	}
	files := 0
	err := filepath.WalkDir(filepath.Join(dir, "secrets"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path) // #nosec G304 -- a file the test wrote below its own data directory.
		if err != nil {
			return err
		}
		files++
		for _, form := range []string{string(data), hex.EncodeToString(data), base64.StdEncoding.EncodeToString(data),
			base64.RawURLEncoding.EncodeToString(data)} {
			if len(form) >= 8 && strings.Contains(text, form) {
				t.Errorf("audit.log holds the content of %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"imported-keys", "remote/demo-raw-proxy", "log-target-demo-target"} {
		if _, err := os.Stat(filepath.Join(dir, "secrets", want)); err != nil {
			t.Errorf("secrets/%s: %v", want, err)
		}
	}
	if files < 4 {
		t.Errorf("%d secret files checked, want the install key, the proxy, the directory and the log target", files)
	}
}
