package config_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// The vectors come from tools/vectors/config.py, which runs config.load and
// config.save of the reference. Paths below a case directory are <root>.
type vectorFile struct {
	Python     string   `json:"python"`
	EnvData    string   `json:"env_data"`
	EnvPort    string   `json:"env_port"`
	LocalBinds []string `json:"local_binds"`
	Defaults   struct {
		Bind string `json:"bind"`
		Port int    `json:"port"`
	} `json:"defaults"`
	ManifestsDir string     `json:"manifests_dir"`
	Load         []loadCase `json:"load"`
	Save         []saveCase `json:"save"`
	OSErrors     []struct {
		Name   string   `json:"name"`
		Layout string   `json:"layout"`
		Error  *pyError `json:"error"`
	} `json:"os_errors"`
}

type pyError struct {
	Class   string `json:"class"`
	Message string `json:"message"`
	OSError bool   `json:"oserror"`
}

type storeDesc struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Root     string    `json:"root"`
	Repos    *[]string `json:"repos"`
	Manifest *string   `json:"manifest"`
}

type described struct {
	DataDir          string      `json:"data_dir"`
	Bind             string      `json:"bind"`
	Port             int         `json:"port"`
	Stores           []storeDesc `json:"stores"`
	DockerPorts      [][2]any    `json:"docker_ports"`
	ConfigPath       string      `json:"config_path"`
	IndexPath        string      `json:"index_path"`
	RepositoriesPath string      `json:"repositories_path"`
	ManifestsPath    string      `json:"manifests_path"`
}

type loadCase struct {
	Name    string            `json:"name"`
	Files   map[string]string `json:"files"`
	Env     map[string]string `json:"env"`
	DataDir *string           `json:"data_dir"`
	Home    *string           `json:"home"`
	Result  *described        `json:"result"`
	Error   *pyError          `json:"error"`
}

type saveCase struct {
	Name string `json:"name"`
	Spec struct {
		Bind        *string        `json:"bind"`
		Port        *int           `json:"port"`
		Sub         *string        `json:"sub"`
		Existing    *string        `json:"existing"`
		DockerPorts map[string]int `json:"docker_ports"`
		Stores      []storeDesc    `json:"stores"`
	} `json:"spec"`
	Bytes         string     `json:"bytes_b64"`
	Mode          string     `json:"mode"`
	Loaded        *described `json:"loaded"`
	Error         *pyError   `json:"error"`
	DataDirExists bool       `json:"data_dir_exists"`
	Files         []string   `json:"files"`
}

func load(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/config.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Load) < 100 || len(v.Save) < 10 {
		t.Fatalf("vector file holds %d load and %d save cases", len(v.Load), len(v.Save))
	}
	return &v
}

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func describe(cfg *config.Config, root string) described {
	hide := func(s string) string { return strings.ReplaceAll(s, root, "<root>") }
	d := described{
		DataDir: hide(cfg.DataDir), Bind: cfg.Bind, Port: cfg.Port,
		ConfigPath: hide(cfg.ConfigPath()), IndexPath: hide(cfg.IndexPath()),
		RepositoriesPath: hide(cfg.RepositoriesPath()), ManifestsPath: hide(cfg.ManifestsPath()),
	}
	for _, s := range cfg.Stores {
		var repos *[]string
		if s.Repos != nil {
			r := append([]string{}, s.Repos...)
			repos = &r
		}
		d.Stores = append(d.Stores, storeDesc{s.Name, s.Kind, s.Root, repos, s.Manifest})
	}
	return d
}

// samePorts compares the docker ports of Python, in their order, with a Go map.
func samePorts(want [][2]any, got map[string]int) bool {
	if len(want) != len(got) {
		return false
	}
	for _, pair := range want {
		port, ok := got[pair[0].(string)]
		if !ok || float64(port) != pair[1].(float64) {
			return false
		}
	}
	return true
}

func checkDescribed(t *testing.T, name string, cfg *config.Config, root string, want *described) {
	t.Helper()
	got, want2 := describe(cfg, root), *want
	want2.DockerPorts = nil
	if len(want2.Stores) == 0 {
		want2.Stores = nil
	}
	if !reflect.DeepEqual(got, want2) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want2)
		t.Errorf("%s:\n got %s\nwant %s", name, g, w)
	}
	if !samePorts(want.DockerPorts, cfg.DockerPorts) {
		t.Errorf("%s: docker ports %v, want %v", name, cfg.DockerPorts, want.DockerPorts)
	}
	if cfg.DockerPorts == nil {
		t.Errorf("%s: DockerPorts is nil; the commands write into it", name)
	}
}

