package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/importer"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The commands of the import: import, import-state and state show. None of
// them prints a password hash or a removed secret.

const stateHelpBody = `State
  import <directory> [--data <dir>]    check an export of the old database (tools/h2export.sh)
                                       and write its records into state.sqlite, its
                                       repositories also into repositories.json; only the
                                       kinds of records the export holds are replaced
  import-state --settings <file> [--data <dir>]
                                       write the repositories of a repository settings
                                       export into state.sqlite and repositories.json
  state show [--data <dir>]            print the counts and records of state.sqlite as JSON,
                                       without password hashes

`

var stateCommandNames = []string{"import", "import-state", "state"}

func addStateCommands(commands map[string]*parser, sub func(map[string]*parser, string, string) *parser) {
	imp := sub(commands, "import", "import")
	imp.add(&action{dest: "directory"})
	imp.add(&action{dest: "data", options: []string{"--data"}})
	imp.defaults["run"] = "cmd_import"

	settings := sub(commands, "import-state", "import-state")
	settings.add(&action{dest: "settings", options: []string{"--settings"}, required: true})
	settings.add(&action{dest: "data", options: []string{"--data"}})
	settings.defaults["run"] = "cmd_import_state"

	stateCommand := sub(commands, "state", "state")
	stateCommands := map[string]*parser{}
	show := sub(stateCommands, "state show", "show")
	show.add(&action{dest: "data", options: []string{"--data"}})
	show.defaults["run"] = "cmd_state_show"
	stateCommand.add(&action{dest: "state_command", nargs: "parser", required: true, choices: []string{"show"},
		commands: stateCommands})
}

// stateConfig is the configuration of --data, else of the environment.
func (c *command) stateConfig() (*config.Config, error) {
	dir := ""
	if value, ok := c.text("data"); ok {
		expanded, err := pathExpandUser(value)
		if err != nil {
			return nil, err
		}
		if dir, err = abspath(expanded); err != nil {
			return nil, err
		}
	}
	return config.Load(dir, nil)
}

func (c *command) argPath(key string) (string, error) {
	value, _ := c.text(key)
	expanded, err := pathExpandUser(value)
	if err != nil {
		return "", err
	}
	return abspath(expanded)
}

