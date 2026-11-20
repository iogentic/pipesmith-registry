package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
)

// The key upstream of config.json (decision 5.1 of the phase 5 plan), as
// TestWritesKey holds the key writes: Save writes it only while it is on, so
// a configuration with upstream off keeps the bytes of the port; Load reads
// true or false and refuses anything else.
func TestUpstreamKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cfg := config.New(dir)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	off, err := os.ReadFile(cfg.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(off), "upstream") {
		t.Fatalf("config.json with upstream off: %s", off)
	}
	cfg.Upstream = true
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	on, err := os.ReadFile(cfg.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(string(off), "\n}\n") + ",\n  \"upstream\": true\n}\n"; string(on) != want {
		t.Fatalf("config.json with upstream on:\n%s\nwant\n%s", on, want)
	}
	cfg.Writes = true
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	both, err := os.ReadFile(cfg.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSuffix(string(off), "\n}\n") + ",\n  \"writes\": true,\n  \"upstream\": true\n}\n"; string(both) != want {
		t.Fatalf("config.json with writes and upstream on:\n%s\nwant\n%s", both, want)
	}
	loaded, err := config.Load(dir, config.Env(nil))
	if err != nil || !loaded.Upstream || !loaded.Writes {
		t.Fatalf("%+v %v", loaded, err)
	}
	for text, want := range map[string]bool{`{"upstream": false}`: false, `{"upstream": true}`: true, `{}`: false, `{"writes": true}`: false} {
		if err := os.WriteFile(cfg.ConfigPath(), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := config.Load(dir, config.Env(nil))
		if err != nil || loaded.Upstream != want {
			t.Fatalf("%s: %+v %v", text, loaded, err)
		}
	}
	for _, text := range []string{`{"upstream": "yes"}`, `{"upstream": 1}`, `{"upstream": null}`} {
		if err := os.WriteFile(cfg.ConfigPath(), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := config.Load(dir, config.Env(nil)); err == nil || !strings.Contains(err.Error(), "upstream is not true or false") {
			t.Fatalf("%s: %v", text, err)
		}
	}
}

// Load takes upstream from config.json only, so that a command that saves
// the configuration never stores the value of the environment.
func TestLoadLeavesTheUpstreamVariable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cfg, err := config.Load(dir, config.Env(map[string]string{config.EnvUpstream: "on"}))
	if err != nil || cfg.Upstream {
		t.Fatalf("%+v %v", cfg, err)
	}
}

// The order of decision 5.1, as for writes: the variable, then config.json,
// then off.
func TestUpstreamFrom(t *testing.T) {
	if config.EnvUpstream != "PIPESMITH_REGISTRY_UPSTREAM" {
		t.Fatal(config.EnvUpstream)
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
		got, err := config.UpstreamFrom(config.Env(map[string]string{config.EnvUpstream: c.value}), c.file)
		if err != nil || got != c.want {
			t.Errorf("%q with file %v: %v %v, want %v", c.value, c.file, got, err, c.want)
		}
		// the variable of writes plays no part
		got, err = config.UpstreamFrom(config.Env(map[string]string{config.EnvWrites: "on"}), c.file)
		if err != nil || got != c.file {
			t.Errorf("writes variable with file %v: %v %v", c.file, got, err)
		}
	}
	for _, value := range []string{"maybe", "2", "enabled"} {
		if _, err := config.UpstreamFrom(config.Env(map[string]string{config.EnvUpstream: value}), false); err == nil ||
			!strings.Contains(err.Error(), config.EnvUpstream) {
			t.Errorf("%q: %v", value, err)
		}
	}
}
