package cli

import (
	"bufio"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/authz"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The identity commands of decision N3 of the phase 3 plan. They act on
// state.sqlite of the data directory with the file rights of its owner and
// need no sign in. A password, a bind password or another secret is never
// an argument: it comes from standard input, at a terminal from a prompt
// without echo. Every change raises the generation of state.sqlite, so a
// running serve follows within about auth.ReloadInterval.

const identityHelpBody = `Identity
  user add <name> [--roles a,b] [--first-name F] [--last-name L] [--email E]
                                       add a local user with these roles; its password comes
                                       from standard input, at a terminal from a prompt
                                       without echo
  user password <name>                 set the password of a local user, read the same way
  user enable <name>                   let a local user sign in again
  user disable <name>                  stop a local user from signing in
  user roles <name> [<role,role>]      show the roles of a local user, or set them
  anonymous on [--user ID] [--roles a,b]
                                       let requests without credentials in as this user,
                                       with these roles
  anonymous off                        ask every request for credentials
  realms list                          the active realms in their order, then the others
  realms set <realm,realm> [--default-role ROLE]
                                       set the active realms and their order; the realm
                                       DefaultRole gives ROLE to every signed in user
  ldap password <connection>           write the bind password of an LDAP connection to
                                       secrets/ of the data directory, read as above
  ldap ca <connection> <file|none>     name the file of CA certificates an ldaps connection
                                       is verified against besides the system roots
  ldap check [<connection>]            connect and bind as serve would, and find the search
                                       base
  token issue <user> --kind npm|user [--source default|LDAP]
                                       issue a token of a user and print it; it is shown once
  token list <user> [--source default|LDAP]
                                       the tokens of a user, without their secret part
  token revoke <id>                    revoke a token by its id, the part between its dots

The identity commands take --data <dir> as the commands of the import do and need no sign
in. A change reaches a running serve within about a second.

`

var identityCommandNames = []string{"user", "anonymous", "realms", "ldap", "token"}

// stdin is the standard input of the commands; tests replace it.
var stdin = os.Stdin

func addIdentityCommands(commands map[string]*parser, sub func(map[string]*parser, string, string) *parser) {
	data := func(p *parser) { p.add(&action{dest: "data", options: []string{"--data"}}) }
	group := func(name string, choices []string, build func(map[string]*parser)) {
		p := sub(commands, name, name)
		children := map[string]*parser{}
		build(children)
		p.add(&action{dest: name + "_command", nargs: "parser", required: true, choices: choices, commands: children})
	}

	group("user", []string{"add", "password", "enable", "disable", "roles"}, func(c map[string]*parser) {
		add := sub(c, "user add", "add")
		add.add(&action{dest: "name"})
		add.add(&action{dest: "roles", options: []string{"--roles"}})
		add.add(&action{dest: "first_name", options: []string{"--first-name"}})
		add.add(&action{dest: "last_name", options: []string{"--last-name"}})
		add.add(&action{dest: "email", options: []string{"--email"}})
		data(add)
		add.defaults["run"] = "cmd_user_add"
		for _, name := range []string{"password", "enable", "disable"} {
			p := sub(c, "user "+name, name)
			p.add(&action{dest: "name"})
			data(p)
			p.defaults["run"] = "cmd_user_" + name
		}
		roles := sub(c, "user roles", "roles")
		roles.add(&action{dest: "name"})
		roles.add(&action{dest: "roles", nargs: "?"})
		data(roles)
		roles.defaults["run"] = "cmd_user_roles"
	})
	group("anonymous", []string{"on", "off"}, func(c map[string]*parser) {
		on := sub(c, "anonymous on", "on")
		on.add(&action{dest: "user", options: []string{"--user"}})
		on.add(&action{dest: "roles", options: []string{"--roles"}})
		data(on)
		on.defaults["run"] = "cmd_anonymous_on"
		off := sub(c, "anonymous off", "off")
		data(off)
		off.defaults["run"] = "cmd_anonymous_off"
	})
	group("realms", []string{"list", "set"}, func(c map[string]*parser) {
		list := sub(c, "realms list", "list")
		data(list)
		list.defaults["run"] = "cmd_realms_list"
		set := sub(c, "realms set", "set")
		set.add(&action{dest: "realms"})
		set.add(&action{dest: "default_role", options: []string{"--default-role"}})
		data(set)
		set.defaults["run"] = "cmd_realms_set"
	})
	group("ldap", []string{"password", "ca", "check"}, func(c map[string]*parser) {
		password := sub(c, "ldap password", "password")
		password.add(&action{dest: "connection"})
		data(password)
		password.defaults["run"] = "cmd_ldap_password"
		ca := sub(c, "ldap ca", "ca")
		ca.add(&action{dest: "connection"})
		ca.add(&action{dest: "file", metavar: "<file|none>"})
		data(ca)
		ca.defaults["run"] = "cmd_ldap_ca"
		check := sub(c, "ldap check", "check")
		check.add(&action{dest: "connection", nargs: "?"})
		data(check)
		check.defaults["run"] = "cmd_ldap_check"
	})
	group("token", []string{"issue", "list", "revoke"}, func(c map[string]*parser) {
		sources := []string{auth.SourceLocal, auth.SourceLDAP}
		issue := sub(c, "token issue", "issue")
		issue.add(&action{dest: "user"})
		issue.add(&action{dest: "kind", options: []string{"--kind"}, required: true, choices: []string{auth.KindNpm, auth.KindUser}})
		issue.add(&action{dest: "source", options: []string{"--source"}, choices: sources})
		data(issue)
		issue.defaults["run"] = "cmd_token_issue"
		list := sub(c, "token list", "list")
		list.add(&action{dest: "user"})
		list.add(&action{dest: "source", options: []string{"--source"}, choices: sources})
		data(list)
		list.defaults["run"] = "cmd_token_list"
		revoke := sub(c, "token revoke", "revoke")
		revoke.add(&action{dest: "id"})
		data(revoke)
		revoke.defaults["run"] = "cmd_token_revoke"
	})
}

// identityCommand runs an identity command; false when run names none.
func (c *command) identityCommand() (int, bool, error) {
	commands := map[string]func() (int, error){
		"cmd_user_add": c.userAdd, "cmd_user_password": c.userPassword, "cmd_user_enable": c.userEnable,
		"cmd_user_disable": c.userDisable, "cmd_user_roles": c.userRoles, "cmd_anonymous_on": c.anonymousOn,
		"cmd_anonymous_off": c.anonymousOff, "cmd_realms_list": c.realmsList, "cmd_realms_set": c.realmsSet,
		"cmd_ldap_password": c.ldapPassword, "cmd_ldap_ca": c.ldapCA, "cmd_ldap_check": c.ldapCheck, "cmd_token_issue": c.tokenIssue,
		"cmd_token_list": c.tokenList, "cmd_token_revoke": c.tokenRevoke,
	}
	fn, ok := commands[c.ns.run]
	if !ok {
		return 0, false, nil
	}
	code, err := fn()
	return code, true, err
}

// openState opens state.sqlite of the data directory of --data or of the
// environment. Only user add creates it.
func (c *command) openState(create bool) (*state.Store, *config.Config, error) {
	cfg, err := c.stateConfig()
	if err != nil {
		return nil, nil, err
	}
	if create {
		st, err := state.Create(cfg.DataDir)
		return st, cfg, err
	}
	st, err := state.Open(cfg.DataDir)
	if errors.Is(err, state.ErrAbsent) {
		return nil, nil, refuse("no state.sqlite in %s; write one with import, or add the first user with: user add <name>", cfg.DataDir)
	}
	return st, cfg, err
}

// withState runs fn on the state file and closes it.
func (c *command) withState(create bool, fn func(st *state.Store, cfg *config.Config) (int, error)) (int, error) {
	st, cfg, err := c.openState(create)
	if err != nil {
		return 1, err
	}
	code, err := fn(st, cfg)
	if cerr := st.Close(); err == nil && cerr != nil {
		return 1, cerr
	}
	return code, err
}

// Secrets

const maxSecretLine = auth.MaxPasswordBytes

// secret reads a secret: at a terminal from a prompt without echo, asked
// twice when repeat is set, else the one line of standard input.
func (c *command) secret(prompt string, repeat bool) (string, error) {
	var value string
	if isTerminal(stdin) {
		ask := askSecret(stdin, c.err)
		first, err := ask(prompt)
		if err != nil {
			return "", err
		}
		if repeat {
			second, err := ask("repeat the password: ")
			if err != nil {
				return "", err
			}
			if second != first {
				return "", refuse("the two entries differ; nothing changed")
			}
		}
		value = first
	} else {
		line, err := secretLine(stdin)
		if err != nil {
			return "", err
		}
		value = line
	}
	if value == "" {
		return "", refuse("no password on standard input; nothing changed")
	}
	if len(value) > maxSecretLine {
		return "", refuse("a password has 1 to %d bytes; nothing changed", maxSecretLine)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", refuse("the password holds a control character; nothing changed")
		}
	}
	return value, nil
}

