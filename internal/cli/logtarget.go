package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/logship"
	"github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/version"
)

// The command log-target keeps the rows of the setting log_targets of
// state.sqlite and the secret files of the targets. It never prints a
// secret, and it never creates state.sqlite.

const logTargetHelpBody = `Log targets
  log-target list [--json] [--data <dir>]
                                       list the targets with kind, address, files and
                                       whether a secret is set; never the secret
  log-target add <name> --kind <kind> --address <url> [--files a,b] [--user U]
                 [--index I] [--ca-file F] [--queue-lines N] [--queue-bytes N]
                 [--batch-lines N] [--flush-interval-ms N] [--timeout-ms N]
                 [--secret-stdin] [--data <dir>]
                                       add a target that receives the lines of the log
                                       files; kind is syslog, gelf, loki, otlp,
                                       splunk-hec, elasticsearch or http-json;
                                       --secret-stdin reads the secret from standard
                                       input, at a terminal from a prompt, into
                                       secrets/log-target-<name>
  log-target remove <name> [--data <dir>]
                                       remove a target and its secret file
  log-target test <name> [--data <dir>]
                                       send one line through the target and print sent
                                       or the class of the failure; exit code 1 when it
                                       failed

A running serve reads the targets again every 5 seconds.

`

var logTargetCommandNames = []string{"log-target"}

// logTargetNumbers are the number options of log-target add and their keys.
var logTargetNumbers = []struct{ dest, key, option string }{
	{"queue_lines", "queue_lines", "--queue-lines"},
	{"queue_bytes", "queue_bytes", "--queue-bytes"},
	{"batch_lines", "batch_lines", "--batch-lines"},
	{"flush_interval_ms", "flush_interval_ms", "--flush-interval-ms"},
	{"timeout_ms", "timeout_ms", "--timeout-ms"},
}

func addLogTargetCommands(commands map[string]*parser, sub func(map[string]*parser, string, string) *parser) {
	data := func(p *parser) { p.add(&action{dest: "data", options: []string{"--data"}}) }
	group := sub(commands, "log-target", "log-target")
	children := map[string]*parser{}

	list := sub(children, "log-target list", "list")
	list.add(&action{dest: "json", options: []string{"--json"}, nargs: "0", flag: true})
	data(list)
	list.defaults["run"] = "cmd_log_target_list"

	add := sub(children, "log-target add", "add")
	add.add(&action{dest: "name"})
	add.add(&action{dest: "kind", options: []string{"--kind"}, required: true})
	add.add(&action{dest: "address", options: []string{"--address"}, required: true})
	add.add(&action{dest: "files", options: []string{"--files"}})
	for _, n := range logTargetNumbers {
		add.add(&action{dest: n.dest, options: []string{n.option}})
	}
	add.add(&action{dest: "ca_file", options: []string{"--ca-file"}})
	add.add(&action{dest: "user", options: []string{"--user"}})
	add.add(&action{dest: "index", options: []string{"--index"}})
	add.add(&action{dest: "secret_stdin", options: []string{"--secret-stdin"}, nargs: "0", flag: true})
	data(add)
	add.defaults["run"] = "cmd_log_target_add"

	for _, name := range []string{"remove", "test"} {
		p := sub(children, "log-target "+name, name)
		p.add(&action{dest: "name"})
		data(p)
		p.defaults["run"] = "cmd_log_target_" + name
	}
	group.add(&action{dest: "log_target_command", nargs: "parser", required: true,
		choices: []string{"list", "add", "remove", "test"}, commands: children})
}

// logTarget runs a sub command of log-target.
func (c *command) logTarget() (int, error) {
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
	rows, err := targetRows(st)
	if err != nil {
		return 1, err
	}
	name, _ := c.text("name")
	switch c.ns.run {
	case "cmd_log_target_list":
		return c.logTargetList(rows)
	case "cmd_log_target_add":
		return c.logTargetAdd(st, cfg.DataDir, rows)
	case "cmd_log_target_remove":
		return c.logTargetRemove(st, cfg.DataDir, rows, name)
	}
	return c.logTargetTest(cfg.DataDir, rows, name)
}

