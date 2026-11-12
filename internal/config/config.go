package config

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"math/big"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/pystr"
)

// The environment: the names of this product first, then those of the
// Python server, so that one data directory serves both.
const (
	EnvData       = "PIPESMITH_REGISTRY_DATA"
	EnvPort       = "PIPESMITH_REGISTRY_PORT"
	LegacyEnvData = "ARTIFACT_REPOSITORY_DATA"
	LegacyEnvPort = "ARTIFACT_REPOSITORY_PORT"
)

const (
	DefaultBind  = "127.0.0.1"
	DefaultPort  = 8793
	ManifestsDir = "manifests"
)

var localBinds = []string{"127.0.0.1", "localhost", "::1"}

// LocalBinds returns the bind addresses a server may use: no authentication
// in phase 1.
func LocalBinds() []string {
	return slices.Clone(localBinds)
}

// New returns the defaults for a data directory.
func New(dataDir string) *Config {
	return &Config{DataDir: pystr.Path(dataDir), Bind: DefaultBind, Port: DefaultPort, DockerPorts: map[string]int{}}
}

// ConfigPath, IndexPath and RepositoriesPath are the files of the data
// directory, joined as pathlib joins them.
func (c *Config) ConfigPath() string       { return pystr.Join(c.DataDir, "config.json") }
func (c *Config) IndexPath() string        { return pystr.Join(c.DataDir, "index.sqlite") }
func (c *Config) RepositoriesPath() string { return pystr.Join(c.DataDir, "repositories.json") }

// ManifestsPath holds the lookup tables of the blob manifests of directory stores.
func (c *Config) ManifestsPath() string { return pystr.Join(c.DataDir, ManifestsDir) }

// Env turns a map into the getenv of Load; a nil map is an empty environment.
func Env(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func valueError(format string, args ...any) error {
	return &pyjson.Error{Class: "ValueError", Message: fmt.Sprintf(format, args...)}
}

// firstSet returns the first of the variables that is set and not empty.
func firstSet(getenv func(string) string, names ...string) (string, string) {
	for _, name := range names {
		if value := getenv(name); value != "" {
			return name, value
		}
	}
	return "", ""
}

// Load reads the configuration of a data directory and creates nothing.
// dataDir "" means none given: then EnvData, LegacyEnvData or
// DefaultDataDir. getenv nil reads the environment of the process.
//
// Errors keep the kind of the Python exception: *pyjson.Error with class
// ValueError for a bad value, OverflowError for an infinite port,
// UnicodeDecodeError or RecursionError from reading the file, RuntimeError
// when "~" cannot be expanded; an OS error as it comes from package os.
func Load(dataDir string, getenv func(string) string) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if dataDir == "" {
		if _, value := firstSet(getenv, EnvData, LegacyEnvData); value != "" {
			expanded, err := expandUser(pystr.Path(value))
			if err != nil {
				return nil, err
			}
			dataDir = expanded
		} else {
			dir, err := DefaultDataDir()
			if err != nil {
				return nil, err
			}
			dataDir = dir
		}
	}
	cfg := New(dataDir)
	data, err := os.ReadFile(cfg.ConfigPath())
	switch {
	case err == nil:
		value, err := parse(data, cfg.ConfigPath())
		if err != nil {
			return nil, err
		}
		if err := apply(cfg, value); err != nil {
			return nil, err
		}
	case !errors.Is(err, fs.ErrNotExist): // FileNotFoundError: ENOENT only
		return nil, err
	}
	if name, value := firstSet(getenv, EnvPort, LegacyEnvPort); value != "" {
		port, err := checkPort(value, name)
		if err != nil {
			return nil, err
		}
		cfg.Port = port
	}
	if err := CheckBind(cfg.Bind); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save writes config.json atomically through a temporary file in the same
// directory. The data directory itself is not stored.
func Save(cfg *Config) error {
	if err := CheckBind(cfg.Bind); err != nil {
		return err
	}
	if _, err := checkPort(cfg.Port, "port"); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return err
	}
	stores := make([]any, 0, len(cfg.Stores))
	for _, s := range cfg.Stores {
		var repos any
		if s.Repos != nil {
			list := make([]any, len(s.Repos))
			for i, name := range s.Repos {
				list[i] = name
			}
			repos = list
		}
		var manifest any
		if s.Manifest != nil {
			manifest = *s.Manifest
		}
		stores = append(stores, pyjson.NewObject().Set("name", s.Name).Set("kind", s.Kind).Set("root", s.Root).
			Set("repos", repos).Set("manifest", manifest))
	}
	names := make([]string, 0, len(cfg.DockerPorts))
	for name := range cfg.DockerPorts {
		names = append(names, name)
	}
	slices.SortFunc(names, pystr.Compare)
	ports := pyjson.NewObject()
	for _, name := range names {
		port, err := checkPort(cfg.DockerPorts[name], "Docker port of "+name)
		if err != nil {
			return err
		}
		ports.Set(name, port)
	}
	doc := pyjson.NewObject().Set("bind", cfg.Bind).Set("port", cfg.Port).Set("stores", stores).Set("docker_ports", ports)
	text, err := pyjson.DumpsIndent(doc, 2)
	if err != nil {
		return err
	}
	return WriteAtomic(cfg.ConfigPath(), ".config-", append(text, '\n'))
}

