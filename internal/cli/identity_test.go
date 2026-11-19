package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// The identity commands of decision N3 (task 17 of the phase 3 plan). Every
// password and token below is invented for these tests.

const (
	newPassword      = "example-new-pass-8"
	testBindPassword = "example-bind-pass-9"
)

// identityHelp are the help texts argparse of Python 3.12 prints at 80
// columns for the identity commands, built as identity.go builds them.
var identityHelp = map[string]string{
	"user": "usage: pipesmith-registry user [-h] {add,password,enable,disable,roles} ...\n\n" +
		"positional arguments:\n  {add,password,enable,disable,roles}\n\n" +
		"options:\n  -h, --help            show this help message and exit\n",
	"user add": "usage: pipesmith-registry user add [-h] [--roles ROLES]\n" +
		"                                   [--first-name FIRST_NAME]\n" +
		"                                   [--last-name LAST_NAME] [--email EMAIL]\n" +
		"                                   [--data DATA]\n" +
		"                                   name\n\n" +
		"positional arguments:\n  name\n\n" +
		"options:\n  -h, --help            show this help message and exit\n  --roles ROLES\n" +
		"  --first-name FIRST_NAME\n  --last-name LAST_NAME\n  --email EMAIL\n  --data DATA\n",
	"user password": "usage: pipesmith-registry user password [-h] [--data DATA] name\n\n" +
		"positional arguments:\n  name\n\noptions:\n  -h, --help   show this help message and exit\n  --data DATA\n",
	"user enable": "usage: pipesmith-registry user enable [-h] [--data DATA] name\n\n" +
		"positional arguments:\n  name\n\noptions:\n  -h, --help   show this help message and exit\n  --data DATA\n",
	"user disable": "usage: pipesmith-registry user disable [-h] [--data DATA] name\n\n" +
		"positional arguments:\n  name\n\noptions:\n  -h, --help   show this help message and exit\n  --data DATA\n",
	"user roles": "usage: pipesmith-registry user roles [-h] [--data DATA] name [roles]\n\n" +
		"positional arguments:\n  name\n  roles\n\noptions:\n  -h, --help   show this help message and exit\n  --data DATA\n",
	"anonymous": "usage: pipesmith-registry anonymous [-h] {on,off} ...\n\n" +
		"positional arguments:\n  {on,off}\n\noptions:\n  -h, --help  show this help message and exit\n",
	"anonymous on": "usage: pipesmith-registry anonymous on [-h] [--user USER] [--roles ROLES]\n" +
		"                                       [--data DATA]\n\n" +
		"options:\n  -h, --help     show this help message and exit\n  --user USER\n  --roles ROLES\n  --data DATA\n",
	"anonymous off": "usage: pipesmith-registry anonymous off [-h] [--data DATA]\n\n" +
		"options:\n  -h, --help   show this help message and exit\n  --data DATA\n",
	"realms": "usage: pipesmith-registry realms [-h] {list,set} ...\n\n" +
		"positional arguments:\n  {list,set}\n\noptions:\n  -h, --help  show this help message and exit\n",
	"realms list": "usage: pipesmith-registry realms list [-h] [--data DATA]\n\n" +
		"options:\n  -h, --help   show this help message and exit\n  --data DATA\n",
	"realms set": "usage: pipesmith-registry realms set [-h] [--default-role DEFAULT_ROLE]\n" +
		"                                     [--data DATA]\n" +
		"                                     realms\n\n" +
		"positional arguments:\n  realms\n\n" +
		"options:\n  -h, --help            show this help message and exit\n  --default-role DEFAULT_ROLE\n  --data DATA\n",
	"ldap": "usage: pipesmith-registry ldap [-h] {password,ca,check} ...\n\n" +
		"positional arguments:\n  {password,ca,check}\n\noptions:\n  -h, --help           show this help message and exit\n",
	"ldap ca": "usage: pipesmith-registry ldap ca [-h] [--data DATA] connection <file|none>\n\n" +
		"positional arguments:\n  connection\n  <file|none>\n\noptions:\n  -h, --help   show this help message and exit\n  --data DATA\n",
	"ldap password": "usage: pipesmith-registry ldap password [-h] [--data DATA] connection\n\n" +
		"positional arguments:\n  connection\n\noptions:\n  -h, --help   show this help message and exit\n  --data DATA\n",
	"ldap check": "usage: pipesmith-registry ldap check [-h] [--data DATA] [connection]\n\n" +
		"positional arguments:\n  connection\n\noptions:\n  -h, --help   show this help message and exit\n  --data DATA\n",
	"token": "usage: pipesmith-registry token [-h] {issue,list,revoke} ...\n\n" +
		"positional arguments:\n  {issue,list,revoke}\n\noptions:\n  -h, --help           show this help message and exit\n",
	"token issue": "usage: pipesmith-registry token issue [-h] --kind {npm,user}\n" +
		"                                      [--source {default,LDAP}] [--data DATA]\n" +
		"                                      user\n\n" +
		"positional arguments:\n  user\n\n" +
		"options:\n  -h, --help            show this help message and exit\n  --kind {npm,user}\n" +
		"  --source {default,LDAP}\n  --data DATA\n",
	"token list": "usage: pipesmith-registry token list [-h] [--source {default,LDAP}]\n" +
		"                                     [--data DATA]\n" +
		"                                     user\n\n" +
		"positional arguments:\n  user\n\n" +
		"options:\n  -h, --help            show this help message and exit\n  --source {default,LDAP}\n  --data DATA\n",
	"token revoke": "usage: pipesmith-registry token revoke [-h] [--data DATA] id\n\n" +
		"positional arguments:\n  id\n\noptions:\n  -h, --help   show this help message and exit\n  --data DATA\n",
}

