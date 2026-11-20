package importer_test

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/importer"
	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// An export with --with-secrets (decisions K1 to K9 of
// docs/plans/secrets-import.md). No file of the repository holds a key or a
// password: every value is drawn at run time.

// inventedKey is a key in the shape of the old product, drawn at run time.
func inventedKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	h := hex.EncodeToString(b)
	return strings.Join([]string{h[:8], h[8:12], h[12:16], h[16:20], h[20:]}, "-")
}

func inventedSecret(t *testing.T, what string) string {
	t.Helper()
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "invented-" + what + "-" + hex.EncodeToString(b)
}

type secretFile struct {
	table   string
	columns []string
	rows    []map[string]any
}

// withSecrets copies an export and adds the secret files as the export
// program writes them with --with-secrets.
func withSecrets(t *testing.T, from string, left map[string]any, files ...secretFile) string {
	t.Helper()
	dir := copyExport(t, from)
	var entries []any
	for _, f := range files {
		name := strings.ToLower(f.table) + ".jsonl"
		var b strings.Builder
		for _, r := range f.rows {
			r["_removed"] = []any{}
			line, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			b.Write(line)
			b.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(b.String()))
		columns := make([]any, len(f.columns))
		for i, c := range f.columns {
			columns[i] = c
		}
		entries = append(entries, map[string]any{"table": f.table, "file": name, "columns": columns,
			"rows": len(f.rows), "removed": 0, "sha256": fmt.Sprintf("%x", sum)})
	}
	if left == nil {
		left = map[string]any{"keyed": 0, "unreadable": 0, "unknownDomains": 0, "unreferenced": 0}
	}
	editManifest(t, dir, func(m map[string]any) {
		m["files"] = append(m["files"].([]any), entries...)
		m["secrets"] = true
		m["secretsLeft"] = left
	})
	return dir
}

func keyRow(user, domain, realm, key string) map[string]any {
	sum := sha256.Sum256([]byte(key))
	return map[string]any{"username": user, "domain": domain, "realm": realm, "sha256": hex.EncodeToString(sum[:]),
		"created": "2026-03-04T05:06:07.123+01:00"}
}

func keys(rows ...map[string]any) secretFile {
	return secretFile{"API_KEY_V2", []string{"username", "domain", "realm", "sha256", "created"}, rows}
}

func remotes(rows ...map[string]any) secretFile {
	return secretFile{"REMOTE_SECRETS", []string{"repository", "field", "secret"}, rows}
}

func ldaps(rows ...map[string]any) secretFile {
	return secretFile{"LDAP_SECRETS", []string{"id", "secret"}, rows}
}

// digestOf is the digest the import stores for a key: HMAC-SHA-256 under
// the install key of the SHA-256 of the key (K4).
func digestOf(t *testing.T, dataDir, key string) [32]byte {
	t.Helper()
	installKey, err := state.ReadSecret(state.InstallKeyPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(key))
	m := hmac.New(sha256.New, installKey)
	m.Write(sum[:])
	var d [32]byte
	copy(d[:], m.Sum(nil))
	return d
}

