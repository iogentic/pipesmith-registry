package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// nobodySignsIn is the refusal of a network bind while state.sqlite is
// missing or holds nobody who can sign in.
const nobodySignsIn = "error: bind address '0.0.0.0' is not local; only 127.0.0.1, localhost, ::1 are allowed until users are " +
	"imported (import) or added (user add), or an LDAP connection is imported, with its realm active (realms set), then with " +
	"--server-names NAME[,NAME]\n"

// noServerNames is the refusal of a network bind that passed the check of
// state.sqlite and names no host.
const noServerNames = "error: bind address '0.0.0.0' is not local; name this server with --server-names NAME[,NAME]\n"

// activeUser is an active local user with a stored password.
func activeUser(t *testing.T) model.User {
	t.Helper()
	hash, err := auth.HashPassword("demo-bind-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	return model.User{ID: "demo-admin", Status: "active", Source: auth.SourceLocal, Roles: []string{}, PasswordHash: hash}
}

// writeState writes the records of kinds of snap into state.sqlite of dir.
func writeState(t *testing.T, dir string, kinds []state.Kind, snap *model.Snapshot) {
	t.Helper()
	st, err := state.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Write(kinds, snap); err != nil {
		t.Fatal(err)
	}
}

func TestServeNetworkBindNeedsUsersAndHostNames(t *testing.T) {
	dir := dataDir(t)
	// without state.sqlite a network bind keeps the refusal of the reference;
	// port 0 makes a missing check fail fast instead of serving
	code, _, errOut := run(t, "serve", "--port", "0", "--bind", "0.0.0.0", "--server-names", "registry.test")
	if code != 1 || errOut != nobodySignsIn {
		t.Fatalf("%d %q", code, errOut)
	}
	writeState(t, dir, []state.Kind{state.KindUsers, state.KindRealms},
		&model.Snapshot{Users: []model.User{activeUser(t)}, Realms: []string{auth.RealmLocal, auth.RealmAuthorizing}})
	for _, c := range []struct {
		args []string
		text string
	}{
		{[]string{"serve", "--port", "0", "--bind", "0.0.0.0"}, noServerNames},
		{[]string{"serve", "--port", "0", "--server-names", "bad name"}, "error: host name 'bad name' is not a DNS name or an IP address\n"},
		{[]string{"serve", "--port", "0", "--server-names", "registry.test:8081"}, "error: host name 'registry.test:8081' is not a DNS name or an IP address\n"},
		{[]string{"serve", "--port", "0", "--server-names", "a.test,,b.test"}, "error: host name '' is not a DNS name or an IP address\n"},
		{[]string{"serve", "--port", "0", "--server-names", ""}, "error: host name '' is not a DNS name or an IP address\n"},
	} {
		code, out, errOut := run(t, c.args...)
		if code != 1 || out != "" || errOut != c.text {
			t.Errorf("%v: %d %q %q", c.args, code, out, errOut)
		}
	}
}

func TestHostNames(t *testing.T) {
	got, err := parseHostNames("Registry.Test, 10.0.0.5,::1,[fd00::5]")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"registry.test", "10.0.0.5", "[::1]", "[fd00::5]"}
	if len(got) != len(want) {
		t.Fatalf("%q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%q, want %q", got, want)
		}
	}
}

// A network bind needs someone who can sign in, not only state.sqlite.
// import-state --settings writes the file with the repositories alone, and
// serve then bound a network address with sign in on and nobody who could
// sign in. Someone can: an active local user with a stored password while
// the realm of the local users is active, or an LDAP connection while the
// LDAP realm is active. A state that passes meets the next refusal, the
// missing --server-names, so no case binds 0.0.0.0.
func TestServeNetworkBindNeedsSomeoneWhoSignsIn(t *testing.T) {
	user := activeUser(t)
	disabled, hashless := user, user
	disabled.Status = "disabled"
	hashless.PasswordHash = ""
	directory := model.LDAPServer{ID: "demo-directory-1", Name: "demo-directory", Order: 1, Protocol: "ldap",
		Host: "ldap.example.com", Port: 389, SearchBase: "dc=example,dc=com", AuthScheme: "NONE"}
	identity := []state.Kind{state.KindUsers, state.KindLDAP, state.KindRealms}
	for name, c := range map[string]struct {
		snap *model.Snapshot // the identity records written after the settings, none for nil
		want string
	}{
		"settings only":                       {nil, nobodySignsIn},
		"an active local user":                {&model.Snapshot{Users: []model.User{user}, Realms: []string{auth.RealmLocal}}, noServerNames},
		"an active local user, its realm off": {&model.Snapshot{Users: []model.User{user}, Realms: []string{auth.RealmAuthorizing}}, nobodySignsIn},
		"a disabled local user":               {&model.Snapshot{Users: []model.User{disabled}, Realms: []string{auth.RealmLocal}}, nobodySignsIn},
		"a local user without a password":     {&model.Snapshot{Users: []model.User{hashless}, Realms: []string{auth.RealmLocal}}, nobodySignsIn},
		"an LDAP connection with its realm":   {&model.Snapshot{LDAP: []model.LDAPServer{directory}, Realms: []string{auth.RealmLDAP}}, noServerNames},
		"an LDAP connection, its realm off":   {&model.Snapshot{LDAP: []model.LDAPServer{directory}, Realms: []string{auth.RealmLocal}}, nobodySignsIn},
	} {
		t.Run(name, func(t *testing.T) {
			dir := dataDir(t)
			source := filepath.Join(t.TempDir(), "repositorySettings.json")
			if err := os.WriteFile(source, []byte(`[{"name": "demo-raw", "format": "raw", "type": "hosted"}]`), 0o600); err != nil {
				t.Fatal(err)
			}
			if code, out, errOut := run(t, "import-state", "--settings", source); code != 0 {
				t.Fatalf("import-state: %d %q %q", code, out, errOut)
			}
			if c.snap != nil {
				writeState(t, dir, identity, c.snap)
			}
			code, out, errOut := run(t, "serve", "--port", "0", "--bind", "0.0.0.0")
			if code != 1 || out != "" || errOut != c.want {
				t.Errorf("%d %q %q, want %q", code, out, errOut, c.want)
			}
		})
	}
}
