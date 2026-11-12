// Package repos holds the repository definitions, the import of a
// repository settings export of the old product and the resolution of
// groups.
package repos

import (
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"slices"

	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/pystr"
)

// Repo is one repository. Members is nil when it has none.
type Repo struct {
	Name, Format, Type string // Type: "hosted", "proxy" or "group"
	Members            []string
	Defined            bool
	VersionPolicy      *string
	LayoutPolicy       *string
	WritePolicy        *string
	RemoteURL          *string
	HTTPPort           *int // docker.httpPort of the settings import
}

// Access maps a format to how the read path serves it.
var Access = map[string]string{"maven2": "maven", "docker": "docker", "npm": "npm"}

// Fields are the keys of one entry of repositories.json, in their order.
var Fields = []string{"name", "format", "type", "members", "version_policy", "layout_policy", "write_policy",
	"remote_url", "http_port"}

// AccessOf is "maven", "docker", "npm", or "files" for any other format.
func AccessOf(r Repo) string {
	if access, ok := Access[r.Format]; ok {
		return access
	}
	return "files"
}

// Repositories are the defined repositories plus the names found in the
// stores without a definition.
type Repositories struct {
	byName map[string]Repo
	sorted []Repo
}

// New takes the first definition of a name. A discovered name without a
// definition takes its hint as format, and stays "unknown" without one.
func New(defined []Repo, discovered []string, hints map[string]string) *Repositories {
	r := &Repositories{byName: map[string]Repo{}}
	for _, repo := range defined {
		if _, ok := r.byName[repo.Name]; !ok {
			r.byName[repo.Name] = repo
		}
	}
	for _, name := range discovered {
		if _, ok := r.byName[name]; name != "" && !ok {
			format := hints[name]
			if format == "" {
				format = "unknown"
			}
			r.byName[name] = Repo{Name: name, Format: format, Type: "hosted"}
		}
	}
	keys := map[string]string{}
	for name, repo := range r.byName {
		keys[name] = pystr.Casefold(name)
		r.sorted = append(r.sorted, repo)
	}
	// sorted(key=(name.casefold(), name)) in code point order
	slices.SortFunc(r.sorted, func(a, b Repo) int {
		if c := pystr.Compare(keys[a.Name], keys[b.Name]); c != 0 {
			return c
		}
		return pystr.Compare(a.Name, b.Name)
	})
	return r
}

// Get returns the repository of a name.
func (r *Repositories) Get(name string) (Repo, bool) {
	repo, ok := r.byName[name]
	return repo, ok
}

// All returns every repository, sorted by name without regard to case.
func (r *Repositories) All() []Repo {
	return slices.Clone(r.sorted)
}

// Resolve returns for a group its leaf members in order, depth first, each
// once; for any other repository the repository itself; nil for an unknown
// name.
func (r *Repositories) Resolve(name string) []Repo {
	repo, ok := r.byName[name]
	if !ok {
		return nil
	}
	if repo.Type != "group" {
		return []Repo{repo}
	}
	var leaves []Repo
	seenLeaves := map[string]bool{}
	visitedGroups := map[string]bool{repo.Name: true}
	stack := reversed(repo.Members)
	for len(stack) > 0 {
		memberName := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		member, ok := r.byName[memberName]
		switch {
		case !ok:
		case member.Type == "group":
			if !visitedGroups[member.Name] {
				visitedGroups[member.Name] = true
				stack = append(stack, reversed(member.Members)...)
			}
		case !seenLeaves[member.Name]:
			seenLeaves[member.Name] = true
			leaves = append(leaves, member)
		}
	}
	return leaves
}

func reversed(names []string) []string {
	out := slices.Clone(names)
	slices.Reverse(out)
	return out
}

// Stat is the index count of one repository.
type Stat struct {
	Repo         string
	Files, Bytes int64
	NewestMS     int64
}

// Row is one repository as `repos --json` and /api/repositories show it.
type Row struct {
	Name, Format, Type string
	Defined            bool
	Access             string
	DockerPort         *int
	Members            []string
	Files, Bytes       int64
	NewestMS           *int64
	VersionPolicy      *string
	LayoutPolicy       *string
	WritePolicy        *string
	RemoteURL          *string
}