func checkError(t *testing.T, name string, err error, root string, want *pyError) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error, want %s %q", name, want.Class, want.Message)
		return
	}
	if want.OSError {
		var pathErr *fs.PathError
		if !errors.As(err, &pathErr) {
			t.Errorf("%s: %T %v, want an OS error (%s)", name, err, err, want.Class)
		}
		errno := map[string]syscall.Errno{"NotADirectoryError": syscall.ENOTDIR, "IsADirectoryError": syscall.EISDIR,
			"FileNotFoundError": syscall.ENOENT}[want.Class]
		if errno != 0 && !errors.Is(err, errno) {
			t.Errorf("%s: %v, want errno %v", name, err, errno)
		}
		return
	}
	var pe *pyjson.Error
	if !errors.As(err, &pe) {
		t.Errorf("%s: %T %v, want %s %q", name, err, err, want.Class, want.Message)
		return
	}
	if got := strings.ReplaceAll(pe.Message, root, "<root>"); pe.Class != want.Class || got != productReason(want.Message) {
		t.Errorf("%s:\n got %s %q\nwant %s %q", name, pe.Class, got, want.Class, productReason(want.Message))
	}
}

// productReason is a message of the reference with the reason this product
// gives for a bind address that is not local: the reference names a phase of
// its plan, the product says how a network address is given.
func productReason(message string) string {
	return strings.Replace(message, " are allowed: no authentication in phase 1",
		" are allowed in config.json; a network address is given with serve --bind once users are imported", 1)
}

func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestConstants(t *testing.T) {
	v := load(t)
	if config.LegacyEnvData != v.EnvData || config.LegacyEnvPort != v.EnvPort {
		t.Errorf("legacy names %s %s, want %s %s", config.LegacyEnvData, config.LegacyEnvPort, v.EnvData, v.EnvPort)
	}
	if config.EnvData != "PIPESMITH_REGISTRY_DATA" || config.EnvPort != "PIPESMITH_REGISTRY_PORT" {
		t.Errorf("new names %s %s", config.EnvData, config.EnvPort)
	}
	if !reflect.DeepEqual(config.LocalBinds(), v.LocalBinds) {
		t.Errorf("LocalBinds %v, want %v", config.LocalBinds(), v.LocalBinds)
	}
	if config.DefaultBind != v.Defaults.Bind || config.DefaultPort != v.Defaults.Port || config.ManifestsDir != v.ManifestsDir {
		t.Errorf("defaults %s %d %s", config.DefaultBind, config.DefaultPort, config.ManifestsDir)
	}
}

func TestLoadVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Load {
		root := t.TempDir()
		fill := func(s string) string { return strings.ReplaceAll(s, "<root>", root) }
		env := map[string]string{}
		for key, value := range c.Env {
			env[key] = fill(value)
		}
		dataDir := ""
		if c.DataDir != nil {
			dataDir = fill(*c.DataDir)
		}
		dataRoot := dataDir
		if dataRoot == "" {
			dataRoot = env[v.EnvData]
		}
		for name, content := range c.Files {
			if err := os.MkdirAll(dataRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dataRoot, name), unb64(t, content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if c.Home != nil {
			t.Setenv("HOME", fill(*c.Home))
		}
		before := tree(t, root)
		cfg, err := config.Load(dataDir, config.Env(env))
		if after := tree(t, root); !reflect.DeepEqual(before, after) {
			t.Errorf("%s: Load wrote files: %v", c.Name, after)
		}
		if c.Error != nil {
			checkError(t, c.Name, err, root, c.Error)
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		checkDescribed(t, c.Name, cfg, root, c.Result)
	}
}

func TestLoadOSErrors(t *testing.T) {
	v := load(t)
	if len(v.OSErrors) != 2 {
		t.Fatalf("%d OS error cases", len(v.OSErrors))
	}
	for _, c := range v.OSErrors {
		root := t.TempDir()
		data := filepath.Join(root, "data")
		switch c.Layout {
		case "file":
			if err := os.WriteFile(data, []byte("not a directory"), 0o644); err != nil {
				t.Fatal(err)
			}
		case "dir":
			if err := os.MkdirAll(filepath.Join(data, "config.json"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		_, err := config.Load(data, config.Env(nil))
		checkError(t, c.Name, err, root, c.Error)
	}
}

func TestSaveVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Save {
		root := t.TempDir()
		sub := "data"
		if c.Spec.Sub != nil {
			sub = *c.Spec.Sub
		}
		dataDir := filepath.Join(root, sub)
		if c.Spec.Existing != nil {
			if err := os.MkdirAll(dataDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(*c.Spec.Existing), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cfg := config.New(dataDir)
		if c.Spec.Bind != nil {
			cfg.Bind = *c.Spec.Bind
		}
		if c.Spec.Port != nil {
			cfg.Port = *c.Spec.Port
		}
		for _, s := range c.Spec.Stores {
			var repos []string
			if s.Repos != nil {
				repos = append([]string{}, *s.Repos...)
			}
			cfg.Stores = append(cfg.Stores, config.StoreConfig{Name: s.Name, Kind: s.Kind, Root: s.Root, Repos: repos,
				Manifest: s.Manifest})
		}
		for name, port := range c.Spec.DockerPorts {
			cfg.DockerPorts[name] = port
		}
		err := config.Save(cfg)
		if c.Error != nil {
			checkError(t, c.Name, err, root, c.Error)
		} else if err != nil {
			t.Errorf("%s: %v", c.Name, err)
		} else {
			got, err := os.ReadFile(cfg.ConfigPath())
			if want := unb64(t, c.Bytes); err != nil || string(got) != string(want) {
				t.Errorf("%s:\n got %q %v\nwant %q", c.Name, got, err, want)
			}
			info, err := os.Stat(cfg.ConfigPath())
			if err != nil || fmt.Sprintf("0o%o", info.Mode().Perm()) != c.Mode {
				t.Errorf("%s: mode %v %v, want %s", c.Name, info.Mode(), err, c.Mode)
			}
			loaded, err := config.Load(dataDir, config.Env(nil))
			if err != nil {
				t.Errorf("%s: load after save: %v", c.Name, err)
			} else {
				checkDescribed(t, c.Name+" (loaded)", loaded, root, c.Loaded)
			}
		}
		info, statErr := os.Stat(dataDir)
		if exists := statErr == nil && info.IsDir(); exists != c.DataDirExists {
			t.Errorf("%s: data dir exists %v, want %v", c.Name, exists, c.DataDirExists)
		}
		var files []string
		if entries, err := os.ReadDir(dataDir); err == nil {
			for _, e := range entries {
				files = append(files, e.Name())
			}
		}
		sort.Strings(files)
		if len(files) == 0 {
			files = nil
		}
		want := c.Files
		if len(want) == 0 {
			want = nil
		}
		if !reflect.DeepEqual(files, want) {
			t.Errorf("%s: files %v, want %v", c.Name, files, want)
		}
	}
}

func TestNewNamesComeFirst(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name     string
		env      map[string]string
		wantDir  string
		wantPort int
	}{
		{"new beats legacy", map[string]string{
			config.EnvData: root + "/new", config.LegacyEnvData: root + "/old",
			config.EnvPort: "14111", config.LegacyEnvPort: "14112",
		}, root + "/new", 14111},
		{"legacy alone", map[string]string{config.LegacyEnvData: root + "/old", config.LegacyEnvPort: "14112"},
			root + "/old", 14112},
		{"new alone", map[string]string{config.EnvData: root + "/new", config.EnvPort: "14111"}, root + "/new", 14111},
		{"empty new falls back", map[string]string{
			config.EnvData: "", config.LegacyEnvData: root + "/old", config.EnvPort: "", config.LegacyEnvPort: "14112",
		}, root + "/old", 14112},
	}
	for _, c := range cases {
		cfg, err := config.Load("", config.Env(c.env))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if cfg.DataDir != c.wantDir || cfg.Port != c.wantPort {
			t.Errorf("%s: %s %d, want %s %d", c.name, cfg.DataDir, cfg.Port, c.wantDir, c.wantPort)
		}
	}
	// the message names the variable that was read
	_, err := config.Load(root, config.Env(map[string]string{config.EnvPort: "abc", config.LegacyEnvPort: "14112"}))
	if err == nil || err.Error() != "PIPESMITH_REGISTRY_PORT is not a port number: 'abc'" {
		t.Errorf("bad new port: %v", err)
	}
	_, err = config.Load(root, config.Env(map[string]string{config.LegacyEnvPort: "0"}))
	if err == nil || err.Error() != "ARTIFACT_REPOSITORY_PORT is outside 1 to 65535: 0" {
		t.Errorf("bad legacy port: %v", err)
	}
}

func TestNilEnvReadsTheProcess(t *testing.T) {
	root := t.TempDir()
	t.Setenv(config.EnvData, root+"/from-process")
	t.Setenv(config.EnvPort, "14123")
	t.Setenv(config.LegacyEnvData, "")
	t.Setenv(config.LegacyEnvPort, "")
	cfg, err := config.Load("", nil)
	if err != nil || cfg.DataDir != root+"/from-process" || cfg.Port != 14123 {
		t.Errorf("Load with nil env: %+v %v", cfg, err)
	}
}

func TestDefaultDataDir(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "install", "bin", "pipesmith-registry")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link-to-binary")
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(root, "install"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{binary, link} {
		got, err := config.DefaultDataDirOf(path)
		if err != nil || got != filepath.Join(real, "data") {
			t.Errorf("DefaultDataDirOf(%s) = %s %v, want %s", path, got, err, filepath.Join(real, "data"))
		}
	}
	if _, err := config.DefaultDataDirOf(filepath.Join(root, "missing")); err == nil {
		t.Error("DefaultDataDirOf of a missing binary gave no error")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := config.DefaultDataDirOf(exe)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load("", config.Env(nil))
	if err != nil || cfg.DataDir != want {
		t.Errorf("Load without a data dir: %v %v, want %s", cfg, err, want)
	}
}

func TestNewAndPaths(t *testing.T) {
	cfg := config.New("demo/./data/")
	if cfg.DataDir != "demo/data" || cfg.Bind != "127.0.0.1" || cfg.Port != 8793 || cfg.Stores != nil || cfg.DockerPorts == nil {
		t.Errorf("New: %+v", cfg)
	}
	if cfg.ConfigPath() != "demo/data/config.json" || cfg.IndexPath() != "demo/data/index.sqlite" ||
		cfg.RepositoriesPath() != "demo/data/repositories.json" || cfg.ManifestsPath() != "demo/data/manifests" {
		t.Errorf("paths: %s %s %s %s", cfg.ConfigPath(), cfg.IndexPath(), cfg.RepositoriesPath(), cfg.ManifestsPath())
	}
	if here := config.New("."); here.ConfigPath() != "config.json" {
		t.Errorf("ConfigPath of . = %s", here.ConfigPath())
	}
}

func TestCheckBind(t *testing.T) {
	for _, bind := range []string{"127.0.0.1", "localhost", "::1"} {
		if err := config.CheckBind(bind); err != nil {
			t.Errorf("CheckBind(%s): %v", bind, err)
		}
	}
	err := config.CheckBind("0.0.0.0")
	want := "bind address '0.0.0.0' is not local; only 127.0.0.1, localhost, ::1 are allowed in config.json; a network address is given with serve --bind once users are imported"
	if err == nil || err.Error() != want || !pyjson.IsValueError(err) {
		t.Errorf("CheckBind(0.0.0.0) = %v", err)
	}
}

func TestSaveLeavesNoTemporaryFile(t *testing.T) {
	root := t.TempDir()
	cfg := config.New(root)
	// config.json as a directory: the rename fails and the temporary file goes
	if err := os.Mkdir(cfg.ConfigPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(cfg); err == nil {
		t.Fatal("Save over a directory gave no error")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Errorf("left behind: %v", entries)
	}
	// the positive twin: a normal save leaves exactly config.json
	other := config.New(filepath.Join(root, "other"))
	if err := config.Save(other); err != nil {
		t.Fatal(err)
	}
	entries, _ = os.ReadDir(other.DataDir)
	if len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Errorf("after save: %v", entries)
	}
}

func TestSaveReplacesAtomically(t *testing.T) {
	root := t.TempDir()
	cfg := config.New(root)
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(cfg.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = 14100
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(cfg.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("Save wrote into the old file instead of replacing it")
	}
}
