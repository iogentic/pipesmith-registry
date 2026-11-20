package cli

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/channel"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/release"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The commands of the update in place: update, rollback and schema. update
// and rollback work on an install root that holds releases/<version>/, the
// links current and previous, and update.json.

const updateHelpBody = `Updates
  update [--root DIR] [--allow-downgrade] <release directory>
                                       check a release directory with the trusted key of
                                       <root>/update.json, plan the schema migration of
                                       the data directory and check the space for its
                                       copies, install it below <root>/releases, switch
                                       current to it, restart the unit and wait for its
                                       health; it goes back by itself when that fails
                                       and puts the copies of the migrated files back; a
                                       version not higher than the running one, or a
                                       release without the command schema, needs
                                       --allow-downgrade;
                                       --no-catalog-backup migrates catalog.sqlite without
                                       a copy when the space for one is missing
  update --switch <version> [--allow-downgrade] [--root DIR]
                                       the same for <root>/releases/<version>, the
                                       directory of the fetch stage; both forms take
                                       --no-catalog-backup
  update --fetch [--version V [--allow-downgrade]] [--root DIR]
                                       download the highest complete version above
                                       the running one (or V) from the channel of
                                       <root>/update.json into <root>/releases/<version>
                                       and check signature, checksum, bill of materials
                                       and version answer; it restarts nothing; a V not
                                       higher than the running version needs
                                       --allow-downgrade
  update --fetch --scheduled           the same, then the switch stage on the version
                                       staged in this run when scheduled_switch of
                                       <root>/update.json is true (the timer unit)
  rollback [--root DIR]                switch current back to previous, restart the unit
                                       and wait for its health
                                       (--root defaults to /opt/pipesmith-registry)
  schema                               print the schema versions this binary writes

`

var updateCommandNames = []string{"update", "rollback", "schema"}

// updateService restarts the unit; tests swap it for a fake.
var updateService release.ServiceManager = release.Systemctl{}

// updateHealthTimeout bounds the health wait; tests shorten it.
var updateHealthTimeout = release.DefaultHealthTimeout

func addUpdateCommands(commands map[string]*parser, sub func(map[string]*parser, string, string) *parser) {
	update := sub(commands, "update", "update")
	update.add(&action{dest: "root", options: []string{"--root"}})
	update.add(&action{dest: "allow_downgrade", options: []string{"--allow-downgrade"}, nargs: "0", flag: true})
	update.add(&action{dest: "fetch", options: []string{"--fetch"}, nargs: "0", flag: true})
	update.add(&action{dest: "fetch_version", options: []string{"--version"}, metavar: "VERSION"})
	update.add(&action{dest: "switch_version", options: []string{"--switch"}, metavar: "VERSION"})
	update.add(&action{dest: "no_catalog_backup", options: []string{"--no-catalog-backup"}, nargs: "0", flag: true})
	update.add(&action{dest: "scheduled", options: []string{"--scheduled"}, nargs: "0", flag: true})
	update.add(&action{dest: "directory", nargs: "?"})
	update.defaults["run"] = "cmd_update"

	rollback := sub(commands, "rollback", "rollback")
	rollback.add(&action{dest: "root", options: []string{"--root"}})
	rollback.defaults["run"] = "cmd_rollback"

	sub(commands, "schema", "schema").defaults["run"] = "cmd_schema"
}

func (c *command) updater() (*release.Updater, error) {
	root := release.DefaultRoot
	if _, ok := c.text("root"); ok {
		dir, err := c.argPath("root")
		if err != nil {
			return nil, err
		}
		root = dir
	}
	return &release.Updater{Root: root, Service: updateService, Out: c.out, Address: healthAddress,
		HealthCert:    healthCert,
		HealthTimeout: updateHealthTimeout}, nil
}

// healthAddress is the base URL of the health wait: the port of config.json
// of the data directory, on 127.0.0.1 for the bind localhost, else on the
// bind address. config.json holds only loopback binds, so the Host header
// of the request passes the host name gate of serve.
func healthAddress(cfg *release.UpdateConfig) (string, error) {
	data, err := config.Load(cfg.DataDir, nil)
	if err != nil {
		return "", err
	}
	host := data.Bind
	if host == "" || host == "localhost" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(data.Port)), nil
}