func TestIdentityCommandHelp(t *testing.T) {
	dir := dataDir(t)
	for command, want := range identityHelp {
		code, out, errOut := run(t, append(strings.Fields(command), "--help")...)
		if code != 0 || out != want || errOut != "" {
			t.Errorf("%s --help: %d\n%s\nwant\n%s", command, code, out, want)
		}
	}
	_, out, _ := run(t, "help")
	for _, line := range []string{"\nIdentity\n", "  user add <name> [--roles a,b]", "  user password <name>", "  anonymous on [--user ID]",
		"  realms set <realm,realm> [--default-role ROLE]", "  ldap password <connection>", "  ldap ca <connection> <file|none>",
		"  ldap check [<connection>]", "  token issue <user> --kind npm|user",
		"  token revoke <id>"} {
		if !strings.Contains(out, line) {
			t.Errorf("help lacks %q", line)
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("help created the data directory")
	}
}

func TestIdentityCommandUsage(t *testing.T) {
	dataDir(t)
	for args, want := range map[string]string{
		"user":                   "error: pipesmith-registry user: the following arguments are required: user_command\n",
		"user add":               "error: pipesmith-registry user add: the following arguments are required: name\n",
		"anonymous":              "error: pipesmith-registry anonymous: the following arguments are required: anonymous_command\n",
		"realms set":             "error: pipesmith-registry realms set: the following arguments are required: realms\n",
		"ldap password":          "error: pipesmith-registry ldap password: the following arguments are required: connection\n",
		"token issue someone":    "error: pipesmith-registry token issue: the following arguments are required: --kind\n",
		"token issue x --kind d": "error: pipesmith-registry token issue: argument --kind: invalid choice: 'd' (choose from npm, user)\n",
		"user frob":              "error: pipesmith-registry user: argument user_command: invalid choice: 'frob' (choose from add, password, enable, disable, roles)\n",
		// a password is never an argument
		"user add someone " + newPassword: "error: pipesmith-registry: unrecognized arguments: " + newPassword + "\n",
	} {
		code, out, errOut := run(t, strings.Fields(args)...)
		if code != 2 || out != "" || !strings.HasPrefix(errOut, want) {
			t.Errorf("%s: %d %q %q", args, code, out, errOut)
		}
	}
}

// infoLog is the log of serve from INFO up, without the time.
func infoLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(fieldHandler{&logHandler{mu: &sync.Mutex{}, w: buf, min: slog.LevelInfo}})
}

// withStdin makes text the standard input of the commands of one test.
func withStdin(t *testing.T, text string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = io.WriteString(w, text)
		_ = w.Close()
	}()
	old := stdin
	stdin = r
	t.Cleanup(func() {
		stdin = old
		_ = r.Close()
	})
}

// withInput runs one identity command with text on standard input; nothing it
// prints may hold a secret of the test.
func withInput(t *testing.T, text string, args ...string) (int, string, string) {
	t.Helper()
	withStdin(t, text)
	code, out, errOut := run(t, args...)
	for _, secret := range []string{newPassword, testBindPassword, "example-admin-1", "example-viewer-1", "$shiro1$"} {
		if strings.Contains(out+errOut, secret) {
			t.Fatalf("%v printed %q:\n%s%s", args, secret, out, errOut)
		}
	}
	return code, out, errOut
}

func stateOf(t *testing.T, dir string) (*model.Snapshot, int64) {
	t.Helper()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	gen, err := st.Generation()
	if err != nil {
		t.Fatal(err)
	}
	snap, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	return snap, gen
}

func userIn(snap *model.Snapshot, id string) *model.User {
	for i := range snap.Users {
		if snap.Users[i].ID == id {
			return &snap.Users[i]
		}
	}
	return nil
}

// signsIn reports whether a fresh service of the data directory takes the
// pair as Basic credentials.
func signsIn(t *testing.T, dir, user, pass string) bool {
	t.Helper()
	var log bytes.Buffer
	sec, err := buildSecurity(dir, nil, nil, logTo(&log))
	if err != nil || sec == nil {
		t.Fatalf("security: %v %v", sec, err)
	}
	defer closeSecurity(sec)
	subject, err := sec.Subject(basicRequest(user, pass))
	return err == nil && subject != nil && subject.User.ID == user
}

func TestUserAddOnAFreshDataDirectory(t *testing.T) {
	dir := dataDir(t)
	code, out, errOut := withInput(t, newPassword+"\n", "user", "add", "demo-admin", "--roles", "nx-admin", "--email", "demo-admin@example.com")
	if code != 0 || errOut != "" {
		t.Fatalf("user add: %d %q %q", code, out, errOut)
	}
	// the state file is new, its realm order empty: the output says how a
	// user signs in, and nothing was switched on without the command
	for _, want := range []string{"added user demo-admin (active, roles: nx-admin)", state.Path(dir),
		"realms set NexusAuthenticatingRealm,NexusAuthorizingRealm"} {
		if !strings.Contains(out, want) {
			t.Errorf("user add output lacks %q:\n%s", want, out)
		}
	}
	snap, gen := stateOf(t, dir)
	u := userIn(snap, "demo-admin")
	if u == nil || u.Status != "active" || u.Source != auth.SourceLocal || !slices.Equal(u.Roles, []string{"nx-admin"}) ||
		u.Email != "demo-admin@example.com" || !strings.HasPrefix(u.PasswordHash, "$shiro1$SHA-512$1024$") ||
		!auth.VerifyPassword(newPassword, u.PasswordHash) || len(snap.Realms) != 0 || gen != 1 {
		t.Fatalf("after user add: %+v, realms %v, generation %d", u, snap.Realms, gen)
	}
	if signsIn(t, dir, "demo-admin", newPassword) {
		t.Fatal("a user signs in while no realm is active")
	}
	if code, out, errOut := withInput(t, "", "realms", "set", "NexusAuthenticatingRealm,NexusAuthorizingRealm"); code != 0 {
		t.Fatalf("realms set: %d %q %q", code, out, errOut)
	}
	if !signsIn(t, dir, "demo-admin", newPassword) || signsIn(t, dir, "demo-admin", newPassword+"x") {
		t.Fatal("the new user does not sign in with its password, or with another")
	}
}

