package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
)

// The key writes of config.json (W17 of the phase 4 plan): Save writes it
// only while it is on, so a configuration with writes off keeps the bytes
// of the port; Load reads true or false and refuses anything else.
func TestWritesKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cfg := config.New(dir)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	off, err := os.ReadFile(cfg.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(off), "writes") {
		t.Fatalf("config.json with writes off: %s", off)
	}
	cfg.Writes = true
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	on, err := os.ReadFile(cfg.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(string(off), "\n}\n") + ",\n  \"writes\": true\n}\n"; string(on) != want {
		t.Fatalf("config.json with writes on:\n%s\nwant\n%s", on, want)
	}
	loaded, err := config.Load(dir, config.Env(nil))
	if err != nil || !loaded.Writes {
		t.Fatalf("%+v %v", loaded, err)
	}
	for text, want := range map[string]bool{`{"writes": false}`: false, `{"writes": true}`: true, `{}`: false} {
		if err := os.WriteFile(cfg.ConfigPath(), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := config.Load(dir, config.Env(nil))
		if err != nil || loaded.Writes != want {
			t.Fatalf("%s: %+v %v", text, loaded, err)
		}
	}
	for _, text := range []string{`{"writes": "yes"}`, `{"writes": 1}`, `{"writes": null}`} {
		if err := os.WriteFile(cfg.ConfigPath(), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := config.Load(dir, config.Env(nil)); err == nil || !strings.Contains(err.Error(), "writes is not true or false") {
			t.Fatalf("%s: %v", text, err)
		}
	}
}

// Load takes writes from config.json only, so that a command that saves
// the configuration never stores the value of the environment.
func TestLoadLeavesTheWritesVariable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cfg, err := config.Load(dir, config.Env(map[string]string{config.EnvWrites: "on"}))
	if err != nil || cfg.Writes {
		t.Fatalf("%+v %v", cfg, err)
	}
}

// The order of W17: the variable, then config.json, then off.
func TestWritesFrom(t *testing.T) {
	if config.EnvWrites != "PIPESMITH_REGISTRY_WRITES" {
		t.Fatal(config.EnvWrites)
	}
	for _, c := range []struct {
		value string
		file  bool
		want  bool
	}{
		{"", false, false}, {"", true, true},
		{"1", false, true}, {"true", false, true}, {"on", false, true}, {"yes", false, true}, {"TRUE", false, true}, {" on ", false, true},
		{"0", true, false}, {"false", true, false}, {"off", true, false}, {"no", true, false}, {"Off", true, false},
	} {
		got, err := config.WritesFrom(config.Env(map[string]string{config.EnvWrites: c.value}), c.file)
		if err != nil || got != c.want {
			t.Errorf("%q with file %v: %v %v, want %v", c.value, c.file, got, err, c.want)
		}
	}
	for _, value := range []string{"maybe", "2", "enabled"} {
		if _, err := config.WritesFrom(config.Env(map[string]string{config.EnvWrites: value}), false); err == nil ||
			!strings.Contains(err.Error(), config.EnvWrites) {
			t.Errorf("%q: %v", value, err)
		}
	}
}