// RepositoryRows gives one row per repository with its own index counts; a
// group counts only its own rows. dockerPorts holds the ports of
// config.json, which win over HTTPPort.
func RepositoryRows(r *Repositories, stats []Stat, dockerPorts map[string]int) []Row {
	byRepo := map[string]Stat{}
	for _, stat := range stats {
		byRepo[stat.Repo] = stat
	}
	rows := make([]Row, 0, len(r.sorted))
	for _, repo := range r.sorted {
		row := Row{Name: repo.Name, Format: repo.Format, Type: repo.Type, Defined: repo.Defined, Access: AccessOf(repo),
			Members: slices.Clone(repo.Members), VersionPolicy: repo.VersionPolicy, LayoutPolicy: repo.LayoutPolicy,
			WritePolicy: repo.WritePolicy, RemoteURL: repo.RemoteURL}
		if row.Access == "docker" {
			if port, ok := dockerPorts[repo.Name]; ok {
				row.DockerPort = &port
			} else if repo.HTTPPort != nil {
				port := *repo.HTTPPort
				row.DockerPort = &port
			}
		}
		if stat, ok := byRepo[repo.Name]; ok {
			row.Files, row.Bytes = stat.Files, stat.Bytes
			newest := stat.NewestMS
			row.NewestMS = &newest
		}
		rows = append(rows, row)
	}
	return rows
}