func TestUserAddRefusals(t *testing.T) {
	dir := importedFixture(t)
	_, before := stateOf(t, dir)
	for _, c := range []struct {
		input string
		args  []string
		want  string
	}{
		{newPassword + "\n", []string{"user", "add", "viewer"}, "user viewer exists already"},
		{newPassword + "\n", []string{"user", "add", "a:b"}, "is not allowed"},
		{newPassword + "\n", []string{"user", "add", "demo-new", "--roles", "nx-admin,no-such-role"}, "no role no-such-role"},
		{"", []string{"user", "add", "demo-new"}, "no password on standard input"},
		{"\n", []string{"user", "add", "demo-new"}, "no password on standard input"},
		{newPassword + "\nsecond line\n", []string{"user", "add", "demo-new"}, "more than one line"},
		{"tab\tin it\n", []string{"user", "add", "demo-new"}, "control character"},
		{strings.Repeat("p", auth.MaxPasswordBytes+1) + "\n", []string{"user", "add", "demo-new"}, "1 to 1024 bytes"},
	} {
		code, out, errOut := withInput(t, c.input, c.args...)
		if code != 1 || out != "" || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: %d %q %q, want %q", c.args, code, out, errOut, c.want)
		}
	}
	if _, gen := stateOf(t, dir); gen != before {
		t.Fatalf("a refused command raised the generation from %d to %d", before, gen)
	}
	// the other commands never create a state file
	empty := filepath.Join(t.TempDir(), "empty")
	for _, args := range [][]string{{"user", "password", "x"}, {"user", "disable", "x"}, {"anonymous", "on"}, {"realms", "list"},
		{"ldap", "check"}, {"token", "list", "x"}} {
		code, _, errOut := withInput(t, newPassword+"\n", append(args, "--data", empty)...)
		if code != 1 || !strings.Contains(errOut, "no state.sqlite") {
			t.Errorf("%v without a state file: %d %q", args, code, errOut)
		}
	}
	if _, err := os.Stat(empty); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused command created the data directory: %v", err)
	}
}

func TestUserCommands(t *testing.T) {
	dir := importedFixture(t)
	_, gen := stateOf(t, dir)
	step := func(input string, args ...string) string {
		t.Helper()
		code, out, errOut := withInput(t, input, args...)
		if code != 0 || errOut != "" {
			t.Fatalf("%v: %d %q %q", args, code, out, errOut)
		}
		_, now := stateOf(t, dir)
		if now != gen+1 {
			t.Fatalf("%v: generation %d, want %d", args, now, gen+1)
		}
		gen = now
		return out
	}
	if out := step(newPassword+"\n", "user", "password", "viewer"); !strings.Contains(out, "set the password of user viewer") {
		t.Fatalf("user password: %q", out)
	}
	if !signsIn(t, dir, "viewer", newPassword) || signsIn(t, dir, "viewer", "example-viewer-1") {
		t.Fatal("the new password does not sign in, or the old one still does")
	}
	if out := step("", "user", "disable", "viewer"); !strings.Contains(out, "user viewer is disabled") {
		t.Fatalf("user disable: %q", out)
	}
	if snap, _ := stateOf(t, dir); userIn(snap, "viewer").Status != "disabled" || signsIn(t, dir, "viewer", newPassword) {
		t.Fatal("a disabled user signs in")
	}
	// enable makes a locked user active as well
	if out := step("", "user", "enable", "locked.user"); !strings.Contains(out, "user locked.user is active") {
		t.Fatalf("user enable: %q", out)
	}
	if !signsIn(t, dir, "locked.user", "example-locked-1") {
		t.Fatal("an enabled user does not sign in")
	}
	code, out, _ := withInput(t, "", "user", "roles", "maven-lead")
	if code != 0 || out != "roles of user maven-lead: demo-maven-lead\n" {
		t.Fatalf("user roles without a list: %d %q", code, out)
	}
	if out := step("", "user", "roles", "maven-lead", "demo-viewer, nx-admin"); !strings.Contains(out, "roles of user maven-lead: demo-viewer, nx-admin") {
		t.Fatalf("user roles: %q", out)
	}
	if snap, _ := stateOf(t, dir); !slices.Equal(userIn(snap, "maven-lead").Roles, []string{"demo-viewer", "nx-admin"}) {
		t.Fatalf("roles %v", userIn(snap, "maven-lead").Roles)
	}
	if out := step("", "user", "roles", "maven-lead", ""); !strings.Contains(out, "roles of user maven-lead: none") {
		t.Fatalf("user roles to none: %q", out)
	}
	for _, args := range [][]string{{"user", "password", "nobody"}, {"user", "enable", "nobody"}, {"user", "roles", "nobody"},
		{"user", "roles", "viewer", "no-such-role"}, {"user", "password", "ldap.person"}} {
		code, out, errOut := withInput(t, newPassword+"\n", args...)
		if code != 1 || out != "" || !strings.HasPrefix(errOut, "error: ") {
			t.Errorf("%v: %d %q %q", args, code, out, errOut)
		}
	}
	if _, now := stateOf(t, dir); now != gen {
		t.Fatalf("a refused command raised the generation from %d to %d", gen, now)
	}
}

