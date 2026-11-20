package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/logs/audit"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The audit line of a command (L5, task 7.18): every command that changes
// state writes one line with the subject cli once it ended, result ok for
// exit code 0 and failed else, and the names of the fields of state.sqlite
// it changed. The line goes to audit.log of the log directory of the data
// directory, when that directory exists, and only while no server holds the
// lock of that directory: the
// rotation of a running server could lose a line appended beside it. With
// the lock held the line waits in audit-pending of the log directory, and
// the server moves it into audit.log and to its log targets within one
// tick (finding 7b-1). A log directory that is off takes no line.

// commandAudit is what the line of a command names: its action, the type
// of its object, and the option whose value names the object.
type commandAudit struct {
	action, objectType, objectKey string
}

// changingCommands are the commands that change state, by run name.
var changingCommands = map[string]commandAudit{
	"cmd_store_add":             {"store.create", "store", "name"},
	"cmd_store_remove":          {"store.delete", "store", "name"},
	"cmd_scan":                  {"index.scan", "store", "name"},
	"cmd_repos_port":            {"repository.port", "repository", "name"},
	"cmd_repos_remote_secret":   {"repository.remote_secret", "repository", "name"},
	"cmd_repos_write_settings":  {"repository.write_settings", "repository", "name"},
	"cmd_import_settings":       {"settings.import", "file", "file"},
	"cmd_import":                {"export.import", "directory", "directory"},
	"cmd_import_state":          {"state.import", "file", "settings"},
	"cmd_maven_rebuild":         {"maven.rebuild_metadata", "repository", "repository"},
	"cmd_update":                {"release.update", "release", "directory"},
	"cmd_rollback":              {"release.rollback", "release", ""},
	"cmd_log_target_add":        {"log_target.create", "log_target", "name"},
	"cmd_log_target_remove":     {"log_target.delete", "log_target", "name"},
	"cmd_user_add":              {"user.create", "user", "name"},
	"cmd_user_password":         {"user.password", "user", "name"},
	"cmd_user_enable":           {"user.enable", "user", "name"},
	"cmd_user_disable":          {"user.disable", "user", "name"},
	"cmd_user_roles":            {"user.roles", "user", "name"},
	"cmd_anonymous_on":          {"anonymous.enable", "anonymous", "user"},
	"cmd_anonymous_off":         {"anonymous.disable", "anonymous", ""},
	"cmd_realms_set":            {"realms.update", "realms", ""},
	"cmd_ldap_password":         {"ldap.password", "ldap", "connection"},
	"cmd_ldap_ca":               {"ldap.ca", "ldap", "connection"},
	"cmd_token_issue":           {"token.issue", "user", "user"},
	"cmd_token_revoke":          {"token.revoke", "token", "id"},
	"cmd_token_forget_imported": {"token.forget_imported", "token", ""},
}

// readCommands are the commands that change nothing; a command in neither
// list fails TestEveryCommandIsClassified.
var readCommands = map[string]bool{
	"cmd_serve": true, "cmd_store_list": true, "cmd_repos": true, "cmd_docker_check": true, "cmd_state_show": true,
	"cmd_schema": true, "cmd_log_target_list": true, "cmd_log_target_test": true, "cmd_config": true, "cmd_version": true,
	"cmd_realms_list": true, "cmd_ldap_check": true, "cmd_token_list": true,
}

// audited runs the command and writes its audit line when it changes state.
func (c *command) audited() (int, error) {
	spec, changes := changingCommands[c.ns.run]
	if !changes {
		return c.run()
	}
	cfg, cfgErr := c.stateConfig()
	// an update or a rollback changes the binary, not the records, and must
	// not find state.sqlite opened by this binary first
	records := spec.objectType != "release"
	var before *model.Snapshot
	if cfgErr == nil && records {
		before = snapshotOf(cfg.DataDir)
	}
	code, err := c.run()
	if cfgErr != nil {
		if cfg, cfgErr = c.stateConfig(); cfgErr != nil {
			return code, err // no data directory: no log directory either
		}
	}
	e := logs.AuditEvent{Subject: audit.Subject, Action: spec.action, ObjectType: spec.objectType, Result: audit.OK}
	if spec.objectKey != "" {
		e.ObjectName, _ = c.text(spec.objectKey)
	}
	if code != 0 || err != nil {
		e.Result = audit.Failed
	}
	if records {
		if after := snapshotOf(cfg.DataDir); before != nil || after != nil {
			e.Fields = audit.Changed(orEmpty(before), orEmpty(after))
		}
	}
	c.writeAudit(cfg, audit.Capped(e))
	return code, err
}

// snapshotOf reads the records of state.sqlite; nil without the file or
// when it cannot be read.
func snapshotOf(dataDir string) *model.Snapshot {
	st, err := state.Open(dataDir)
	if err != nil {
		return nil
	}
	defer func() { _ = st.Close() }() // read only
	snap, err := st.Load()
	if err != nil {
		return nil
	}
	return snap
}

func orEmpty(s *model.Snapshot) *model.Snapshot {
	if s == nil {
		return &model.Snapshot{}
	}
	return s
}

// writeAudit appends the line to audit.log while no server holds the log
// directory, else hands it to that server through audit-pending; a line
// that reaches neither goes to standard error with a warning.
func (c *command) writeAudit(cfg *config.Config, e logs.AuditEvent) {
	dir, on := config.LogDirFrom("", os.Getenv, cfg)
	if !on {
		return
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return // a command makes no log directory; serve may keep it off with --log-dir off
	}
	settings, _ := logSettings(cfg.DataDir) // a setting that cannot be read keeps the defaults
	err := logs.AppendAudit(logs.Options{Dir: dir, Settings: settings}, e)
	if err == nil {
		return
	}
	line := strings.TrimSuffix(string(logs.AuditLine(e, time.Now())), "\n")
	fmt.Fprintf(c.err, "warning: the audit line did not reach audit.log (%s): %s\n", logs.LineText(err.Error()), line)
}