func optional[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// Object is the row as a JSON object with the keys in the order of Python.
func (row Row) Object() *pyjson.Object {
	members := make([]any, len(row.Members))
	for i, m := range row.Members {
		members[i] = m
	}
	return pyjson.NewObject().
		Set("name", row.Name).
		Set("format", row.Format).
		Set("type", row.Type).
		Set("defined", row.Defined).
		Set("access", row.Access).
		Set("docker_port", optional(row.DockerPort)).
		Set("members", members).
		Set("files", row.Files).
		Set("bytes", row.Bytes).
		Set("newest_ms", optional(row.NewestMS)).
		Set("version_policy", optional(row.VersionPolicy)).
		Set("layout_policy", optional(row.LayoutPolicy)).
		Set("write_policy", optional(row.WritePolicy)).
		Set("remote_url", optional(row.RemoteURL))
}

// RowsDocument is {"repositories": rows}, the document of `repos --json`.
func RowsDocument(rows []Row) *pyjson.Object {
	list := make([]any, len(rows))
	for i, row := range rows {
		list[i] = row.Object()
	}
	return pyjson.NewObject().Set("repositories", list)
}

func valueError(format string, args ...any) error {
	return &pyjson.Error{Class: "ValueError", Message: fmt.Sprintf(format, args...)}
}

func get(o *pyjson.Object, key string) any {
	v, _ := o.Get(key)
	return v
}

// text is a string that is not empty, else nil.
func text(v any) *string {
	if s, ok := v.(string); ok && s != "" {
		return &s
	}
	return nil
}

func orDefault(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
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

// port is a JSON integer in 1 to 65535; bool and float are not.
func port(v any) (int, bool) {
	n, ok := v.(int64)
	if !ok || n < 1 || n > 65535 {
		return 0, false
	}
	return int(n), true
}

// LoadDefinitions reads repositories.json; a file that does not exist gives
// nil. Errors keep the kind of the Python exception: *pyjson.Error with
// class ValueError, UnicodeDecodeError or RecursionError, or an OS error.
func LoadDefinitions(path string) ([]Repo, error) {
	path = pystr.Path(path)
	data, err := os.ReadFile(path) // #nosec G304 -- repositories.json of the data directory the operator configured.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	value, err := pyjson.DecodeText(data)
	if err != nil {
		var syntax *pyjson.SyntaxError
		if errors.As(err, &syntax) {
			return nil, valueError("%s is not valid JSON: %s", path, syntax)
		}
		return nil, err
	}
	var items []any
	if obj, ok := value.(*pyjson.Object); ok {
		items, ok = get(obj, "repositories").([]any)
		if !ok {
			return nil, valueError("%s has no list 'repositories'", path)
		}
	} else {
		return nil, valueError("%s has no list 'repositories'", path)
	}
	var out []Repo
	for position, item := range items {
		obj, ok := item.(*pyjson.Object)
		if !ok || text(get(obj, "name")) == nil {
			return nil, valueError("%s: entry %d has no name", path, position)
		}
		name := get(obj, "name").(string)
		var members []string
		if value := get(obj, "members"); truthy(value) {
			list, ok := value.([]any)
			if !ok {
				return nil, valueError("%s: members of %s are not a list of names", path, pystr.Repr(name))
			}
			for _, m := range list {
				s, ok := m.(string)
				if !ok {
					return nil, valueError("%s: members of %s are not a list of names", path, pystr.Repr(name))
				}
				members = append(members, s)
			}
		}
		repo := Repo{
			Name:          name,
			Format:        orDefault(text(get(obj, "format")), "unknown"),
			Type:          orDefault(text(get(obj, "type")), "hosted"),
			Members:       members,
			Defined:       true,
			VersionPolicy: text(get(obj, "version_policy")),
			LayoutPolicy:  text(get(obj, "layout_policy")),
			WritePolicy:   text(get(obj, "write_policy")),
			RemoteURL:     text(get(obj, "remote_url")),
		}
		if value := get(obj, "http_port"); value != nil {
			p, ok := port(value)
			if !ok {
				return nil, valueError("%s: http_port of %s is not a port number: %s", path, pystr.Repr(name),
					pystr.Repr(value))
			}
			repo.HTTPPort = &p
		}
		out = append(out, repo)
	}
	return out, nil
}

func section(item *pyjson.Object, key string) *pyjson.Object {
	if obj, ok := get(item, key).(*pyjson.Object); ok {
		return obj
	}
	return pyjson.NewObject()
}

// ImportRepositorySettings writes target in the form of repositories.json
// from a repository settings export of the old product and returns the count
// of repositories. Only the fields of that form are taken over; credentials,
// HTTP client settings and everything else of the export stay behind. A JSON
// error of the export is the *pyjson.SyntaxError itself, as Python raises its
// JSONDecodeError.
func ImportRepositorySettings(source, target string) (int, error) {
	source = pystr.Path(source)
	data, err := os.ReadFile(source) // #nosec G304 -- the operator names the settings export on the command line.
	if err != nil {
		return 0, err
	}
	value, err := pyjson.DecodeText(data)
	if err != nil {
		return 0, err
	}
	if obj, ok := value.(*pyjson.Object); ok {
		if items, ok := get(obj, "items").([]any); ok {
			value = items
		}
	}
	list, ok := value.([]any)
	if !ok {
		return 0, valueError("%s does not hold a list of repositories", source)
	}
	var out []*pyjson.Object
	names := map[string]bool{}
	for _, entry := range list {
		item, ok := entry.(*pyjson.Object)
		if !ok {
			continue
		}
		name := text(get(item, "name"))
		if name == nil || names[*name] {
			continue
		}
		names[*name] = true
		members := []any{}
		if value := get(section(item, "group"), "memberNames"); truthy(value) {
			if list, ok := value.([]any); ok {
				for _, m := range list {
					if s, ok := m.(string); ok {
						members = append(members, s)
					}
				}
			}
		}
		var httpPort any
		if p, ok := port(get(section(item, "docker"), "httpPort")); ok {
			httpPort = p
		}
		out = append(out, pyjson.NewObject().
			Set("name", *name).
			Set("format", orDefault(text(get(item, "format")), "unknown")).
			Set("type", orDefault(text(get(item, "type")), "hosted")).
			Set("members", members).
			Set("version_policy", optional(text(get(section(item, "maven"), "versionPolicy")))).
			Set("layout_policy", optional(text(get(section(item, "maven"), "layoutPolicy")))).
			Set("write_policy", optional(text(get(section(item, "storage"), "writePolicy")))).
			Set("remote_url", optional(text(get(section(item, "proxy"), "remoteUrl")))).
			Set("http_port", httpPort))
	}
	if err := WriteDefinitions(target, out); err != nil {
		return 0, err
	}
	return len(out), nil
}

// WriteDefinitions writes {"repositories": entries} atomically, each entry
// with the keys of Fields in their order; a key an entry lacks is null.
func WriteDefinitions(target string, entries []*pyjson.Object) error {
	target = pystr.Path(target)
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	list := make([]any, 0, len(entries))
	for _, entry := range entries {
		obj := pyjson.NewObject()
		for _, key := range Fields {
			obj.Set(key, get(entry, key))
		}
		list = append(list, obj)
	}
	data, err := pyjson.DumpsIndent(pyjson.NewObject().Set("repositories", list), 2)
	if err != nil {
		return err
	}
	return config.WriteAtomic(target, ".repositories-", append(data, '\n'))
}
