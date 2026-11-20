package cli

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The commands around an export with secrets (docs/plans/secrets-import.md):
// import reports the counts, serve lets an imported key in, state show and
// token list count the keys, token forget-imported deletes them. Every key
// is drawn at run time.

func drawnKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	h := hex.EncodeToString(b)
	return strings.Join([]string{h[:8], h[8:12], h[12:16], h[16:20], h[20:]}, "-")
}

// exportWithKeys copies the fixture export and adds the three secret files
// of --with-secrets, with one npm key per user.
func exportWithKeys(t *testing.T, keys map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "export")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fixtureExport)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(fixtureExport, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var lines bytes.Buffer
	for user, key := range keys {
		sum := sha256.Sum256([]byte(key))
		row, _ := json.Marshal(map[string]any{"username": user, "domain": "NpmToken", "realm": "NexusAuthenticatingRealm",
			"sha256": hex.EncodeToString(sum[:]), "created": "2025-06-07T08:09:10Z", "_removed": []any{}})
		lines.Write(append(row, '\n'))
	}
	files := []struct {
		table   string
		columns []string
		data    []byte
		rows    int
	}{
		{"API_KEY_V2", []string{"username", "domain", "realm", "sha256", "created"}, lines.Bytes(), len(keys)},
		{"REMOTE_SECRETS", []string{"repository", "field", "secret"}, nil, 0},
		{"LDAP_SECRETS", []string{"id", "secret"}, nil, 0},
	}
	path := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		name := strings.ToLower(f.table) + ".jsonl"
		if err := os.WriteFile(filepath.Join(dir, name), f.data, 0o600); err != nil {
			t.Fatal(err)
		}
		m["files"] = append(m["files"].([]any), map[string]any{"table": f.table, "file": name, "columns": f.columns,
			"rows": f.rows, "removed": 0, "sha256": fmt.Sprintf("%x", sha256.Sum256(f.data))})
	}
	m["secrets"] = true
	m["secretsLeft"] = map[string]any{"keyed": 0, "unreadable": 1, "unknownDomains": 0, "unreferenced": 0}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func bearerSubject(t *testing.T, dir, header string) (*model.Subject, error) {
	t.Helper()
	var log bytes.Buffer
	sec, err := buildSecurity(dir, nil, nil, logTo(&log))
	if err != nil || sec == nil {
		t.Fatalf("security: %v %v", sec, err)
	}
	defer closeSecurity(sec)
	r, _ := http.NewRequest(http.MethodGet, "http://localhost/repository/demo-npm/", nil)
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Authorization", "Bearer "+header)
	return sec.Subject(r)
}

func TestImportedKeysThroughTheCommands(t *testing.T) {
	dir := dataDir(t)
	key := drawnKey(t)
	t.Setenv(ldapauth.PasswordVariable(fixtureLDAPID), "")
	code, out, errOut := run(t, "import", exportWithKeys(t, map[string]string{"admin": key}))
	if code != 0 || !strings.Contains(out, "secrets: imported keys 1, remote secrets 0, LDAP bind passwords 0") ||
		!strings.Contains(out, "note: 1 secrets of the export were not taken over: 0 under a key of their own, 1 unreadable") ||
		strings.Contains(out, "LDAP bind passwords are never imported") || strings.Contains(out+errOut, key) {
		t.Fatalf("import: %d %q %q", code, out, errOut)
	}
	if code, _, errOut := withInput(t, "", "realms", "set", "NexusAuthenticatingRealm,NpmToken"); code != 0 {
		t.Fatalf("realms set: %s", errOut)
	}
	s, err := bearerSubject(t, dir, "NpmToken."+key)
	if err != nil || s == nil || s.User.ID != "admin" {
		t.Fatalf("the imported key through serve: %+v %v", s, err)
	}
	code, out, _ = run(t, "state", "show")
	if code != 0 || !strings.Contains(out, `"importedKeys": {`+"\n"+`    "npm": 1`) {
		t.Fatalf("state show: %d %s", code, out)
	}
	code, out, _ = run(t, "token", "list", "admin")
	if code != 0 || !strings.Contains(out, "imported") || !strings.Contains(out, "2025-06-07 08:09") {
		t.Fatalf("token list: %d %q", code, out)
	}
	code, out, _ = run(t, "token", "forget-imported")
	if code != 0 || out != "deleted 1 imported keys\n" {
		t.Fatalf("token forget-imported: %d %q", code, out)
	}
	if s, err := bearerSubject(t, dir, "NpmToken."+key); s != nil || !errors.Is(err, model.ErrOldToken) {
		t.Fatalf("after forget-imported: %+v %v", s, err)
	}
}