// secretLine is the one line of r: up to its line break, without "\r\n" or
// "\n". A second line refuses the input.
func secretLine(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(bufio.NewReader(r), maxSecretLine+3))
	if err != nil {
		return "", err
	}
	text := string(data)
	line, rest, _ := strings.Cut(text, "\n")
	if rest != "" {
		if len(data) > maxSecretLine+2 {
			return "", refuse("a password has 1 to %d bytes; nothing changed", maxSecretLine)
		}
		return "", refuse("standard input holds more than one line; give the password alone; nothing changed")
	}
	return strings.TrimSuffix(line, "\r"), nil
}

// Users

// userName is the name user add takes: a letter or digit first, then
// letters, digits, '.', '_', '@', '+' and '-', at most 256 bytes. A ':' would
// end the user name of Basic credentials.
var userName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,255}$`)

// roleList is a comma separated list of role ids, each one trimmed; an
// empty text is no role.
func roleList(text string) []string {
	roles := []string{}
	for _, part := range strings.Split(text, ",") {
		if part = strings.TrimSpace(part); part != "" && !slices.Contains(roles, part) {
			roles = append(roles, part)
		}
	}
	return roles
}

// knownRoles refuses a role that is neither stored nor built in.
func knownRoles(snap *model.Snapshot, roles []string) error {
	resolver := authz.NewResolver(snap)
	for _, r := range roles {
		if !slices.Contains(resolver.Roles([]string{r}), r) {
			return refuse("no role %s; the roles are those of the import and nx-admin and nx-anonymous; nothing changed", r)
		}
	}
	return nil
}

func roleText(roles []string) string {
	if len(roles) == 0 {
		return "none"
	}
	return strings.Join(roles, ", ")
}

// localUser is the local user name in snap, or a refusal.
func localUser(snap *model.Snapshot, name string) (*model.User, error) {
	for i := range snap.Users {
		if u := &snap.Users[i]; u.ID == name && u.Source == auth.SourceLocal {
			return u, nil
		}
	}
	return nil, refuse("no local user %s; nothing changed", name)
}

// realmNote is the note of a write when the stored order lets no local
// user sign in.
func (c *command) realmNote(snap *model.Snapshot) {
	if !slices.Contains(snap.Realms, auth.RealmLocal) {
		c.print("note: the realm %s is not active, so no local user signs in; turn it on with: realms set %s,%s",
			auth.RealmLocal, auth.RealmLocal, auth.RealmAuthorizing)
	}
}

func (c *command) userAdd() (int, error) {
	name, _ := c.text("name")
	if !userName.MatchString(name) {
		return 1, refuse("user name %q is not allowed; use letters, digits, '.', '_', '@', '+' and '-', a letter or digit first", name)
	}
	roles := []string{}
	if text, ok := c.text("roles"); ok {
		roles = roleList(text)
	}
	cfg, err := c.stateConfig()
	if err != nil {
		return 1, err
	}
	// refuse what can be refused before the password is asked
	if st, err := state.Open(cfg.DataDir); err == nil {
		snap, lerr := st.Load()
		if cerr := st.Close(); lerr == nil {
			lerr = cerr
		}
		if lerr != nil {
			return 1, lerr
		}
		if err := addable(snap, name, roles); err != nil {
			return 1, err
		}
	} else if !errors.Is(err, state.ErrAbsent) {
		return 1, err
	} else if err := knownRoles(&model.Snapshot{}, roles); err != nil {
		return 1, err
	}
	password, err := c.secret("password of user "+name+": ", true)
	if err != nil {
		return 1, err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return 1, err
	}
	first, _ := c.text("first_name")
	last, _ := c.text("last_name")
	email, _ := c.text("email")
	return c.withState(true, func(st *state.Store, cfg *config.Config) (int, error) {
		var after *model.Snapshot
		err := st.Update([]state.Kind{state.KindUsers}, func(snap *model.Snapshot) error {
			if err := addable(snap, name, roles); err != nil {
				return err
			}
			snap.Users = append(snap.Users, model.User{ID: name, FirstName: first, LastName: last, Email: email, Status: "active",
				Source: auth.SourceLocal, Roles: roles, PasswordHash: hash})
			after = snap
			return nil
		})
		if err != nil {
			return 1, err
		}
		c.print("added user %s (active, roles: %s) to %s", name, roleText(roles), state.Path(cfg.DataDir))
		c.realmNote(after)
		return 0, nil
	})
}

// addable refuses a new local user whose name is taken or whose roles do
// not exist.
func addable(snap *model.Snapshot, name string, roles []string) error {
	for _, u := range snap.Users {
		if u.ID == name {
			return refuse("user %s exists already; set its password with: user password %s", name, name)
		}
	}
	for _, m := range snap.RoleMappings {
		if m.UserID == name && m.Source == auth.SourceLocal {
			return refuse("user %s has roles of the local source already (anonymous on --user %s); nothing changed", name, name)
		}
	}
	return knownRoles(snap, roles)
}

func (c *command) userPassword() (int, error) {
	name, _ := c.text("name")
	return c.withState(false, func(st *state.Store, _ *config.Config) (int, error) {
		snap, err := st.Load()
		if err != nil {
			return 1, err
		}
		if _, err := localUser(snap, name); err != nil {
			return 1, err
		}
		password, err := c.secret("password of user "+name+": ", true)
		if err != nil {
			return 1, err
		}
		hash, err := auth.HashPassword(password)
		if err != nil {
			return 1, err
		}
		err = st.Update([]state.Kind{state.KindUsers}, func(snap *model.Snapshot) error {
			u, err := localUser(snap, name)
			if err == nil {
				u.PasswordHash = hash
			}
			return err
		})
		if err != nil {
			return 1, err
		}
		c.print("set the password of user %s", name)
		return 0, nil
	})
}

func (c *command) userEnable() (int, error)  { return c.userStatus("active", "user %s is active") }
func (c *command) userDisable() (int, error) { return c.userStatus("disabled", "user %s is disabled") }

func (c *command) userStatus(status, done string) (int, error) {
	name, _ := c.text("name")
	return c.withState(false, func(st *state.Store, _ *config.Config) (int, error) {
		err := st.Update([]state.Kind{state.KindUsers}, func(snap *model.Snapshot) error {
			u, err := localUser(snap, name)
			if err == nil {
				u.Status = status
			}
			return err
		})
		if err != nil {
			return 1, err
		}
		c.print(done, name)
		return 0, nil
	})
}

func (c *command) userRoles() (int, error) {
	name, _ := c.text("name")
	text, set := c.text("roles")
	return c.withState(false, func(st *state.Store, _ *config.Config) (int, error) {
		if !set {
			snap, err := st.Load()
			if err != nil {
				return 1, err
			}
			u, err := localUser(snap, name)
			if err != nil {
				return 1, err
			}
			c.print("roles of user %s: %s", name, roleText(u.Roles))
			return 0, nil
		}
		roles := roleList(text)
		err := st.Update([]state.Kind{state.KindUsers}, func(snap *model.Snapshot) error {
			u, err := localUser(snap, name)
			if err != nil {
				return err
			}
			if err := knownRoles(snap, roles); err != nil {
				return err
			}
			u.Roles = roles
			return nil
		})
		if err != nil {
			return 1, err
		}
		c.print("roles of user %s: %s", name, roleText(roles))
		return 0, nil
	})
}

// Anonymous access

// The defaults of the old product for the anonymous user (lab research,
// section Anonymous).
const (
	anonymousUser  = "anonymous"
	anonymousRealm = "NexusAuthorizingRealm"
)

func (c *command) anonymousOn() (int, error) {
	user, userGiven := c.text("user")
	rolesText, rolesGiven := c.text("roles")
	kinds := []state.Kind{state.KindAnonymous}
	if rolesGiven {
		kinds = append(kinds, state.KindUsers)
	}
	return c.withState(false, func(st *state.Store, _ *config.Config) (int, error) {
		var id string
		var roles []string
		local := true
		err := st.Update(kinds, func(snap *model.Snapshot) error {
			a := &snap.Anonymous
			a.Enabled = true
			if userGiven {
				a.UserID = user
			}
			if a.UserID == "" {
				a.UserID = anonymousUser
			}
			if a.RealmName == "" {
				a.RealmName = anonymousRealm
			}
			id = a.UserID
			if rolesGiven {
				roles = roleList(rolesText)
				if err := knownRoles(snap, roles); err != nil {
					return err
				}
				if u := signInUser(snap, id); userGiven && u != nil {
					// review R9: no step of the advice lets requests without
					// credentials hold the rights of a user that signs in
					given := strings.Join(roles, ",")
					return refuse("user %s signs in with the roles %s, and --roles would replace them; nothing changed. "+
						"Requests without credentials should not hold the rights of a user that signs in. To give them the "+
						"roles %s, name an id that is no user: anonymous on --user <id> --roles %s. For user %s to hold "+
						"these roles as well, then run: user roles %s %s", id, roleText(u.Roles), given, given, id, id, given)
				}
				setAnonymousRoles(snap, id, roles)
			}
			roles, local = anonymousRoles(snap, id)
			return nil
		})
		if err != nil {
			return 1, err
		}
		if len(roles) == 0 {
			c.print("anonymous access on as user %s; it holds no role, so requests without credentials are still refused: "+
				"give it one with: anonymous on --roles <role>", id)
			return 0, nil
		}
		c.print("anonymous access on as user %s with the roles %s", id, roleText(roles))
		if !local {
			c.print("note: the local user %s is not active, so it holds no right", id)
		}
		return 0, nil
	})
}

// setAnonymousRoles gives the anonymous user its roles: those of its local
// row, else a role mapping of the local source, which the server reads for
// a user without a row.
// signInUser is the local user of id that signs in with a password: active
// and with a stored form. Nil for any other id (review N5).
func signInUser(snap *model.Snapshot, id string) *model.User {
	for i := range snap.Users {
		if u := &snap.Users[i]; u.ID == id && signsInWithPassword(*u) {
			return u
		}
	}
	return nil
}

// signsInWithPassword reports a local user that signs in with a password:
// active and with a stored form.
func signsInWithPassword(u model.User) bool {
	return u.Source == auth.SourceLocal && u.Status == "active" && u.PasswordHash != ""
}

func setAnonymousRoles(snap *model.Snapshot, id string, roles []string) {
	for i := range snap.Users {
		if snap.Users[i].ID == id && snap.Users[i].Source == auth.SourceLocal {
			snap.Users[i].Roles = roles
			return
		}
	}
	for i := range snap.RoleMappings {
		if snap.RoleMappings[i].UserID == id && snap.RoleMappings[i].Source == auth.SourceLocal {
			snap.RoleMappings[i].Roles = roles
			return
		}
	}
	snap.RoleMappings = append(snap.RoleMappings, model.RoleMapping{UserID: id, Source: auth.SourceLocal, Roles: roles})
}

// anonymousRoles are the roles the server gives the anonymous user, and
// false for a local row that is not active.
func anonymousRoles(snap *model.Snapshot, id string) ([]string, bool) {
	for _, u := range snap.Users {
		if u.ID == id && u.Source == auth.SourceLocal {
			return u.Roles, u.Status == "active"
		}
	}
	var roles []string
	for _, m := range snap.RoleMappings {
		if m.Source == auth.SourceLocal && strings.EqualFold(m.UserID, id) {
			roles = append(roles, m.Roles...)
		}
	}
	return roles, true
}

func (c *command) anonymousOff() (int, error) {
	return c.withState(false, func(st *state.Store, _ *config.Config) (int, error) {
		err := st.Update([]state.Kind{state.KindAnonymous}, func(snap *model.Snapshot) error {
			snap.Anonymous.Enabled = false
			return nil
		})
		if err != nil {
			return 1, err
		}
		c.print("anonymous access off")
		return 0, nil
	})
}

// Realms

func (c *command) realmsList() (int, error) {
	return c.withState(false, func(st *state.Store, _ *config.Config) (int, error) {
		snap, err := st.Load()
		if err != nil {
			return 1, err
		}
		rows := [][]string{{"ACTIVE", "REALM", "WHAT"}}
		for i, id := range snap.Realms {
			what := "not built here: nobody signs in through it"
			if r, ok := auth.RealmByID(id); ok {
				what = r.Name
			}
			rows = append(rows, []string{fmt.Sprint(i + 1), id, what})
		}
		for _, r := range auth.Realms {
			if !slices.Contains(snap.Realms, r.ID) {
				rows = append(rows, []string{"-", r.ID, r.Name})
			}
		}
		if err := table(c.out, rows); err != nil {
			return 1, err
		}
		if role := snap.DefaultRole; role != "" {
			if slices.Contains(snap.Realms, auth.RealmDefaultRole) {
				c.print("default role: %s", role)
			} else {
				c.print("default role: %s, without effect while the realm %s is not active", role, auth.RealmDefaultRole)
			}
		}
		return 0, nil
	})
}

func (c *command) realmsSet() (int, error) {
	text, _ := c.text("realms")
	var realms []string
	for _, part := range strings.Split(text, ",") {
		id := strings.TrimSpace(part)
		switch _, known := auth.RealmByID(id); {
		case id == "":
			return 1, refuse("the list holds an empty realm name; give the realms separated by commas; nothing changed")
		case !known:
			ids := make([]string, len(auth.Realms))
			for i, r := range auth.Realms {
				ids[i] = r.ID
			}
			return 1, refuse("realm %s is not one this server builds; realms: %s; nothing changed", id, strings.Join(ids, ", "))
		case slices.Contains(realms, id):
			return 1, refuse("realm %s is named twice; nothing changed", id)
		}
		realms = append(realms, id)
	}
	role, roleGiven := c.text("default_role")
	kinds := []state.Kind{state.KindRealms}
	if roleGiven {
		role = strings.TrimSpace(role)
		kinds = append(kinds, state.KindDefaultRole)
	}
	return c.withState(false, func(st *state.Store, _ *config.Config) (int, error) {
		var after *model.Snapshot
		err := st.Update(kinds, func(snap *model.Snapshot) error {
			if roleGiven {
				if role != "" {
					if err := knownRoles(snap, []string{role}); err != nil {
						return err
					}
				}
				snap.DefaultRole = role
			}
			if slices.Contains(realms, auth.RealmDefaultRole) && snap.DefaultRole == "" {
				return refuse("the realm %s needs a role: give --default-role <role>; nothing changed", auth.RealmDefaultRole)
			}
			snap.Realms = realms
			after = snap
			return nil
		})
		if err != nil {
			return 1, err
		}
		c.print("active realms: %s", strings.Join(realms, ", "))
		switch {
		case after.DefaultRole == "":
		case slices.Contains(realms, auth.RealmDefaultRole):
			c.print("default role: %s, given to every signed in user", after.DefaultRole)
		default:
			c.print("note: the default role %s takes effect while the realm %s is active", after.DefaultRole, auth.RealmDefaultRole)
		}
		c.realmNote(after)
		if len(after.LDAP) > 0 && !slices.Contains(realms, auth.RealmLDAP) {
			c.print("note: LDAP users do not sign in: the realm %s is not active", auth.RealmLDAP)
		}
		return 0, nil
	})
}

// LDAP

// connection is the LDAP connection of a name or an id.
func connection(snap *model.Snapshot, given string) (model.LDAPServer, error) {
	for _, s := range snap.LDAP {
		if s.Name == given {
			return s, nil
		}
	}
	for _, s := range snap.LDAP {
		if s.ID == given {
			return s, nil
		}
	}
	return model.LDAPServer{}, refuse("no LDAP connection %s; see: state show", given)
}

func (c *command) ldapPassword() (int, error) {
	given, _ := c.text("connection")
	return c.withState(false, func(st *state.Store, cfg *config.Config) (int, error) {
		snap, err := st.Load()
		if err != nil {
			return 1, err
		}
		server, err := connection(snap, given)
		if err != nil {
			return 1, err
		}
		password, err := c.secret("bind password of LDAP connection "+server.Name+": ", false)
		if err != nil {
			return 1, err
		}
		path := state.LDAPSecretPath(cfg.DataDir, server.ID)
		if err := state.WriteSecret(path, []byte(password)); err != nil {
			return 1, err
		}
		if err := st.RaiseGeneration(); err != nil {
			return 1, err
		}
		c.print("wrote the bind password of LDAP connection %s to %s (mode 0600); check it with: ldap check %s", server.Name, path, server.Name)
		if !strings.EqualFold(strings.TrimSpace(server.AuthScheme), "SIMPLE") {
			c.print("note: the connection binds with the scheme %s, which takes no bind password", server.AuthScheme)
		}
		if variable := ldapauth.PasswordVariable(server.ID); os.Getenv(variable) != "" {
			c.print("note: %s is set here; serve takes the variable before the file", variable)
		}
		return 0, nil
	})
}

// maxCABytes bounds a CA file.
const maxCABytes = 1 << 20

// readCA reads a file of CA certificates, at most maxCABytes.
func readCA(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxCABytes {
		return nil, fmt.Errorf("%s is no regular file of at most %d bytes", path, maxCABytes)
	}
	return os.ReadFile(path) // #nosec G304 -- the CA file the operator names in the LDAP settings.
}

func (c *command) ldapCA() (int, error) {
	given, _ := c.text("connection")
	file, _ := c.text("file")
	path := ""
	if !strings.EqualFold(file, "none") {
		var err error
		if path, err = c.argPath("file"); err != nil {
			return 1, err
		}
		pem, err := readCA(path)
		if err != nil {
			return 1, refuse("%v; nothing changed", err)
		}
		if !x509.NewCertPool().AppendCertsFromPEM(pem) {
			return 1, refuse("%s holds no certificate in PEM form; nothing changed", path)
		}
	}
	return c.withState(false, func(st *state.Store, _ *config.Config) (int, error) {
		var server model.LDAPServer
		err := st.Update([]state.Kind{state.KindLDAP}, func(snap *model.Snapshot) error {
			found, err := connection(snap, given)
			if err != nil {
				return err
			}
			for i := range snap.LDAP {
				if snap.LDAP[i].ID == found.ID {
					snap.LDAP[i].CAFile = path
					server = snap.LDAP[i]
				}
			}
			return nil
		})
		if err != nil {
			return 1, err
		}
		if path == "" {
			c.print("LDAP connection %s has no CA file; ldaps verifies it against the system roots only", server.Name)
			return 0, nil
		}
		c.print("CA file of LDAP connection %s: %s; serve reads it at start and at each change", server.Name, path)
		if !strings.EqualFold(strings.TrimSpace(server.Protocol), "ldaps") {
			c.print("note: the connection uses the protocol %s; the CA file counts for ldaps only", server.Protocol)
		}
		return 0, nil
	})
}

// checkTime bounds one check of a connection.
const checkTime = 60 * time.Second

func (c *command) ldapCheck() (int, error) {
	given, one := c.text("connection")
	return c.withState(false, func(st *state.Store, cfg *config.Config) (int, error) {
		snap, err := st.Load()
		if err != nil {
			return 1, err
		}
		servers := snap.LDAP
		if one {
			server, err := connection(snap, given)
			if err != nil {
				return 1, err
			}
			servers = []model.LDAPServer{server}
		}
		if len(servers) == 0 {
			return 1, refuse("no LDAP connection in %s", state.Path(cfg.DataDir))
		}
		failed := 0
		for _, server := range ordered(servers) {
			src, err := bindPassword(cfg.DataDir, server, nil)
			var d *ldapauth.Directory
			if err == nil {
				d, err = ldapauth.New(server, src.password, src.options...)
			}
			if err != nil {
				failed++
				c.print("%s: refused: %v", server.Name, err)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), checkTime)
			done, err := d.Check(ctx)
			cancel()
			if err != nil {
				failed++
				c.print("%s: failed: %v", server.Name, err)
				continue
			}
			c.print("%s: ok: %s", server.Name, done)
		}
		if failed > 0 {
			return 1, nil
		}
		return 0, nil
	})
}

// Tokens

func (c *command) tokenSource() string {
	if source, ok := c.text("source"); ok {
		return source
	}
	return auth.SourceLocal
}

func (c *command) tokenIssue() (int, error) {
	user, _ := c.text("user")
	kind, _ := c.text("kind")
	source := c.tokenSource()
	return c.withState(false, func(st *state.Store, _ *config.Config) (int, error) {
		snap, err := st.Load()
		if err != nil {
			return 1, err
		}
		if source == auth.SourceLocal {
			u, err := localUser(snap, user)
			if err != nil {
				return 1, refuse("no local user %s; a token of a directory user takes --source %s", user, auth.SourceLDAP)
			}
			if u.Status != "active" {
				return 1, refuse("user %s is %s, and its token would sign nobody in; nothing changed", user, u.Status)
			}
		}
		text, row, err := auth.NewToken(kind, user, source, time.Now())
		if err != nil {
			return 1, err
		}
		revoked, err := st.AddToken(row, auth.MaxTokensPerKind)
		if err != nil {
			return 1, err
		}
		c.print("%s", text)
		fmt.Fprintf(c.err, "issued a%s %s token for user %s (%s), id %s; it is shown once, above\n", article(kind), kind, user, source, row.Public)
		for _, public := range revoked {
			fmt.Fprintf(c.err, "revoked the oldest %s token %s of user %s: a user holds at most %d per kind\n", kind, public, user,
				auth.MaxTokensPerKind)
		}
		if realm := auth.TokenRealm(kind); !slices.Contains(snap.Realms, realm) {
			fmt.Fprintf(c.err, "note: the realm %s is not active, so the token signs nobody in until: realms set ...,%s\n", realm, realm)
		}
		return 0, nil
	})
}

func article(kind string) string {
	if kind == auth.KindNpm {
		return "n"
	}
	return ""
}

func (c *command) tokenList() (int, error) {
	user, _ := c.text("user")
	source := c.tokenSource()
	return c.withState(false, func(st *state.Store, _ *config.Config) (int, error) {
		list, err := st.Tokens(user, source)
		if err != nil {
			return 1, err
		}
		if len(list) == 0 {
			c.print("no tokens of user %s (%s)", user, source)
			return 0, nil
		}
		rows := [][]string{{"ID", "KIND", "ISSUED", "LAST USED"}}
		for _, t := range list {
			rows = append(rows, []string{t.Public, t.Kind, minute(t.Created), minute(t.LastUsed)})
		}
		if err := table(c.out, rows); err != nil {
			return 1, err
		}
		return 0, nil
	})
}

func minute(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// tokenID is the form of the public half of a token, its id.
var tokenID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

func (c *command) tokenRevoke() (int, error) {
	id, _ := c.text("id")
	if strings.Contains(id, ".") {
		return 1, refuse("give the id of the token, the part between its dots: the whole token is a secret and no argument; nothing changed")
	}
	if !tokenID.MatchString(id) {
		return 1, refuse("no token with the id %s; see: token list <user>", id)
	}
	return c.withState(false, func(st *state.Store, _ *config.Config) (int, error) {
		row, found, err := st.Token(id)
		if err != nil {
			return 1, err
		}
		removed := false
		if found {
			if removed, err = st.RemoveToken(id); err != nil {
				return 1, err
			}
		}
		if !removed {
			return 1, refuse("no token with the id %s; see: token list <user>", id)
		}
		c.print("revoked the %s token %s of user %s", row.Kind, id, row.UserID)
		return 0, nil
	})
}
