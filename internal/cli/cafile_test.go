package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Review 3-14: the CA file of an LDAP connection is a trust anchor. ldap ca
// and every build of serve refuse a symbolic link and a file that the group
// or others may write, with a message that says so; a file others may only
// read is taken.
func TestLDAPCAFileRefusesALinkAndAWritableFile(t *testing.T) {
	dir := importedFixture(t)
	folder := t.TempDir()
	certFile, _, _, err := testkit.TLSPair(folder)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(certFile, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(folder, "link.pem")
	if err := os.Symlink(certFile, link); err != nil {
		t.Fatal(err)
	}
	writable := filepath.Join(folder, "writable.pem")
	pem, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(writable, pem, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o666); err != nil {
		t.Fatal(err)
	}
	setCA := func(path string) []model.LDAPServer {
		t.Helper()
		st, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		var servers []model.LDAPServer
		err = st.Update([]state.Kind{state.KindLDAP}, func(snap *model.Snapshot) error {
			snap.LDAP[0].CAFile = path
			servers = snap.LDAP
			return nil
		})
		if cerr := st.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
		return servers
	}
	if code, _, _ := withInput(t, testBindPassword+"\n", "ldap", "password", "demo-directory"); code != 0 {
		t.Fatal(code)
	}
	for _, c := range []struct{ file, want string }{
		{link, "symbolic link"},
		{writable, "others may write"},
	} {
		if code, out, errOut := withInput(t, "", "ldap", "ca", "demo-directory", c.file); code != 1 || out != "" ||
			!strings.Contains(errOut, c.want) || !strings.Contains(errOut, c.file) {
			t.Errorf("ldap ca %s: %d %q %q", c.file, code, out, errOut)
		}
		// a row that names the file already: the build leaves the
		// connection out
		var log bytes.Buffer
		if dirs, err := newLDAPBuilds(dir, logTo(&log)).build(setCA(c.file), nil); err != nil || len(dirs) != 0 ||
			!strings.Contains(log.String(), "LDAP connection demo-directory left out") || !strings.Contains(log.String(), c.want) {
			t.Errorf("serve with the CA file %s: %d directories, %v, log %q", c.file, len(dirs), err, log.String())
		}
	}
	if code, out, errOut := withInput(t, "", "ldap", "ca", "demo-directory", certFile); code != 0 ||
		!strings.Contains(out, "CA file of LDAP connection demo-directory: "+certFile) {
		t.Fatalf("ldap ca with a file of mode 0644: %d %q %q", code, out, errOut)
	}
	if _, err := readCA(certFile); err != nil {
		t.Fatalf("a CA file of mode 0644: %v", err)
	}
}