// healthCert is tls_cert of config.json of the data directory when tls_key
// is set too, the certificate serve presents; else "" for plain HTTP.
func healthCert(cfg *release.UpdateConfig) (string, error) {
	data, err := config.Load(cfg.DataDir, nil)
	if err != nil {
		return "", err
	}
	if data.TLSCert == "" || data.TLSKey == "" {
		return "", nil
	}
	return data.TLSCert, nil
}

// update runs one of the forms of update: a release directory, --fetch or
// --switch <version>, exactly one of them.
func (c *command) update() (int, error) {
	_, hasDir := c.text("directory")
	_, hasVersion := c.text("fetch_version")
	switchTo, hasSwitch := c.text("switch_version")
	fetch := c.ns.values["fetch"] == true
	opts := release.UpdateOptions{AllowDowngrade: c.ns.values["allow_downgrade"] == true,
		NoCatalogBackup: c.ns.values["no_catalog_backup"] == true}
	forms := 0
	for _, given := range []bool{hasDir, fetch, hasSwitch} {
		if given {
			forms++
		}
	}
	switch {
	case forms > 1:
		return 2, &usageError{program + " update: give one of a release directory, --fetch and --switch <version>"}
	case hasVersion && !fetch:
		return 2, &usageError{program + " update: --version needs --fetch"}
	case c.ns.values["scheduled"] == true && !fetch:
		return 2, &usageError{program + " update: --scheduled needs --fetch"}
	case forms == 0:
		return 2, &usageError{program + " update: give a release directory, --fetch or --switch <version>"}
	case fetch && opts.NoCatalogBackup:
		return 2, &usageError{program + " update: --no-catalog-backup needs a release directory or --switch"}
	case fetch && opts.AllowDowngrade && !hasVersion:
		return 2, &usageError{program + " update: --allow-downgrade with --fetch needs --version"}
	case fetch:
		return c.fetch()
	}
	u, err := c.updater()
	if err != nil {
		return 1, err
	}
	if hasSwitch {
		err = u.Switch(switchTo, opts)
	} else {
		var dir string
		if dir, err = c.argPath("directory"); err != nil {
			return 1, err
		}
		err = u.UpdateWith(dir, opts)
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// schema prints the schema versions of state.sqlite and catalog.sqlite that
// this binary writes, one line of JSON that the update reads.
func (c *command) schema() (int, error) {
	fmt.Fprintf(c.out, "{\"catalog\":%d,\"state\":%d}\n", catalog.SchemaVersion, state.SchemaVersion)
	return 0, nil
}

func (c *command) rollback() (int, error) {
	u, err := c.updater()
	if err != nil {
		return 1, err
	}
	if err := u.Rollback(); err != nil {
		return 1, err
	}
	return 0, nil
}

// fetch runs the fetch stage on the install root. With --scheduled and
// scheduled_switch true in update.json it runs the switch stage on the
// version this run staged.
func (c *command) fetch() (int, error) {
	root := release.DefaultRoot
	if _, ok := c.text("root"); ok {
		dir, err := c.argPath("root")
		if err != nil {
			return 1, err
		}
		root = dir
	}
	scheduled := false
	if c.ns.values["scheduled"] == true {
		cfg, err := release.LoadUpdateConfig(root)
		if err != nil {
			return 1, err
		}
		if scheduled, err = cfg.ScheduledSwitch(); err != nil {
			return 1, err
		}
		if scheduled {
			if _, err := release.RequireDataDir(cfg); err != nil {
				return 1, err
			}
		}
	}
	if c.ns.values["scheduled"] == true {
		if code, err := c.endRecord(root, scheduled); err != nil {
			return code, err
		}
	}
	version, _ := c.text("fetch_version")
	opts := channel.Options{Version: version, AllowDowngrade: c.ns.values["allow_downgrade"] == true}
	f := &channel.Fetcher{Root: root, Out: c.out}
	res, err := f.Fetch(opts)
	if err != nil {
		return 1, err
	}
	if c.ns.values["scheduled"] != true {
		return 0, nil
	}
	switch {
	case !scheduled:
		fmt.Fprintln(c.out, "scheduled_switch: off, nothing restarted")
		return 0, nil
	case res.Existed:
		failed, mark, err := channel.SwitchFailed(root, res.Version)
		if err != nil {
			return 1, err
		}
		if failed {
			return 1, fmt.Errorf("scheduled_switch: the switch to %s failed in an earlier run (%s); the timer does not try it again; run %s update --switch %s, or move %s/%s away so that a fetch stages it anew",
				res.Version, mark, program, res.Version, release.ReleasesDir, res.Version)
		}
		due, err := channel.SwitchDue(root, res.Version)
		if err != nil {
			return 1, err
		}
		if !due {
			fmt.Fprintln(c.out, "scheduled_switch: nothing staged in this run, nothing restarted")
			return 0, nil
		}
		fmt.Fprintf(c.out, "scheduled_switch: %s was staged by an earlier scheduled run whose switch did not begin; trying it again\n", res.Version)
	case res.Nothing:
		fmt.Fprintln(c.out, "scheduled_switch: nothing staged in this run, nothing restarted")
		return 0, nil
	}
	if !opts.AllowDowngrade {
		now, err := release.Layout{Root: root}.ReadLink(release.CurrentLink)
		if err != nil {
			return 1, err
		}
		running, rerr := release.ParseVersion(now)
		staged, serr := release.ParseVersion(res.Version)
		if rerr == nil && serr == nil && staged.Compare(running) <= 0 {
			if err := channel.ClearSwitchDue(root, res.Version); err != nil {
				return 1, err
			}
			fmt.Fprintf(c.out, "scheduled_switch: current runs %s now, not lower than %s; nothing restarted\n", now, res.Version)
			return 0, nil
		}
	}
	fmt.Fprintf(c.out, "scheduled_switch: switching to %s\n", res.Version)
	u, err := c.updater()
	if err != nil {
		return 1, err
	}
	if err := channel.MarkSwitchDue(root, res.Version); err != nil {
		return 1, fmt.Errorf("scheduled_switch: the mark of the switch to %s was not written: %w", res.Version, err)
	}
	err = u.Switch(res.Version, release.UpdateOptions{AllowDowngrade: opts.AllowDowngrade})
	if err != nil && (errors.Is(err, release.ErrLocked) || !u.SwitchBegan()) {
		return 1, fmt.Errorf("%w; the switch to %s did not begin, the next scheduled run tries it again", err, res.Version)
	}
	if derr := channel.ClearSwitchDue(root, res.Version); derr != nil {
		fmt.Fprintf(c.out, "warning: the mark of the switch to %s was not removed: %v\n", res.Version, derr)
	}
	if err != nil {
		if merr := channel.MarkSwitchFailed(root, res.Version, err); merr != nil {
			return 1, fmt.Errorf("%w; the mark of the failed switch was not written: %v", err, merr)
		}
		return 1, err
	}
	return 0, nil
}

// endRecord ends the record of a switch of a run that stopped before the
// scheduled run does anything else (finding R3-2): with scheduled_switch the
// run ends it as update does and exits with 1; without, it only names it and
// exits with 1, as the timer then has no right to switch.
func (c *command) endRecord(root string, scheduled bool) (int, error) {
	path, found := release.PendingRecord(root)
	if !found {
		return 0, nil
	}
	if !scheduled {
		return 1, fmt.Errorf("scheduled run: %s is the record of a switch of a run that stopped; the timer fetches nothing while it is there; run %s update --switch <version> or %s rollback by hand, which end it first",
			path, program, program)
	}
	u, err := c.updater()
	if err != nil {
		return 1, err
	}
	if err := u.EndSwitch(); err != nil {
		return 1, fmt.Errorf("scheduled run: %w", err)
	}
	return 0, nil
}