func kindNames(kinds []state.Kind) string {
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// report prints what an import wrote: counts per kind, never a value of a
// hash or a secret.
func (c *command) report(source string, r *importer.Result) {
	s := r.Snapshot
	c.print("imported set %s from %s", r.Set, source)
	var counts []string
	has := func(k state.Kind) bool { return slices.Contains(r.Kinds, k) }
	if has(state.KindUsers) {
		counts = append(counts, fmt.Sprintf("users %d (%d with a password hash)", len(s.Users), r.PasswordHashes),
			fmt.Sprintf("role mappings %d", len(s.RoleMappings)))
	}
	if has(state.KindRoles) {
		counts = append(counts, fmt.Sprintf("roles %d", len(s.Roles)))
	}
	if has(state.KindPrivileges) {
		counts = append(counts, fmt.Sprintf("privileges %d", len(s.Privileges)))
	}
	if has(state.KindLDAP) {
		counts = append(counts, fmt.Sprintf("LDAP servers %d", len(s.LDAP)))
	}
	if has(state.KindRealms) {
		counts = append(counts, fmt.Sprintf("realms %d", len(s.Realms)))
	}
	if has(state.KindAnonymous) {
		counts = append(counts, "anonymous access "+onOff(s.Anonymous.Enabled))
	}
	if has(state.KindBlobStores) {
		counts = append(counts, fmt.Sprintf("blob stores %d", len(s.BlobStores)))
	}
	if has(state.KindRepositories) {
		counts = append(counts, fmt.Sprintf("repositories %d", len(s.Repositories)))
	}
	c.print("%s", strings.Join(counts, ", "))
	c.print("replaced in %s: %s", r.StatePath, kindNames(r.Kinds))
	if r.RepositoriesPath != "" {
		c.print("wrote %s", r.RepositoriesPath)
	}
	if has(state.KindUsers) && len(s.Users) > 0 && r.PasswordHashes == 0 {
		c.print("note: the export holds no password hashes, so no local user can sign in")
	}
	if len(r.NeedsCredentials) > 0 {
		c.print("note: the credentials of %d repositories were removed; enter them again: %s", len(r.NeedsCredentials),
			strings.Join(r.NeedsCredentials, ", "))
	}
	if len(r.UnmappedLDAPKeys) > 0 {
		c.print("note: LDAP keys without a field, not imported: %s", strings.Join(r.UnmappedLDAPKeys, ", "))
	}
	if has(state.KindLDAP) && len(s.LDAP) > 0 {
		c.print("note: LDAP bind passwords are never imported; give them at the start of serve")
	}
}

func (c *command) importExport() (int, error) {
	cfg, err := c.stateConfig()
	if err != nil {
		return 1, err
	}
	dir, err := c.argPath("directory")
	if err != nil {
		return 1, err
	}
	if ok, err := isDir(dir); err != nil || !ok {
		if err != nil {
			return 1, err
		}
		return 1, refuse("export directory %s does not exist", dir)
	}
	result, err := importer.Import(dir, cfg)
	if err != nil {
		return 1, err
	}
	c.report(dir, result)
	return 0, nil
}

func (c *command) importState() (int, error) {
	cfg, err := c.stateConfig()
	if err != nil {
		return 1, err
	}
	source, err := c.argPath("settings")
	if err != nil {
		return 1, err
	}
	if ok, err := isFile(source); err != nil || !ok {
		if err != nil {
			return 1, err
		}
		return 1, refuse("settings file %s does not exist", source)
	}
	result, err := importer.ImportSettings(source, cfg)
	if err != nil {
		return 1, err
	}
	c.report(source, result)
	return 0, nil
}

// shownState is the document of state show. User has no JSON key for its
// hash, so no hash can reach it.
type shownState struct {
	Schema                int                 `json:"schema"`
	Path                  string              `json:"path"`
	Counts                map[string]int      `json:"counts"`
	UsersWithPasswordHash int                 `json:"usersWithPasswordHash"`
	Users                 []model.User        `json:"users"`
	RoleMappings          []model.RoleMapping `json:"roleMappings"`
	Roles                 []model.Role        `json:"roles"`
	Privileges            []model.Privilege   `json:"privileges"`
	LDAP                  []model.LDAPServer  `json:"ldapServers"`
	Realms                []string            `json:"realms"`
	Anonymous             model.Anonymous     `json:"anonymous"`
	BlobStores            []model.BlobStore   `json:"blobStores"`
	Repositories          []model.Repository  `json:"repositories"`
}

func (c *command) stateShow() (int, error) {
	cfg, err := c.stateConfig()
	if err != nil {
		return 1, err
	}
	st, err := state.Open(cfg.DataDir)
	if errors.Is(err, state.ErrAbsent) {
		return 1, refuse("no state.sqlite in %s; write one with import or import-state", cfg.DataDir)
	}
	if err != nil {
		return 1, err
	}
	defer st.Close()
	snap, err := st.Load()
	if err != nil {
		return 1, err
	}
	hashes := 0
	for i := range snap.Users {
		if snap.Users[i].PasswordHash != "" {
			hashes++
		}
		snap.Users[i].PasswordHash = ""
	}
	doc := shownState{Schema: state.SchemaVersion, Path: state.Path(cfg.DataDir), Counts: state.Counts(snap),
		UsersWithPasswordHash: hashes, Users: snap.Users, RoleMappings: snap.RoleMappings, Roles: snap.Roles,
		Privileges: snap.Privileges, LDAP: snap.LDAP, Realms: snap.Realms, Anonymous: snap.Anonymous,
		BlobStores: snap.BlobStores, Repositories: snap.Repositories}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return 1, err
	}
	c.print("%s", data)
	return 0, nil
}