func TestAnonymousCommands(t *testing.T) {
	dir := importedFixture(t)
	read := "nexus:repository-view:maven2:demo-maven-hosted:read"
	permitted := func() bool {
		t.Helper()
		var log bytes.Buffer
		sec, err := buildSecurity(dir, nil, nil, logTo(&log))
		if err != nil {
			t.Fatal(err)
		}
		defer closeSecurity(sec)
		return sec.AnonymousEnabled() && sec.Permits(nil, read)
	}
	code, out, errOut := withInput(t, "", "anonymous", "on")
	if code != 0 || !strings.Contains(out, "anonymous access on as user anonymous") || !strings.Contains(out, "holds no role") {
		t.Fatalf("anonymous on: %d %q %q", code, out, errOut)
	}
	if permitted() {
		t.Fatal("the anonymous user reads without a role")
	}
	code, out, _ = withInput(t, "", "anonymous", "on", "--roles", "nx-anonymous")
	if code != 0 || !strings.Contains(out, "anonymous access on as user anonymous with the roles nx-anonymous") {
		t.Fatalf("anonymous on --roles: %d %q", code, out)
	}
	snap, _ := stateOf(t, dir)
	if !snap.Anonymous.Enabled || snap.Anonymous.UserID != "anonymous" ||
		!slices.ContainsFunc(snap.RoleMappings, func(m model.RoleMapping) bool {
			return m.UserID == "anonymous" && m.Source == auth.SourceLocal && slices.Equal(m.Roles, []string{"nx-anonymous"})
		}) {
		t.Fatalf("anonymous %+v, mappings %+v", snap.Anonymous, snap.RoleMappings)
	}
	if !permitted() {
		t.Fatal("the anonymous user with nx-anonymous does not read")
	}
	// another user takes the roles of its local row
	code, out, _ = withInput(t, "", "anonymous", "on", "--user", "viewer")
	if code != 0 || !strings.Contains(out, "as user viewer with the roles demo-viewer") || !permitted() {
		t.Fatalf("anonymous on --user viewer: %d %q", code, out)
	}
	if code, out, _ := withInput(t, "", "anonymous", "on", "--roles", "no-such-role"); code != 1 || out != "" {
		t.Fatalf("anonymous on with an unknown role: %d %q", code, out)
	}
	code, out, _ = withInput(t, "", "anonymous", "off")
	if code != 0 || out != "anonymous access off\n" || permitted() {
		t.Fatalf("anonymous off: %d %q", code, out)
	}
}

func TestRealmsCommands(t *testing.T) {
	dir := importedFixture(t)
	code, out, _ := withInput(t, "", "realms", "list")
	want := "ACTIVE  REALM                     WHAT\n" +
		"1       NexusAuthenticatingRealm  Local users\n" +
		"2       NexusAuthorizingRealm     Local authorization\n" +
		"3       LdapRealm                 LDAP directories\n" +
		"4       DockerToken               Docker bearer tokens\n" +
		"-       NpmToken                  npm tokens\n" +
		"-       User-Token-Realm          User tokens\n" +
		"-       DefaultRole               Default role\n"
	if code != 0 || out != want {
		t.Fatalf("realms list: %d\n%s\nwant\n%s", code, out, want)
	}
	for _, c := range []struct{ list, want string }{
		{"NexusAuthenticatingRealm,NoSuchRealm", "realm NoSuchRealm is not one this server builds"},
		{"NexusAuthenticatingRealm,NexusAuthenticatingRealm", "realm NexusAuthenticatingRealm is named twice"},
		{"NexusAuthenticatingRealm,,LdapRealm", "an empty realm"},
		{"", "an empty realm"},
	} {
		if code, out, errOut := withInput(t, "", "realms", "set", c.list); code != 1 || out != "" || !strings.Contains(errOut, c.want) {
			t.Errorf("realms set %q: %d %q %q, want %q", c.list, code, out, errOut, c.want)
		}
	}
	code, out, _ = withInput(t, "", "realms", "set", "User-Token-Realm, NexusAuthenticatingRealm,NpmToken,DockerToken")
	if code != 0 || !strings.Contains(out, "active realms: User-Token-Realm, NexusAuthenticatingRealm, NpmToken, DockerToken") ||
		!strings.Contains(out, "LDAP users do not sign in") {
		t.Fatalf("realms set: %d %q", code, out)
	}
	if snap, _ := stateOf(t, dir); !slices.Equal(snap.Realms, []string{"User-Token-Realm", "NexusAuthenticatingRealm", "NpmToken", "DockerToken"}) {
		t.Fatalf("realms %v", snap.Realms)
	}
	code, out, _ = withInput(t, "", "realms", "list")
	if code != 0 || !strings.HasPrefix(strings.SplitN(out, "\n", 3)[1], "1       User-Token-Realm") ||
		!strings.Contains(out, "-       LdapRealm") {
		t.Fatalf("realms list after set: %q", out)
	}
}

// A realm id of the stored order that this server does not build is listed
// as such (N16).
func TestRealmsListNamesUnknownRealms(t *testing.T) {
	dir := importedFixture(t)
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = st.Update([]state.Kind{state.KindRealms}, func(snap *model.Snapshot) error {
		snap.Realms = append(snap.Realms, "SamlRealm")
		return nil
	})
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	if code, out, _ := withInput(t, "", "realms", "list"); code != 0 || !strings.Contains(out, "5       SamlRealm                 not built here: nobody signs in through it\n") {
		t.Fatalf("realms list: %d %q", code, out)
	}
}

