package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
)

func writeConfigAt(t *testing.T, dataDir, text string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// log_dir is absent from config.json while it has its default, written and
// read back when it is set.
func TestLogDirInConfigFile(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := config.New(dataDir)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	data := string(mustReadFile(t, cfg.ConfigPath()))
	if strings.Contains(data, "log_dir") || data != "{\n  \"bind\": \"127.0.0.1\",\n  \"port\": 8793,\n  \"stores\": [],\n  \"docker_ports\": {}\n}\n" {
		t.Fatalf("default bytes %q", data)
	}
	cfg.LogDir = "/srv/demo-logs"
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if data := string(mustReadFile(t, cfg.ConfigPath())); !strings.HasSuffix(data, ",\n  \"log_dir\": \"/srv/demo-logs\"\n}\n") {
		t.Fatalf("set bytes %q", data)
	}
	loaded, err := config.Load(dataDir, config.Env(nil))
	if err != nil || loaded.LogDir != "/srv/demo-logs" {
		t.Fatalf("read back %v %v", loaded, err)
	}
	writeConfigAt(t, dataDir, `{"log_dir": "off"}`)
	if loaded, err := config.Load(dataDir, config.Env(nil)); err != nil || loaded.LogDir != "off" {
		t.Fatalf("off %v %v", loaded, err)
	}
	for _, value := range []string{`"logs"`, `"./logs"`, `""`, `"Off"`, `7`, `null`, `["/srv"]`} {
		writeConfigAt(t, dataDir, `{"log_dir": `+value+`}`)
		_, err := config.Load(dataDir, config.Env(nil))
		if err == nil || err.Error() != filepath.Join(dataDir, "config.json")+": log_dir is not an absolute path or off" {
			t.Errorf("log_dir %s: %v", value, err)
		}
	}
}

// Load leaves the variable to serve, so a command that saves config.json
// never stores it.
func TestLoadLeavesTheLogDirVariable(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg, err := config.Load(dataDir, config.Env(map[string]string{config.EnvLogDir: "/srv/demo-logs"}))
	if err != nil || cfg.LogDir != "" {
		t.Fatalf("%v %v", cfg, err)
	}
}

// The order: the option, the variable, config.json, <data directory>/logs.
// A relative path of the option or the variable is taken from the current
// directory, and off switches the files off.
func TestLogDirFrom(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	cwd, _ = os.Getwd()
	cfg := config.New("/srv/demo-data")
	env := func(value string) func(string) string {
		return config.Env(map[string]string{config.EnvLogDir: value})
	}
	fromFile := config.New("/srv/demo-data")
	fromFile.LogDir = "/srv/file-logs"
	offFile := config.New("/srv/demo-data")
	offFile.LogDir = "off"
	for _, c := range []struct {
		name   string
		option string
		getenv func(string) string
		cfg    *config.Config
		dir    string
		on     bool
	}{
		{"default", "", env(""), cfg, "/srv/demo-data/logs", true},
		{"config file", "", env(""), fromFile, "/srv/file-logs", true},
		{"variable beats the file", "", env("/srv/env-logs"), fromFile, "/srv/env-logs", true},
		{"option beats the variable", "/srv/option-logs", env("/srv/env-logs"), fromFile, "/srv/option-logs", true},
		{"relative option", "demo/logs", env("/srv/env-logs"), cfg, filepath.Join(cwd, "demo/logs"), true},
		{"relative variable", "", env("env/logs"), cfg, filepath.Join(cwd, "env/logs"), true},
		{"option off", "off", env("/srv/env-logs"), fromFile, "", false},
		{"variable off", "", env("off"), fromFile, "", false},
		{"file off", "", env(""), offFile, "", false},
		{"option beats a file off", "/srv/option-logs", env(""), offFile, "/srv/option-logs", true},
	} {
		dir, on := config.LogDirFrom(c.option, c.getenv, c.cfg)
		if dir != c.dir || on != c.on {
			t.Errorf("%s: %q %v, want %q %v", c.name, dir, on, c.dir, c.on)
		}
	}
	if config.EnvLogDir != "PIPESMITH_REGISTRY_LOG_DIR" {
		t.Errorf("variable %s", config.EnvLogDir)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