// WriteAtomic writes data to a temporary file "<prefix>*.json" in the
// directory of target and renames it over target, as tempfile.mkstemp and
// os.replace do: the new file has mode 0600, and the temporary file goes
// when anything fails.
func WriteAtomic(target, prefix string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(target), prefix+"*.json")
	if err != nil {
		return err
	}
	_, err = temp.Write(data)
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temp.Name(), target)
	}
	if err != nil {
		_ = os.Remove(temp.Name()) // cleanup; the write error is the one returned
	}
	return err
}

// CheckBind refuses an address that is not local.
func CheckBind(bind string) error {
	if !slices.Contains(localBinds, bind) {
		return valueError("bind address %s is not local; only %s are allowed: no authentication in phase 1",
			pystr.Repr(bind), strings.Join(localBinds, ", "))
	}
	return nil
}

// DefaultDataDir is "data" in the parent of the directory that holds the
// running binary: <root>/bin/pipesmith-registry gives <root>/data.
func DefaultDataDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return DefaultDataDirOf(exe)
}

// DefaultDataDirOf is DefaultDataDir for the binary at executable. Links are
// resolved first, as the Python server resolves the path of its package.
func DefaultDataDirOf(executable string) (string, error) {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(filepath.Dir(abs)), "data"), nil
}

// expandUser is Path.expanduser of a path in its pathlib form. "~" takes
// HOME of the process, as os.path.expanduser does, not the getenv of Load.
func expandUser(path string) (string, error) {
	if strings.HasPrefix(path, "/") || !strings.HasPrefix(path, "~") {
		return path, nil
	}
	first, rest, _ := strings.Cut(path, "/")
	var home string
	if first == "~" {
		if value, ok := os.LookupEnv("HOME"); ok {
			home = value
		} else if u, err := user.Current(); err == nil {
			home = u.HomeDir
		} else {
			return "", noHome()
		}
	} else {
		u, err := user.Lookup(first[1:])
		if err != nil {
			return "", noHome()
		}
		home = u.HomeDir
	}
	if home = strings.TrimRight(home, "/"); home == "" {
		home = "/"
	}
	if strings.HasPrefix(home, "~") {
		return "", noHome()
	}
	if rest == "" {
		return pystr.Path(home), nil
	}
	return pystr.Join(home, rest), nil
}

func noHome() error {
	return &pyjson.Error{Class: "RuntimeError", Message: "Could not determine home directory."}
}

func parse(data []byte, path string) (*pyjson.Object, error) {
	value, err := pyjson.DecodeText(data)
	if err != nil {
		var syntax *pyjson.SyntaxError
		if errors.As(err, &syntax) {
			return nil, valueError("%s is not valid JSON: %s", path, syntax)
		}
		return nil, err
	}
	obj, ok := value.(*pyjson.Object)
	if !ok {
		return nil, valueError("%s does not hold an object", path)
	}
	return obj, nil
}