// targetRows reads the rows of the setting as they are stored, one JSON
// object each; a missing setting holds none.
func targetRows(st *state.Store) ([]json.RawMessage, error) {
	value, found, err := st.Setting(logship.SettingKey)
	if err != nil || !found {
		return nil, err
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(value, &rows); err != nil {
		return nil, fmt.Errorf("the setting %s is not a JSON list", logship.SettingKey)
	}
	return rows, nil
}

// rowName is the name of a stored row, also of one the validation refuses.
func rowName(row json.RawMessage) string {
	var head struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(row, &head) // a row without a name matches no name
	return head.Name
}

// listedTarget is one target as list shows it: the secret only as whether
// one is set.
type listedTarget struct {
	Name            string   `json:"name"`
	Kind            string   `json:"kind"`
	Address         string   `json:"address"`
	Files           []string `json:"files"`
	Secret          bool     `json:"secret"`
	User            string   `json:"user,omitempty"`
	Index           string   `json:"index,omitempty"`
	CAFile          string   `json:"ca_file,omitempty"`
	QueueLines      int      `json:"queue_lines,omitempty"`
	QueueBytes      int64    `json:"queue_bytes,omitempty"`
	BatchLines      int      `json:"batch_lines,omitempty"`
	FlushIntervalMS int      `json:"flush_interval_ms,omitempty"`
	TimeoutMS       int      `json:"timeout_ms,omitempty"`
}

var allLogFiles = []string{"server", "requests", "audit", "tasks"}

func (c *command) logTargetList(rows []json.RawMessage) (int, error) {
	listed := []listedTarget{}
	for i, row := range rows {
		t, err := logship.ParseTarget(row)
		if err != nil {
			fmt.Fprintf(c.err, "warning: row %d of %s is skipped: %v\n", i+1, logship.SettingKey, err)
			continue
		}
		files := t.Files
		if len(files) == 0 {
			files = allLogFiles
		}
		listed = append(listed, listedTarget{Name: t.Name, Kind: t.Kind, Address: t.Address, Files: files,
			Secret: t.Secret != "", User: t.User, Index: t.Index, CAFile: t.CAFile, QueueLines: t.QueueLines,
			QueueBytes: t.QueueBytes, BatchLines: t.BatchLines, FlushIntervalMS: t.FlushIntervalMS, TimeoutMS: t.TimeoutMS})
	}
	if c.ns.values["json"] == true {
		data, err := json.MarshalIndent(listed, "", "  ")
		if err != nil {
			return 1, err
		}
		c.print("%s", data)
		return 0, nil
	}
	if len(listed) == 0 {
		c.print("no log targets")
	}
	for _, t := range listed {
		secret := "no"
		if t.Secret {
			secret = "yes"
		}
		c.print("%s %s %s files=%s secret=%s", t.Name, t.Kind, t.Address, strings.Join(t.Files, ","), secret)
	}
	return 0, nil
}

func (c *command) logTargetAdd(st *state.Store, dataDir string, rows []json.RawMessage) (int, error) {
	name, _ := c.text("name")
	row := map[string]any{"name": name}
	for _, key := range []string{"kind", "address", "user", "index"} {
		if value, ok := c.text(key); ok {
			row[key] = value
		}
	}
	if value, ok := c.text("files"); ok {
		files := []string{}
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				files = append(files, part)
			}
		}
		row["files"] = files
	}
	for _, n := range logTargetNumbers {
		value, ok := c.text(n.dest)
		if !ok {
			continue
		}
		number, err := strconv.Atoi(value)
		if err != nil {
			return 1, refuse("%s %s is not a whole number; nothing changed", n.option, value)
		}
		row[n.key] = number
	}
	if _, ok := c.text("ca_file"); ok {
		path, err := c.argPath("ca_file")
		if err != nil {
			return 1, err
		}
		row["ca_file"] = path
	}
	withSecret := c.ns.values["secret_stdin"] == true
	if withSecret {
		row["secret"] = state.LogTargetSecretName(name)
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		return 1, err
	}
	if _, err := logship.ParseTarget(encoded); err != nil {
		return 1, refuse("%v; nothing changed", err)
	}
	for _, existing := range rows {
		if rowName(existing) == name {
			return 1, refuse("a log target %s exists; remove it first; nothing changed", name)
		}
	}
	path := state.LogTargetSecretPath(dataDir, name)
	if withSecret {
		secret, err := c.targetSecret()
		if err != nil {
			return 1, err
		}
		if err := state.WriteSecret(path, []byte(secret)); err != nil {
			return 1, err
		}
	}
	if err := st.PutSetting(logship.SettingKey, append(rows, encoded)); err != nil {
		if withSecret {
			_ = state.RemoveSecret(path) // the row was not written; its error counts
		}
		return 1, err
	}
	c.print("added log target %s", name)
	if withSecret {
		c.print("wrote the secret to %s", path)
	}
	return 0, nil
}