func TestLDAPPasswordCommand(t *testing.T) {
	dir := importedFixture(t)
	_, before := stateOf(t, dir)
	code, out, errOut := withInput(t, testBindPassword+"\n", "ldap", "password", "demo-directory")
	path := state.LDAPSecretPath(dir, fixtureLDAPID)
	if code != 0 || errOut != "" || !strings.Contains(out, path) || !strings.Contains(out, "ldap check demo-directory") {
		t.Fatalf("ldap password: %d %q %q", code, out, errOut)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("secret file: %v %v", info, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != testBindPassword {
		t.Fatalf("secret file holds %q, %v", data, err)
	}
	if _, gen := stateOf(t, dir); gen != before+1 {
		t.Fatalf("generation %d, want %d", gen, before+1)
	}
	// the id names the connection as well
	if code, _, _ := withInput(t, testBindPassword+"\n", "ldap", "password", fixtureLDAPID); code != 0 {
		t.Fatalf("ldap password by id: %d", code)
	}
	if code, out, errOut := withInput(t, testBindPassword+"\n", "ldap", "password", "no-such-directory"); code != 1 || out != "" ||
		!strings.Contains(errOut, "no LDAP connection no-such-directory") {
		t.Fatalf("an unknown connection: %d %q %q", code, out, errOut)
	}

	// serve takes the bind password of the file when its variable is empty
	var log bytes.Buffer
	sec, err := buildSecurity(dir, nil, nil, infoLog(&log))
	if err != nil || sec == nil {
		t.Fatalf("%v %v", sec, err)
	}
	closeSecurity(sec)
	if text := log.String(); strings.Contains(text, "left out") || !strings.Contains(text, "bind password from "+path) ||
		strings.Contains(text, testBindPassword) {
		t.Fatalf("log %q", text)
	}
	// and refuses the file while the group or others may read it
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	sec, err = buildSecurity(dir, nil, nil, infoLog(&log))
	if err != nil || sec == nil {
		t.Fatalf("%v %v", sec, err)
	}
	closeSecurity(sec)
	if text := log.String(); !strings.Contains(text, "LDAP connection demo-directory left out") || !strings.Contains(text, "chmod 600") ||
		strings.Contains(text, testBindPassword) {
		t.Fatalf("log with a file of mode 0644: %q", text)
	}
	// the variable comes first, as before the file existed
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ldapauth.PasswordVariable(fixtureLDAPID), testBindPassword+"-variable")
	log.Reset()
	sec, err = buildSecurity(dir, nil, nil, infoLog(&log))
	if err != nil || sec == nil {
		t.Fatalf("%v %v", sec, err)
	}
	closeSecurity(sec)
	if text := log.String(); !strings.Contains(text, "bind password from "+ldapauth.PasswordVariable(fixtureLDAPID)) {
		t.Fatalf("log with the variable set: %q", text)
	}
}

// fakeDirectory is a directory server on the loopback address that knows
// the service bind of one password: it answers a bind, a search and an
// unbind in the encoding of the LDAP protocol, and records the passwords of
// the binds it saw.
type fakeDirectory struct {
	addr     string
	password string
	mu       sync.Mutex
	binds    []string
}

func newFakeDirectory(t *testing.T, password string) *fakeDirectory {
	t.Helper()
	return newFakeDirectoryTLS(t, password, nil)
}

// newFakeDirectoryTLS is newFakeDirectory over TLS with config, as ldaps
// is; a nil config speaks plain ldap.
func newFakeDirectoryTLS(t *testing.T, password string, config *tls.Config) *fakeDirectory {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if config != nil {
		ln = tls.NewListener(ln, config)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakeDirectory{addr: ln.Addr().String(), password: password}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeDirectory) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.binds)
}

// berElement reads one element: its tag, its content and the whole encoding.
func berElement(r io.Reader) (tag byte, content, whole []byte, err error) {
	head := make([]byte, 2)
	if _, err = io.ReadFull(r, head); err != nil {
		return 0, nil, nil, err
	}
	length := int(head[1])
	whole = append(whole, head...)
	if head[1]&0x80 != 0 {
		n := int(head[1] & 0x7f)
		more := make([]byte, n)
		if _, err = io.ReadFull(r, more); err != nil {
			return 0, nil, nil, err
		}
		whole = append(whole, more...)
		length = 0
		for _, b := range more {
			length = length<<8 | int(b)
		}
	}
	content = make([]byte, length)
	if _, err = io.ReadFull(r, content); err != nil {
		return 0, nil, nil, err
	}
	return head[0], content, append(whole, content...), nil
}

func berWrap(tag byte, content []byte) []byte {
	return append([]byte{tag, byte(len(content))}, content...)
}

func (f *fakeDirectory) serve(conn net.Conn) {
	defer conn.Close()
	for {
		_, message, _, err := berElement(conn)
		if err != nil {
			return
		}
		body := bytes.NewReader(message)
		_, _, id, err := berElement(body) // the message id, kept as encoded
		if err != nil {
			return
		}
		opTag, op, _, err := berElement(body)
		if err != nil {
			return
		}
		result := func(answerTag byte, code byte) {
			done := berWrap(answerTag, []byte{0x0a, 0x01, code, 0x04, 0x00, 0x04, 0x00})
			_, _ = conn.Write(berWrap(0x30, append(slices.Clone(id), done...)))
		}
		switch opTag {
		case 0x60: // bind: version, name, simple password [0]
			fields := bytes.NewReader(op)
			_, _, _, _ = berElement(fields)
			_, _, _, _ = berElement(fields)
			_, password, _, _ := berElement(fields)
			f.mu.Lock()
			f.binds = append(f.binds, string(password))
			f.mu.Unlock()
			if string(password) == f.password {
				result(0x61, 0)
			} else {
				result(0x61, 49) // invalidCredentials
			}
		case 0x63: // search: done, without entries
			result(0x65, 0)
		default: // unbind and anything else end the connection
			return
		}
	}
}