func TestAnExportWithSecretsImportsKeysAndSecretFiles(t *testing.T) {
	npmKey, dockerKey, ldapKey, nugetKey, unknownKey := inventedKey(t), inventedKey(t), inventedKey(t), inventedKey(t),
		inventedKey(t)
	bind := inventedSecret(t, "bind")
	dir := withSecrets(t, fixture, nil,
		keys(keyRow("admin", "NpmToken", "NexusAuthenticatingRealm", npmKey),
			keyRow("admin", "DockerToken", "NexusAuthenticatingRealm", dockerKey),
			keyRow("ldap.person", "NpmToken", "LdapRealm", ldapKey),
			keyRow("viewer", "NuGetApiKey", "unknown", nugetKey),
			keyRow("someone.else", "NpmToken", "unknown", unknownKey)),
		remotes(), ldaps(map[string]any{"id": "6f1c2a52-0000-4000-8000-000000000001", "secret": bind}))
	cfg := dataDir(t)
	result, err := importer.Import(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.ImportedKeys != 5 || result.LDAPSecrets != 1 || result.RemoteSecrets != 0 {
		t.Fatalf("keys %d, LDAP secrets %d, remote secrets %d", result.ImportedKeys, result.LDAPSecrets, result.RemoteSecrets)
	}
	st, err := state.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, c := range []struct{ kind, key, user, source string }{
		{"npm", npmKey, "admin", "default"}, {"docker", dockerKey, "admin", "default"},
		{"npm", ldapKey, "ldap.person", "LDAP"},
		{"nuget", nugetKey, "viewer", "default"},    // unknown realm, a local user of the name
		{"npm", unknownKey, "someone.else", "LDAP"}, // unknown realm, no local user: the directory
	} {
		got, found, err := st.ImportedKey(c.kind, digestOf(t, cfg.DataDir, c.key))
		if err != nil || !found {
			t.Fatalf("%s key of %s: found %v, %v", c.kind, c.user, found, err)
		}
		if got.UserID != c.user || got.Source != c.source || got.Created.UnixMilli() != 1772597167123 {
			t.Fatalf("%s key of %s: %#v, created %d", c.kind, c.user, got, got.Created.UnixMilli())
		}
	}
	data, err := state.ReadSecret(state.LDAPSecretPath(cfg.DataDir, "6f1c2a52-0000-4000-8000-000000000001"))
	if err != nil || string(data) != bind {
		t.Fatalf("LDAP secret file: %v, equal %v", err, string(data) == bind)
	}
	info, err := os.Stat(state.InstallKeyPath(cfg.DataDir))
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != 32 {
		t.Fatalf("install key: %v %v", info, err)
	}
	// no value of a key and no secret reaches state.sqlite
	raw, err := os.ReadFile(state.Path(cfg.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{npmKey, dockerKey, ldapKey, bind} {
		sum := sha256.Sum256([]byte(v))
		if strings.Contains(string(raw), v) || strings.Contains(string(raw), string(sum[:])) {
			t.Fatal("state.sqlite holds a key, a secret or the plain SHA-256 of a key")
		}
	}
}

// A second import keeps the install key, so the keys of the first stay
// valid, and replaces the keys.
func TestASecondImportKeepsTheInstallKey(t *testing.T) {
	key := inventedKey(t)
	dir := withSecrets(t, fixture, nil, keys(keyRow("admin", "NpmToken", "NexusAuthenticatingRealm", key)), remotes(), ldaps())
	cfg := dataDir(t)
	if _, err := importer.Import(dir, cfg); err != nil {
		t.Fatal(err)
	}
	first := digestOf(t, cfg.DataDir, key)
	if _, err := importer.Import(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if digestOf(t, cfg.DataDir, key) != first {
		t.Fatal("the second import made a new install key")
	}
}

func TestRemoteSecretsBecomeTheFilesOfTheProxies(t *testing.T) {
	password, bearer := inventedSecret(t, "password"), inventedSecret(t, "bearer")
	dir := withSecrets(t, fixtureContent, nil, keys(),
		remotes(map[string]any{"repository": "demo-maven-proxy", "field": "password", "secret": password},
			map[string]any{"repository": "demo-npm-proxy", "field": "bearerToken", "secret": bearer}),
		ldaps())
	cfg := dataDir(t)
	result, err := importer.Import(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemoteSecrets != 2 || len(result.NeedsCredentials) != 0 {
		t.Fatalf("remote secrets %d, needs credentials %v", result.RemoteSecrets, result.NeedsCredentials)
	}
	for repo, want := range map[string]string{"demo-maven-proxy": password, "demo-npm-proxy": bearer} {
		got, err := proxy.ReadSecret(cfg.DataDir, repo)
		if err != nil || got != want {
			t.Fatalf("%s: %v, equal %v", repo, err, got == want)
		}
	}
	info, err := os.Stat(filepath.Join(cfg.DataDir, "secrets", "remote"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("folder secrets/remote: %v %v", info, err)
	}
}

// An export without secrets keeps the imported keys of an earlier import.
func TestAnImportWithoutSecretsKeepsTheImportedKeys(t *testing.T) {
	key := inventedKey(t)
	cfg := dataDir(t)
	if _, err := importer.Import(withSecrets(t, fixture, nil,
		keys(keyRow("admin", "NpmToken", "NexusAuthenticatingRealm", key)), remotes(), ldaps()), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.Import(fixture, cfg); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, found, err := st.ImportedKey("npm", digestOf(t, cfg.DataDir, key)); err != nil || !found {
		t.Fatalf("the key went with an import without secrets: %v %v", found, err)
	}
}

// Broken secret files are refused before anything is written, and no error
// holds a value.
func TestBrokenSecretFilesAreRefusedWithoutTheirValues(t *testing.T) {
	key, secret := inventedKey(t), inventedSecret(t, "value")
	good := keyRow("admin", "NpmToken", "NexusAuthenticatingRealm", key)
	with := func(edit func(r map[string]any)) map[string]any {
		r := keyRow("admin", "NpmToken", "NexusAuthenticatingRealm", key)
		edit(r)
		return r
	}
	cases := map[string]string{}
	build := func(name string, files ...secretFile) {
		cases[name] = withSecrets(t, fixture, nil, files...)
	}
	build("unknown domain", keys(with(func(r map[string]any) { r["domain"] = "OtherToken" })), remotes(), ldaps())
	build("unknown realm", keys(with(func(r map[string]any) { r["realm"] = "OtherRealm" })), remotes(), ldaps())
	build("short digest", keys(with(func(r map[string]any) { r["sha256"] = "ab" })), remotes(), ldaps())
	build("no time", keys(with(func(r map[string]any) { r["created"] = "yesterday" })), remotes(), ldaps())
	build("no user", keys(with(func(r map[string]any) { r["username"] = "" })), remotes(), ldaps())
	build("one key twice", keys(good, keyRow("viewer", "NpmToken", "unknown", key)), remotes(), ldaps())
	build("remote field", keys(), remotes(map[string]any{"repository": "demo-raw", "field": "other", "secret": secret}), ldaps())
	build("remote name", keys(), remotes(map[string]any{"repository": "../x", "field": "password", "secret": secret}), ldaps())
	build("remote twice", keys(), remotes(map[string]any{"repository": "demo-raw", "field": "password", "secret": secret},
		map[string]any{"repository": "demo-raw", "field": "bearerToken", "secret": secret}), ldaps())
	build("empty secret", keys(), remotes(), ldaps(map[string]any{"id": "x", "secret": ""}))
	build("control character", keys(), remotes(), ldaps(map[string]any{"id": "x", "secret": secret + "\n"}))
	cases["files missing"] = withSecrets(t, fixture, nil, keys(), remotes())
	for name, dir := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := dataDir(t)
			_, err := importer.Import(dir, cfg)
			if err == nil {
				t.Fatal("imported")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), key) {
				t.Fatalf("the error holds a value: %v", err)
			}
			if _, statErr := os.Stat(cfg.DataDir); !os.IsNotExist(statErr) {
				entries, _ := os.ReadDir(cfg.DataDir)
				t.Fatalf("the refused import wrote %v (%v)", entries, err)
			}
		})
	}
}

// The counts of what the export program left out reach the result.
func TestTheSecretsLeftOutAreCounted(t *testing.T) {
	dir := withSecrets(t, fixture, map[string]any{"keyed": 1, "unreadable": 2, "unknownDomains": 3, "unreferenced": 4},
		keys(), remotes(), ldaps())
	export, err := importer.ReadExport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := export.SecretsLeft; got != (importer.SecretsLeft{Keyed: 1, Unreadable: 2, UnknownDomains: 3, Unreferenced: 4}) {
		t.Fatalf("left %+v", got)
	}
	if !export.Secrets || !slices.Contains(export.Kinds, state.KindUsers) {
		t.Fatalf("secrets %v kinds %v", export.Secrets, export.Kinds)
	}
}

// The set secrets holds the three secret files and no table: its import
// writes the keys and the secret files and leaves every record kind and
// repositories.json as they are (the run book of the host).
func TestTheSetSecretsChangesNoRecordKind(t *testing.T) {
	cfg := dataDir(t)
	if _, err := importer.Import(fixture, cfg); err != nil {
		t.Fatal(err)
	}
	before := loadState(t, cfg)
	key, bind := inventedKey(t), inventedSecret(t, "bind")
	dir := withSecrets(t, fixture, nil, keys(keyRow("ldap.person", "NpmToken", "unknown", key)), remotes(),
		ldaps(map[string]any{"id": "6f1c2a52-0000-4000-8000-000000000001", "secret": bind}))
	for _, name := range []string{"security_user.jsonl", "role.jsonl", "user_role_mapping.jsonl", "privilege.jsonl",
		"realm_configuration.jsonl", "anonymous_configuration.jsonl", "ldap_configuration.jsonl"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	editManifest(t, dir, func(m map[string]any) {
		m["set"] = "secrets"
		m["files"] = m["files"].([]any)[7:]
	})
	result, err := importer.Import(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Kinds) != 0 || result.ImportedKeys != 1 || result.LDAPSecrets != 1 || result.RepositoriesPath != "" {
		t.Fatalf("kinds %v, keys %d, LDAP %d, repositories %q", result.Kinds, result.ImportedKeys, result.LDAPSecrets,
			result.RepositoriesPath)
	}
	same(t, loadState(t, cfg), before)
	st, err := state.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, found, err := st.ImportedKey("npm", digestOf(t, cfg.DataDir, key))
	if err != nil || !found || got.Source != "LDAP" {
		t.Fatalf("the key: %#v %v %v", got, found, err)
	}
	// the set needs the secret files
	plain := copyExport(t, dir)
	editManifest(t, plain, func(m map[string]any) {
		delete(m, "secrets")
		delete(m, "secretsLeft")
		m["files"] = []any{}
	})
	if _, err := importer.ReadExport(plain); err == nil {
		t.Fatal("a set secrets without secrets was read")
	}
}

// A secret file that cannot be written ends the import before state.sqlite
// takes it, so state.sqlite and repositories.json never part (review
// secrets-1).
func TestASecretFileThatFailsWritesNoState(t *testing.T) {
	dir := withSecrets(t, fixtureContent, nil, keys(),
		remotes(map[string]any{"repository": "demo-maven-proxy", "field": "password", "secret": inventedSecret(t, "p")}),
		ldaps())
	cfg := dataDir(t)
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "secrets", "remote"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.Import(dir, cfg); err == nil {
		t.Fatal("imported although the secret file cannot be written")
	}
	if _, err := os.Stat(state.Path(cfg.DataDir)); !os.IsNotExist(err) {
		t.Fatalf("state.sqlite was written: %v", err)
	}
	if _, err := os.Stat(cfg.RepositoriesPath()); !os.IsNotExist(err) {
		t.Fatalf("repositories.json was written: %v", err)
	}
}

// A secret file that exists is kept: a value set by hand (repos
// remote-secret, ldap password) may be newer than the one the old product
// stored, so the import names the file and writes no value over it.
func TestSecretFilesThatExistAreKept(t *testing.T) {
	stored, byHand := inventedSecret(t, "stored"), inventedSecret(t, "by-hand")
	dir := withSecrets(t, fixtureContent, nil, keys(),
		remotes(map[string]any{"repository": "demo-maven-proxy", "field": "password", "secret": stored},
			map[string]any{"repository": "demo-npm-proxy", "field": "bearerToken", "secret": stored}),
		ldaps(map[string]any{"id": "6f1c2a52-0000-4000-8000-000000000001", "secret": stored}))
	cfg := dataDir(t)
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := proxy.WriteSecret(cfg.DataDir, "demo-maven-proxy", byHand); err != nil {
		t.Fatal(err)
	}
	ldapPath := state.LDAPSecretPath(cfg.DataDir, "6f1c2a52-0000-4000-8000-000000000001")
	if err := state.WriteSecret(ldapPath, []byte(byHand)); err != nil {
		t.Fatal(err)
	}
	result, err := importer.Import(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemoteSecrets != 1 || result.LDAPSecrets != 0 ||
		!slices.Equal(result.SecretsKept, []string{"secrets/remote/demo-maven-proxy", "secrets/ldap-6f1c2a52-0000-4000-8000-000000000001"}) {
		t.Fatalf("remote %d, LDAP %d, kept %v", result.RemoteSecrets, result.LDAPSecrets, result.SecretsKept)
	}
	if got, _ := proxy.ReadSecret(cfg.DataDir, "demo-maven-proxy"); got != byHand {
		t.Fatal("the remote secret set by hand was replaced")
	}
	if got, _ := state.ReadSecret(ldapPath); string(got) != byHand {
		t.Fatal("the bind password set by hand was replaced")
	}
	if got, _ := proxy.ReadSecret(cfg.DataDir, "demo-npm-proxy"); got != stored {
		t.Fatal("the remote secret without a file was not written")
	}
}