// truthy is bool() of a decoded JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case int64:
		return x != 0
	case *big.Int:
		return x.Sign() != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case *pyjson.Object:
		return x.Len() > 0
	}
	return true
}

func get(o *pyjson.Object, key string) any {
	v, _ := o.Get(key)
	return v
}

func nonEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && s != ""
}

func apply(cfg *Config, data *pyjson.Object) error {
	path := cfg.ConfigPath()
	if bind, ok := data.Get("bind"); ok {
		s, isString := bind.(string)
		if !isString {
			return valueError("%s: bind is not a string", path)
		}
		cfg.Bind = s
	}
	if port, ok := data.Get("port"); ok {
		p, err := checkPort(port, path+": port")
		if err != nil {
			return err
		}
		cfg.Port = p
	}
	var stores []any
	if value := get(data, "stores"); truthy(value) {
		list, ok := value.([]any)
		if !ok {
			return valueError("%s: stores is not a list", path)
		}
		stores = list
	}
	for position, item := range stores {
		obj, ok := item.(*pyjson.Object)
		if !ok || !nonEmptyString(get(obj, "name")) || !nonEmptyString(get(obj, "kind")) || !nonEmptyString(get(obj, "root")) {
			return valueError("%s: store %d needs name, kind and root", path, position)
		}
		name := get(obj, "name").(string)
		var repos []string
		if value := get(obj, "repos"); value != nil {
			list, ok := value.([]any)
			if !ok {
				return valueError("%s: repos of store %s is not a list of names", path, pystr.Repr(name))
			}
			repos = make([]string, 0, len(list))
			for _, r := range list {
				s, ok := r.(string)
				if !ok {
					return valueError("%s: repos of store %s is not a list of names", path, pystr.Repr(name))
				}
				repos = append(repos, s)
			}
		}
		var manifest *string
		if value := get(obj, "manifest"); value != nil {
			if !nonEmptyString(value) {
				return valueError("%s: manifest of store %s is not a path", path, pystr.Repr(name))
			}
			s := value.(string)
			manifest = &s
		}
		cfg.Stores = append(cfg.Stores, StoreConfig{Name: name, Kind: get(obj, "kind").(string),
			Root: get(obj, "root").(string), Repos: repos, Manifest: manifest})
	}
	ports := pyjson.NewObject()
	if value := get(data, "docker_ports"); truthy(value) {
		obj, ok := value.(*pyjson.Object)
		if !ok {
			return valueError("%s: docker_ports is not an object", path)
		}
		ports = obj
	}
	for name, value := range ports.All() {
		if name == "" {
			return valueError("%s: docker_ports has an empty repository name", path)
		}
		port, err := checkPort(value, path+": Docker port of "+name)
		if err != nil {
			return err
		}
		cfg.DockerPorts[name] = port
	}
	return nil
}

// checkPort is _port of the Python module: int(value) in 1 to 65535. A
// value int() refuses is "not a port number"; int() of an infinite float is
// the OverflowError that Python does not catch there.
func checkPort(value any, what string) (int, error) {
	var n *big.Int
	switch x := value.(type) {
	case bool:
		return 0, valueError("%s is not a port number: %s", what, pystr.Repr(x))
	case int:
		n = big.NewInt(int64(x))
	case int64:
		n = big.NewInt(x)
	case *big.Int:
		n = x
	case float64:
		switch {
		case math.IsNaN(x):
			return 0, valueError("%s is not a port number: %s", what, pystr.Repr(x))
		case math.IsInf(x, 0):
			return 0, &pyjson.Error{Class: "OverflowError", Message: "cannot convert float infinity to integer"}
		}
		n, _ = new(big.Float).SetFloat64(math.Trunc(x)).Int(nil)
	case string:
		parsed, err := pystr.ParseInt(x)
		if err != nil {
			return 0, valueError("%s is not a port number: %s", what, pystr.Repr(x))
		}
		n = parsed
	default:
		return 0, valueError("%s is not a port number: %s", what, pystr.Repr(value))
	}
	if n.Cmp(big.NewInt(1)) < 0 || n.Cmp(big.NewInt(65535)) > 0 {
		return 0, valueError("%s is outside 1 to 65535: %s", what, n)
	}
	return int(n.Int64()), nil
}
