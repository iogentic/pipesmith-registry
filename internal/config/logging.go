package config

import (
	"os"
	"path/filepath"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// EnvLogDir names the log directory of serve: an absolute path, a path from
// the current directory, or off.
const EnvLogDir = "PIPESMITH_REGISTRY_LOG_DIR"

// LogDirOff, as the option, the variable or log_dir of config.json, switches
// the log files off.
const LogDirOff = "off"

// applyLogDir reads log_dir of config.json: an absolute path or off.
func applyLogDir(cfg *Config, data *pyjson.Object) error {
	value, ok := data.Get("log_dir")
	if !ok {
		return nil
	}
	dir, isString := value.(string)
	if !isString || (dir != LogDirOff && !filepath.IsAbs(dir)) {
		return valueError("%s: log_dir is not an absolute path or off", cfg.ConfigPath())
	}
	cfg.LogDir = dir
	return nil
}

// saveLogDir writes log_dir only while it is set, so that config.json of
// the default keeps its bytes.
func saveLogDir(cfg *Config, doc *pyjson.Object) {
	if cfg.LogDir != "" {
		doc.Set("log_dir", cfg.LogDir)
	}
}

// LogDirFrom is the log directory of serve: the option, else the variable,
// else log_dir of cfg, else <data directory>/logs. A relative path of the
// option or the variable is taken from the current directory; off switches
// the files off. getenv nil reads the environment of the process.
func LogDirFrom(option string, getenv func(string) string, cfg *Config) (dir string, on bool) {
	if getenv == nil {
		getenv = os.Getenv
	}
	dir, relative := option, true
	if dir == "" {
		dir = getenv(EnvLogDir)
	}
	if dir == "" {
		dir, relative = cfg.LogDir, false
	}
	switch {
	case dir == "":
		return filepath.Join(cfg.DataDir, "logs"), true
	case dir == LogDirOff:
		return "", false
	case relative:
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
	}
	return dir, true
}
