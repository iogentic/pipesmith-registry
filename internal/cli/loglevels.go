package cli

import (
	"errors"

	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/server"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// logLevels are the levels of the loggers of serve (L10): read from the
// setting log_levels of state.sqlite, saved there after each change of the
// log routes, and handed with the log directory to the server. dir is
// empty while the files are off. The warnings of a setting it cannot read
// go to server.log with those of the setting logs.
func logLevels(dataDir, dir string, opts *server.ServeOptions) (*logs.Levels, []string) {
	levels := logs.NewLevels(func(set map[string]string) error { return saveLevels(dataDir, set) })
	opts.Logging = &server.LogAccess{Dir: dir, Levels: levels}
	st, err := state.Open(dataDir)
	if errors.Is(err, state.ErrAbsent) {
		return levels, nil
	}
	if err != nil {
		return levels, []string{"the setting log_levels is not read; every logger is at INFO: " + err.Error()}
	}
	defer func() { _ = st.Close() }() // read only; nothing to keep
	value, found, err := st.Setting(logs.LevelsKey)
	switch {
	case err != nil:
		return levels, []string{"the setting log_levels is not read; every logger is at INFO: " + err.Error()}
	case !found:
		return levels, nil
	}
	return levels, levels.Load(value)
}

// saveLevels writes the setting log_levels.
func saveLevels(dataDir string, set map[string]string) error {
	st, err := state.Open(dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }() // the write is committed by PutSetting
	return st.PutSetting(logs.LevelsKey, set)
}