// maxTargetSecret bounds the secret of a target, as the server reads it.
const maxTargetSecret = state.MaxSecretBytes

// targetSecret reads the secret of add: at a terminal from a prompt without
// echo, else the bytes of standard input up to the first line break.
func (c *command) targetSecret() (string, error) {
	var value string
	if isTerminal(stdin) {
		answer, err := askSecret(stdin, c.err)("secret of the log target: ")
		if err != nil {
			return "", err
		}
		value = answer
	} else {
		data, err := io.ReadAll(io.LimitReader(bufio.NewReader(stdin), maxTargetSecret+2))
		if err != nil {
			return "", err
		}
		line, _, broken := strings.Cut(string(data), "\n")
		if !broken && len(line) > maxTargetSecret {
			return "", refuse("a secret has 1 to %d bytes; nothing changed", maxTargetSecret)
		}
		value = strings.TrimSuffix(line, "\r")
	}
	switch {
	case value == "":
		return "", refuse("no secret on standard input; nothing changed")
	case len(value) > maxTargetSecret:
		return "", refuse("a secret has 1 to %d bytes; nothing changed", maxTargetSecret)
	case strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "", refuse("the secret holds a control character; nothing changed")
	}
	return value, nil
}

func (c *command) logTargetRemove(st *state.Store, dataDir string, rows []json.RawMessage, name string) (int, error) {
	kept := []json.RawMessage{}
	for _, row := range rows {
		if rowName(row) != name {
			kept = append(kept, row)
		}
	}
	if len(kept) == len(rows) {
		return 1, refuse("no log target %s", name)
	}
	if err := st.PutSetting(logship.SettingKey, kept); err != nil {
		return 1, err
	}
	if err := state.RemoveSecret(state.LogTargetSecretPath(dataDir, name)); err != nil {
		return 1, err
	}
	c.print("removed log target %s", name)
	return 0, nil
}

func (c *command) logTargetTest(dataDir string, rows []json.RawMessage, name string) (int, error) {
	for _, row := range rows {
		if rowName(row) != name {
			continue
		}
		t, err := logship.ParseTarget(row)
		if err != nil {
			return 1, refuse("the log target %s is not valid: %v", name, err)
		}
		secret, err := targetSecretFile(dataDir)(t.Secret)
		if err != nil && t.Secret != "" {
			return 1, err
		}
		if err := logship.SendTest(t, secret, version.Version); err != nil {
			c.print("failed: %v", err)
			return 1, nil
		}
		c.print("sent")
		return 0, nil
	}
	return 1, refuse("no log target %s", name)
}

// targetSecretFile reads the secret file name of a target below secrets/ of
// dataDir through state.ReadSecret. It reads files of log targets only, and
// an error names the file, never its content.
func targetSecretFile(dataDir string) func(name string) (string, error) {
	return func(name string) (string, error) {
		if name == "" {
			return "", nil
		}
		path := filepath.Join(dataDir, state.SecretsDir, name)
		if !strings.HasPrefix(name, state.LogTargetSecretName("")) {
			return "", fmt.Errorf("the secret file %s is not a file of a log target", path)
		}
		data, err := state.ReadSecret(path)
		if errors.Is(err, state.ErrNoSecret) {
			return "", fmt.Errorf("the secret file %s does not exist", path)
		}
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}