// fakeConnection points the LDAP connection of the fixture at the fake.
func fakeConnection(t *testing.T, dir string, f *fakeDirectory) {
	t.Helper()
	host, port, err := net.SplitHostPort(f.addr)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	err = st.Update([]state.Kind{state.KindLDAP}, func(snap *model.Snapshot) error {
		snap.LDAP[0].Host = host
		snap.LDAP[0].Port = 0
		for _, c := range port {
			snap.LDAP[0].Port = snap.LDAP[0].Port*10 + int(c-'0')
		}
		snap.LDAP[0].ConnectionTimeoutSeconds = 5
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLDAPCheckCommand(t *testing.T) {
	dir := importedFixture(t)
	f := newFakeDirectory(t, testBindPassword)
	fakeConnection(t, dir, f)
	code, out, errOut := withInput(t, "", "ldap", "check")
	if code != 1 || !strings.Contains(out, "demo-directory: refused: ") || !strings.Contains(out, "needs a bind password") {
		t.Fatalf("ldap check without a bind password: %d %q %q", code, out, errOut)
	}
	if code, _, _ := withInput(t, testBindPassword+"-wrong\n", "ldap", "password", "demo-directory"); code != 0 {
		t.Fatal(code)
	}
	code, out, _ = withInput(t, "", "ldap", "check", "demo-directory")
	if code != 1 || !strings.Contains(out, "demo-directory: failed: ") || !strings.Contains(out, "service bind") {
		t.Fatalf("ldap check with a wrong bind password: %d %q", code, out)
	}
	if code, _, _ := withInput(t, testBindPassword+"\n", "ldap", "password", "demo-directory"); code != 0 {
		t.Fatal(code)
	}
	code, out, _ = withInput(t, "", "ldap", "check")
	if code != 0 || !strings.Contains(out, "demo-directory: ok: ldap://"+f.addr+", service bind as cn=reader,dc=example,dc=com, search base dc=example,dc=com found") {
		t.Fatalf("ldap check: %d %q", code, out)
	}
	if got := f.seen(); len(got) != 2 || got[0] != testBindPassword+"-wrong" || got[1] != testBindPassword {
		t.Fatalf("the directory saw the binds %q", got)
	}
	if code, _, errOut := withInput(t, "", "ldap", "check", "no-such-directory"); code != 1 || !strings.Contains(errOut, "no LDAP connection no-such-directory") {
		t.Fatalf("ldap check of an unknown connection: %d %q", code, errOut)
	}
}

// A reload builds a connection again when its bind password changed, and
// keeps the directory of an unchanged one, with its incident counter.
func TestReloadBuildsAConnectionAgainOnlyWhenItChanged(t *testing.T) {
	dir := importedFixture(t)
	f := newFakeDirectory(t, testBindPassword)
	fakeConnection(t, dir, f)
	if code, _, _ := withInput(t, testBindPassword+"-old\n", "ldap", "password", "demo-directory"); code != 0 {
		t.Fatal(code)
	}
	snap, _ := stateOf(t, dir)
	var log bytes.Buffer
	builds := newLDAPBuilds(dir, logTo(&log))
	first := builds.directories(snap.LDAP)
	again := builds.directories(snap.LDAP)
	if len(first) != 1 || len(again) != 1 || first[0] != again[0] {
		t.Fatalf("an unchanged connection was built again: %v %v", first, again)
	}
	if code, _, _ := withInput(t, testBindPassword+"\n", "ldap", "password", "demo-directory"); code != 0 {
		t.Fatal(code)
	}
	changed := builds.directories(snap.LDAP)
	if len(changed) != 1 || changed[0] == first[0] {
		t.Fatalf("a new bind password kept the old directory: %v", changed)
	}
	if _, _, err := changed[0].Login(context.Background(), "someone", "example-user-pass"); !errors.Is(err, auth.ErrWrongPair) {
		t.Fatalf("sign in through the rebuilt connection: %v", err)
	}
	if got := f.seen(); len(got) != 1 || got[0] != testBindPassword {
		t.Fatalf("the directory saw the binds %q", got)
	}
	if strings.Contains(log.String(), testBindPassword) {
		t.Fatal("the log holds the bind password")
	}
}

func TestTokenCommands(t *testing.T) {
	dir := importedFixture(t)
	if code, _, _ := withInput(t, "", "realms", "set", "NexusAuthenticatingRealm,User-Token-Realm,NpmToken"); code != 0 {
		t.Fatal(code)
	}
	_, gen := stateOf(t, dir)
	code, out, errOut := run(t, "token", "issue", "viewer", "--kind", "user")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if code != 0 || len(lines) != 1 || !strings.HasPrefix(lines[0], "UserToken.") || strings.Count(lines[0], ".") != 2 ||
		!strings.Contains(errOut, "issued a user token for user viewer") || !strings.Contains(errOut, "shown once") {
		t.Fatalf("token issue: %d %q %q", code, out, errOut)
	}
	text := lines[0]
	public, secret := strings.Split(text, ".")[1], strings.Split(text, ".")[2]
	if !strings.Contains(errOut, public) || strings.Contains(errOut, secret) {
		t.Fatalf("the note names %q", errOut)
	}
	if _, now := stateOf(t, dir); now != gen+1 {
		t.Fatalf("generation %d, want %d", now, gen+1)
	}
	if !signsIn(t, dir, "viewer", text) {
		t.Fatal("the user token does not sign its user in")
	}
	code, out, _ = run(t, "token", "list", "viewer")
	if code != 0 || !strings.Contains(out, public) || !strings.Contains(out, "user") || strings.Contains(out, secret) {
		t.Fatalf("token list: %d %q", code, out)
	}
	// the whole token is a secret and no argument
	code, out, errOut = run(t, "token", "revoke", text)
	if code != 1 || out != "" || !strings.Contains(errOut, "the part between its dots") || strings.Contains(errOut, secret) {
		t.Fatalf("token revoke of the whole token: %d %q %q", code, out, errOut)
	}
	code, out, _ = run(t, "token", "revoke", public)
	if code != 0 || out != "revoked the user token "+public+" of user viewer\n" || signsIn(t, dir, "viewer", text) {
		t.Fatalf("token revoke: %d %q", code, out)
	}
	if code, _, errOut := run(t, "token", "revoke", public); code != 1 || !strings.Contains(errOut, "no token with the id "+public) {
		t.Fatalf("a second revoke: %d %q", code, errOut)
	}
	if code, out, _ := run(t, "token", "list", "viewer"); code != 0 || out != "no tokens of user viewer (default)\n" {
		t.Fatalf("token list after the revoke: %d %q", code, out)
	}
	for _, args := range [][]string{{"token", "issue", "nobody", "--kind", "npm"}, {"token", "issue", "locked.user", "--kind", "npm"}} {
		if code, out, errOut := run(t, args...); code != 1 || out != "" || !strings.HasPrefix(errOut, "error: ") {
			t.Errorf("%v: %d %q %q", args, code, out, errOut)
		}
	}
	// the cap of N10: the 21st npm token revokes the oldest
	var first string
	for i := range auth.MaxTokensPerKind + 1 {
		code, out, errOut := run(t, "token", "issue", "admin", "--kind", "npm")
		if code != 0 {
			t.Fatalf("issue %d: %d %q", i, code, errOut)
		}
		if i == 0 {
			first = strings.Split(strings.TrimSpace(out), ".")[1]
		}
		if i == auth.MaxTokensPerKind && !strings.Contains(errOut, "revoked the oldest npm token "+first) {
			t.Fatalf("issue %d: %q", i, errOut)
		}
	}
	// a token of a realm that is off is issued with a note
	if code, _, _ := withInput(t, "", "realms", "set", "NexusAuthenticatingRealm"); code != 0 {
		t.Fatal(code)
	}
	if code, _, errOut := run(t, "token", "issue", "admin", "--kind", "user"); code != 0 ||
		!strings.Contains(errOut, "the realm User-Token-Realm is not active") {
		t.Fatalf("token issue while its realm is off: %d %q", code, errOut)
	}
}

// N1: a data directory without users starts closed, and the start names the
// command that adds the first one.
func TestServeStartNamesUserAdd(t *testing.T) {
	dir := dataDir(t)
	st, err := state.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	sec, err := buildSecurity(dir, nil, nil, logTo(&log))
	if err != nil || sec == nil {
		t.Fatalf("%v %v", sec, err)
	}
	closeSecurity(sec)
	if !strings.Contains(log.String(), "nobody can sign in: add the first user with: pipesmith-registry user add <name> --roles nx-admin") {
		t.Fatalf("log %q", log.String())
	}
}

// realms set --default-role names the role the realm DefaultRole gives every
// signed in user (task 6 of the phase 3 plan); the realm needs a role.
func TestRealmsSetDefaultRole(t *testing.T) {
	dir := importedFixture(t)
	if code, out, errOut := withInput(t, "", "realms", "set", "NexusAuthenticatingRealm,DefaultRole"); code != 1 || out != "" ||
		!strings.Contains(errOut, "the realm DefaultRole needs a role: give --default-role <role>") {
		t.Fatalf("DefaultRole without a role: %d %q %q", code, out, errOut)
	}
	if code, _, errOut := withInput(t, "", "realms", "set", "NexusAuthenticatingRealm,DefaultRole", "--default-role", "no-such-role"); code != 1 ||
		!strings.Contains(errOut, "no role no-such-role") {
		t.Fatalf("an unknown default role: %d %q", code, errOut)
	}
	code, out, _ := withInput(t, "", "realms", "set", "NexusAuthenticatingRealm,DefaultRole", "--default-role", "developers")
	if code != 0 || !strings.Contains(out, "active realms: NexusAuthenticatingRealm, DefaultRole") ||
		!strings.Contains(out, "default role: developers, given to every signed in user") {
		t.Fatalf("realms set with a default role: %d %q", code, out)
	}
	snap, _ := stateOf(t, dir)
	if snap.DefaultRole != "developers" || !slices.Equal(snap.Realms, []string{"NexusAuthenticatingRealm", "DefaultRole"}) {
		t.Fatalf("default role %q, realms %v", snap.DefaultRole, snap.Realms)
	}
	// the role stays while the realm order changes without the option
	if code, out, _ := withInput(t, "", "realms", "set", "DefaultRole,NexusAuthenticatingRealm"); code != 0 ||
		!strings.Contains(out, "default role: developers") {
		t.Fatalf("realms set without the option: %d %q", code, out)
	}
	code, out, _ = withInput(t, "", "realms", "list")
	if code != 0 || !strings.Contains(out, "1       DefaultRole               Default role\n") ||
		!strings.HasSuffix(out, "default role: developers\n") {
		t.Fatalf("realms list: %d %q", code, out)
	}
	var log bytes.Buffer
	sec, err := buildSecurity(dir, nil, nil, logTo(&log))
	if err != nil || sec == nil {
		t.Fatalf("%v %v", sec, err)
	}
	subject, err := sec.Subject(basicRequest("maven-lead", "example-maven-1"))
	if err != nil || subject == nil || !sec.Permits(subject, "nexus:settings:read") {
		t.Fatalf("the default role does not reach maven-lead: %v %v", subject, err)
	}
	closeSecurity(sec)
	// a role without its realm takes effect once the realm is active, and the
	// output says so
	if code, out, _ := withInput(t, "", "realms", "set", "NexusAuthenticatingRealm", "--default-role", "demo-viewer"); code != 0 ||
		!strings.Contains(out, "note: the default role demo-viewer takes effect while the realm DefaultRole is active") {
		t.Fatalf("a default role without its realm: %d %q", code, out)
	}
	if code, out, _ := withInput(t, "", "realms", "set", "NexusAuthenticatingRealm", "--default-role", ""); code != 0 ||
		strings.Contains(out, "default role") {
		t.Fatalf("clearing the default role: %d %q", code, out)
	}
	if snap, _ := stateOf(t, dir); snap.DefaultRole != "" {
		t.Fatalf("default role %q after it was cleared", snap.DefaultRole)
	}
}

// The CA file of an ldaps connection (decision N18): ldap ca names it in
// the LDAP settings, and serve and ldap check verify the directory against
// the system roots plus its certificates. There is no switch that skips the
// verification.
func TestLDAPCAFile(t *testing.T) {
	dir := importedFixture(t)
	certFile, keyFile, _, err := testkit.TLSPair(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeDirectoryTLS(t, testBindPassword, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
	fakeConnection(t, dir, f)
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = st.Update([]state.Kind{state.KindLDAP}, func(snap *model.Snapshot) error {
		snap.LDAP[0].Protocol = "ldaps"
		return nil
	})
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	if code, _, _ := withInput(t, testBindPassword+"\n", "ldap", "password", "demo-directory"); code != 0 {
		t.Fatal(code)
	}
	code, out, _ := withInput(t, "", "ldap", "check", "demo-directory")
	if code != 1 || !strings.Contains(out, "demo-directory: failed: ") || !strings.Contains(out, "certificate") {
		t.Fatalf("ldaps without the CA file: %d %q", code, out)
	}
	notPEM := filepath.Join(t.TempDir(), "not-a-certificate.pem")
	if err := os.WriteFile(notPEM, []byte("no certificate here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, before := stateOf(t, dir)
	for _, c := range []struct{ file, want string }{
		{notPEM, "holds no certificate"},
		{filepath.Join(t.TempDir(), "absent.pem"), "absent.pem"},
	} {
		if code, out, errOut := withInput(t, "", "ldap", "ca", "demo-directory", c.file); code != 1 || out != "" || !strings.Contains(errOut, c.want) {
			t.Errorf("ldap ca %s: %d %q %q", c.file, code, out, errOut)
		}
	}
	code, out, errOut := withInput(t, "", "ldap", "ca", "demo-directory", certFile)
	if code != 0 || !strings.Contains(out, "CA file of LDAP connection demo-directory: "+certFile) || errOut != "" {
		t.Fatalf("ldap ca: %d %q %q", code, out, errOut)
	}
	snap, gen := stateOf(t, dir)
	if snap.LDAP[0].CAFile != certFile || gen != before+1 {
		t.Fatalf("CA file %q, generation %d, want %d", snap.LDAP[0].CAFile, gen, before+1)
	}
	code, out, _ = withInput(t, "", "ldap", "check", "demo-directory")
	if code != 0 || !strings.Contains(out, "demo-directory: ok: ldaps://"+f.addr+", certificate verified, service bind as ") {
		t.Fatalf("ldap check with the CA file: %d %q", code, out)
	}
	// serve passes the CA file to the directory it builds
	var log bytes.Buffer
	builds := newLDAPBuilds(dir, logTo(&log))
	dirs, err := builds.build(snap.LDAP, nil)
	if err != nil || len(dirs) != 1 {
		t.Fatalf("serve: %d directories, %v, log %q", len(dirs), err, log.String())
	}
	if _, _, err := dirs[0].Login(context.Background(), "someone", "example-user-pass"); !errors.Is(err, auth.ErrWrongPair) {
		t.Fatalf("sign in over ldaps with the CA file: %v", err)
	}
	// a CA file that cannot be read leaves the connection out; the system
	// roots alone are not taken instead
	if err := os.Rename(certFile, certFile+".gone"); err != nil {
		t.Fatal(err)
	}
	if dirs := builds.directories(snap.LDAP); len(dirs) != 0 || !strings.Contains(log.String(), "LDAP connection demo-directory left out") ||
		!strings.Contains(log.String(), certFile) {
		t.Fatalf("a missing CA file: %d directories, log %q", len(dirs), log.String())
	}
	if code, out, _ := withInput(t, "", "ldap", "ca", "demo-directory", "NONE"); code != 0 || !strings.Contains(out, "no CA file") {
		t.Fatalf("ldap ca none: %d %q", code, out)
	}
	if snap, _ := stateOf(t, dir); snap.LDAP[0].CAFile != "" {
		t.Fatalf("CA file %q after none", snap.LDAP[0].CAFile)
	}
	if strings.Contains(log.String(), testBindPassword) {
		t.Fatal("the log holds the bind password")
	}
}

// Review S4: user add of a name whose earlier user left a token row behind
// (an import removed the user, the token was not used meanwhile) starts the
// new user without that row.
func TestUserAddStartsWithoutTokens(t *testing.T) {
	dir := dataDir(t)
	if code, out, errOut := withInput(t, newPassword+"\n", "user", "add", "demo-first", "--roles", "nx-admin"); code != 0 {
		t.Fatalf("user add: %d %q %q", code, out, errOut)
	}
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, row, err := auth.NewToken(auth.KindNpm, "demo-bob", auth.SourceLocal, time.Now())
	if err == nil {
		_, err = st.AddToken(row, auth.MaxTokensPerKind)
	}
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := withInput(t, newPassword+"\n", "user", "add", "demo-bob"); code != 0 {
		t.Fatalf("user add demo-bob: %d %q %q", code, out, errOut)
	}
	st, err = state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if rows, err := st.Tokens("demo-bob", auth.SourceLocal); err != nil || len(rows) != 0 {
		t.Fatalf("tokens of the new demo-bob: %+v %v", rows, err)
	}
}

// Review N5: anonymous on --user <id> --roles refuses to replace the roles
// of a local user that signs in and changes nothing. Review R9 of the second
// security review: the steps the refusal names, taken in their order, never
// leave requests without credentials with the rights of a user that signs
// in: an id that is no user takes the roles, then the user gets them by user
// roles. Until R9 the refusal named anonymous on --user admin as the first
// step, which gave requests without credentials the rights of admin until
// the second.
func TestAnonymousOnKeepsTheRolesOfASignInUser(t *testing.T) {
	dir := importedFixture(t)
	before, gen := stateOf(t, dir)
	admin := userIn(before, "admin")
	if admin == nil || admin.Status != "active" || admin.PasswordHash == "" || len(admin.Roles) == 0 {
		t.Fatalf("fixture user admin: %+v", admin)
	}
	code, out, errOut := withInput(t, "", "anonymous", "on", "--user", "admin", "--roles", "nx-anonymous")
	if code != 1 || out != "" || !strings.Contains(errOut, "user admin signs in") ||
		!strings.Contains(errOut, "anonymous on --user <id> --roles nx-anonymous") ||
		!strings.Contains(errOut, "user roles admin nx-anonymous") || strings.Contains(errOut, "anonymous on --user admin") {
		t.Fatalf("anonymous on --user admin --roles: %d %q %q", code, out, errOut)
	}
	after, gen2 := stateOf(t, dir)
	if gen2 != gen || !slices.Equal(userIn(after, "admin").Roles, admin.Roles) || after.Anonymous != before.Anonymous {
		t.Fatalf("a refusal changed the state: generation %d to %d, roles %v, anonymous %+v", gen, gen2, userIn(after, "admin").Roles, after.Anonymous)
	}
	// the steps of the refusal in their order, with demo-guest, an id that is
	// no user
	for _, args := range [][]string{
		{"anonymous", "on", "--user", "demo-guest", "--roles", "nx-anonymous"},
		{"user", "roles", "admin", "nx-anonymous"},
	} {
		if code, out, errOut := withInput(t, "", args...); code != 0 {
			t.Fatalf("%v: %d %q %q", args, code, out, errOut)
		}
		snap, _ := stateOf(t, dir)
		if u := userIn(snap, snap.Anonymous.UserID); snap.Anonymous.Enabled && u != nil && u.Status == "active" && u.PasswordHash != "" {
			t.Errorf("after %v requests without credentials hold the rights of user %s, who signs in", args, u.ID)
		}
	}
	if snap, _ := stateOf(t, dir); !slices.Equal(userIn(snap, "admin").Roles, []string{"nx-anonymous"}) || snap.Anonymous.UserID != "demo-guest" {
		t.Fatalf("after the two steps: roles %v, anonymous %+v", userIn(snap, "admin").Roles, snap.Anonymous)
	}
}
