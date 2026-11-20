package cli

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/logship"
	"github.com/iogentic/pipesmith-registry/internal/release"
	"github.com/iogentic/pipesmith-registry/internal/server"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// logsLogger names the lines about the log files themselves.
const logsLogger = "pipesmith_registry.logs"

// logFiles sets opts.Logs, which opens the log files of serve once
// server.Serve has passed its refusals: server.log as the second sink of the
// log handlers on standard error, requests.log as the sink of the request
// records. It cannot fail: with the files off or a directory it cannot use,
// the server serves all the same. With the files off the lines are still
// made and handed to the log targets (logtargets.go), whose manager starts
// once the sink is set. The function it returns closes the manager first,
// so its stop lines reach the files, then removes the sink,
// writes the lines that wait and closes the files, when they were opened. A
// line logged before the files open, the security summary of the start among
// them, is on standard error only.
func (c *command) logFiles(cfg *config.Config, opts *server.ServeOptions) func() {
	option, _ := c.text("log_dir")
	dir, on := config.LogDirFrom(option, os.Getenv, cfg)
	if !on {
		dir = "" // the files are off: the lines are made for the log targets only
	}
	var files *logs.Files
	stopHistory := func() {}
	var targets atomic.Pointer[logship.Manager]
	ship := func(line logs.Line) {
		if m := targets.Load(); m != nil {
			m.Ship(line)
		}
	}
	levels, levelWarnings := logLevels(cfg.DataDir, dir, opts) // the log routes and the level of each logger (loglevels.go)
	opts.Logs = func() func(logs.Request) {
		settings, warnings := logSettings(cfg.DataDir)
		files = logs.Open(logs.Options{Dir: dir, Settings: settings, Ship: ship})
		serveTasks.set(files) // the runs of the scheduler write tasks.log (tasklog.go)
		own := files.Handler().WithAttrs([]slog.Attr{slog.String("logger", logsLogger)})
		for _, text := range append(warnings, levelWarnings...) {
			_ = own.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelWarn, text, 0)) // server.log only
		}
		setSink(c.err, files.HandlerAt(levels))
		files.Start()
		targets.Store(c.logTargets(cfg.DataDir)) // after the sink, so its first warnings reach server.log
		c.shipping.Store(targets.Load())
		stopHistory = shipHistory(dir, files)
		return files.Request
	}
	opts.Audit = func(e logs.AuditEvent) error { return files.Audit(e) } // Serve calls Logs before the first request
	return func() {
		if files == nil {
			return
		}
		stopHistory()
		if m := targets.Load(); m != nil {
			m.Close() // its stop lines reach the files before they close
		}
		setSink(c.err, nil)
		_ = files.Close() // a line that does not reach its file is counted by Files
	}
}

// logSettings reads the setting logs of state.sqlite when the file exists;
// a setting it cannot read keeps the defaults with a warning.
func logSettings(dataDir string) (logs.Settings, []string) {
	st, err := state.Open(dataDir)
	if errors.Is(err, state.ErrAbsent) {
		return logs.DefaultSettings(), nil
	}
	if err != nil {
		return logs.DefaultSettings(), []string{"the setting logs is not read; the defaults hold: " + err.Error()}
	}
	defer func() { _ = st.Close() }() // read only; nothing to keep
	value, found, err := st.Setting(logs.SettingKey)
	switch {
	case err != nil:
		return logs.DefaultSettings(), []string{"the setting logs is not read; the defaults hold: " + err.Error()}
	case !found:
		return logs.DefaultSettings(), nil
	}
	return logs.SettingsFrom(value)
}

// historyEvery is the time between two reads of the history of updates.
const historyEvery = 5 * time.Second

// executable is the path of the running binary; tests replace it.
var executable = os.Executable

// historyOf is releases/history.jsonl of the install root when exe, after
// its symlinks, is <root>/releases/<version>/pipesmith-registry; else "".
func historyOf(exe string) string {
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return ""
	}
	releases := filepath.Dir(filepath.Dir(real))
	if filepath.Base(releases) != release.ReleasesDir || filepath.Base(real) != release.InstalledBinary {
		return ""
	}
	return filepath.Join(releases, release.HistoryFile)
}

// shipHistory writes the audit and server lines of the history of updates
// of the install root that runs this binary, at once and every historyEvery
// until the function it returns is called. With the files off, or a binary
// outside an install root, it does nothing. The first failed read logs one
// warning.
func shipHistory(dir string, files *logs.Files) func() {
	exe, err := executable()
	if dir == "" || err != nil {
		return func() {}
	}
	history := historyOf(exe)
	if history == "" {
		return func() {}
	}
	shipper := &release.HistoryShipper{History: history, Cursor: filepath.Join(dir, release.HistoryCursor), Files: files}
	warned := false
	ship := func() {
		if _, err := shipper.Ship(); err != nil && !warned {
			warned = true
			slog.New(files.Handler()).Warn("the history of updates is not read: "+err.Error(), "logger", release.HistoryLogger)
		}
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ship()
		ticker := time.NewTicker(historyEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ship()
			case <-stop:
				ship()
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-stopped
	}
}
