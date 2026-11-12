package repos_test

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
	"time"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// The vectors come from tools/vectors/repos.py, which runs repos.py of the
// reference. Paths below a case directory are <root>.
type vectorFile struct {
	Python       string             `json:"python"`
	Fields       []string           `json:"fields"`
	Access       map[string]string  `json:"access"`
	AccessOf     map[string]string  `json:"access_of"`
	Secrets      []string           `json:"secrets"`
	Import       []importCase       `json:"import"`
	Load         []loadCase         `json:"load"`
	Write        []writeCase        `json:"write"`
	Repositories []repositoriesCase `json:"repositories"`
	Rows         []rowsCase         `json:"rows"`
}

type pyError struct {
	Class   string `json:"class"`
	Message string `json:"message"`
	OSError bool   `json:"oserror"`
}

type repoDict struct {
	Name          string   `json:"name"`
	Format        string   `json:"format"`
	Type          string   `json:"type"`
	Members       []string `json:"members"`
	Defined       bool     `json:"defined"`
	VersionPolicy *string  `json:"version_policy"`
	LayoutPolicy  *string  `json:"layout_policy"`
	WritePolicy   *string  `json:"write_policy"`
	RemoteURL     *string  `json:"remote_url"`
	HTTPPort      *int     `json:"http_port"`
}

func (d repoDict) repo() repos.Repo {
	var members []string
	if len(d.Members) > 0 {
		members = append(members, d.Members...)
	}
	return repos.Repo{Name: d.Name, Format: d.Format, Type: d.Type, Members: members, Defined: d.Defined,
		VersionPolicy: d.VersionPolicy, LayoutPolicy: d.LayoutPolicy, WritePolicy: d.WritePolicy,
		RemoteURL: d.RemoteURL, HTTPPort: d.HTTPPort}
}

type importCase struct {
	Name      string     `json:"name"`
	Source    *string    `json:"source_b64"`
	Count     *int       `json:"count"`
	Target    string     `json:"target_b64"`
	Mode      string     `json:"mode"`
	Loaded    []repoDict `json:"loaded"`
	Error     *pyError   `json:"error"`
	DataFiles []string   `json:"data_files"`
}

type loadCase struct {
	Name  string     `json:"name"`
	File  *string    `json:"file_b64"`
	Repos []repoDict `json:"repos"`
	Error *pyError   `json:"error"`
}

type writeCase struct {
	Name    string            `json:"name"`
	Entries []json.RawMessage `json:"entries"`
	Bytes   string            `json:"bytes_b64"`
	Files   []string          `json:"files"`
}

type repositoriesCase struct {
	Name       string               `json:"name"`
	Defined    []repoDict           `json:"defined"`
	Discovered []string             `json:"discovered"`
	Hints      map[string]string    `json:"hints"`
	All        []repoDict           `json:"all"`
	Get        map[string]*repoDict `json:"get"`
	Resolve    map[string][]string  `json:"resolve"`
}

type rowsCase struct {
	Name       string            `json:"name"`
	Defined    []repoDict        `json:"defined"`
	Discovered []string          `json:"discovered"`
	Hints      map[string]string `json:"hints"`
	Stats      []struct {
		Repo     string `json:"repo"`
		Files    int64  `json:"files"`
		Bytes    int64  `json:"bytes"`
		NewestMS int64  `json:"newest_ms"`
	} `json:"stats"`
	DockerPorts map[string]int `json:"docker_ports"`
	JSON        string         `json:"json"`
}

func load(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/repos.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Import) == 0 || len(v.Load) == 0 || len(v.Repositories) == 0 || len(v.Rows) == 0 {
		t.Fatal("vector file holds no cases")
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

func defined(ds []repoDict) []repos.Repo {
	var out []repos.Repo
	for _, d := range ds {
		out = append(out, d.repo())
	}
	return out
}

func checkRepos(t *testing.T, name string, got []repos.Repo, want []repoDict) {
	t.Helper()
	w := defined(want)
	if len(got) == 0 && len(w) == 0 {
		return
	}
	if !reflect.DeepEqual(got, w) {
		g, _ := json.Marshal(got)
		e, _ := json.Marshal(w)
		t.Errorf("%s:\n got %s\nwant %s", name, g, e)
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
		if !errors.As(err, &pathErr) || (want.Class == "FileNotFoundError" && !errors.Is(err, syscall.ENOENT)) {
			t.Errorf("%s: %T %v, want an OS error %s", name, err, err, want.Class)
		}
		return
	}
	var message, class string
	var syntax *pyjson.SyntaxError
	var pe *pyjson.Error
	switch {
	case errors.As(err, &syntax):
		class, message = "JSONDecodeError", syntax.Error()
	case errors.As(err, &pe):
		class, message = pe.Class, pe.Message
	default:
		t.Errorf("%s: %T %v, want %s %q", name, err, err, want.Class, want.Message)
		return
	}
	if message = strings.ReplaceAll(message, root, "<root>"); class != want.Class || message != want.Message {
		t.Errorf("%s:\n got %s %q\nwant %s %q", name, class, message, want.Class, want.Message)
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestConstants(t *testing.T) {
	v := load(t)
	if !reflect.DeepEqual(repos.Fields, v.Fields) {
		t.Errorf("Fields %v, want %v", repos.Fields, v.Fields)
	}
	if !reflect.DeepEqual(repos.Access, v.Access) {
		t.Errorf("Access %v, want %v", repos.Access, v.Access)
	}
	for format, want := range v.AccessOf {
		if got := repos.AccessOf(repos.Repo{Name: "x", Format: format, Type: "hosted"}); got != want {
			t.Errorf("AccessOf(%q) = %q, want %q", format, got, want)
		}
	}
}

func TestImportVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Import {
		root := t.TempDir()
		source := filepath.Join(root, "repositorySettings.json")
		if c.Source != nil {
			if err := os.WriteFile(source, unb64(t, *c.Source), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		target := filepath.Join(root, "data", "repositories.json")
		count, err := repos.ImportRepositorySettings(source, target)
		if c.Error != nil {
			checkError(t, c.Name, err, root, c.Error)
		} else if err != nil {
			t.Errorf("%s: %v", c.Name, err)
		} else {
			if count != *c.Count {
				t.Errorf("%s: count %d, want %d", c.Name, count, *c.Count)
			}
			got, err := os.ReadFile(target)
			want := unb64(t, c.Target)
			if err != nil || string(got) != string(want) {
				t.Errorf("%s:\n got %q %v\nwant %q", c.Name, got, err, want)
			}
			for _, secret := range v.Secrets {
				if strings.Contains(string(got), secret) {
					t.Errorf("%s: %q reached the target", c.Name, secret)
				}
			}
			if info, err := os.Stat(target); err != nil || fmt.Sprintf("0o%o", info.Mode().Perm()) != c.Mode {
				t.Errorf("%s: mode %v %v, want %s", c.Name, info, err, c.Mode)
			}
			loaded, err := repos.LoadDefinitions(target)
			if err != nil {
				t.Errorf("%s: load: %v", c.Name, err)
			}
			checkRepos(t, c.Name+" (loaded)", loaded, c.Loaded)
		}
		if got := listDir(t, filepath.Join(root, "data")); !reflect.DeepEqual(got, c.DataFiles) {
			t.Errorf("%s: data files %v, want %v", c.Name, got, c.DataFiles)
		}
	}
}

func TestLoadVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Load {
		root := t.TempDir()
		path := filepath.Join(root, "repositories.json")
		if c.File != nil {
			if err := os.WriteFile(path, unb64(t, *c.File), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got, err := repos.LoadDefinitions(path)
		if c.Error != nil {
			checkError(t, c.Name, err, root, c.Error)
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		checkRepos(t, c.Name, got, c.Repos)
	}
}

func TestWriteVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Write {
		root := t.TempDir()
		var entries []*pyjson.Object
		for _, raw := range c.Entries {
			value, err := pyjson.Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			entries = append(entries, value.(*pyjson.Object))
		}
		target := filepath.Join(root, "new", "repositories.json")
		if err := repos.WriteDefinitions(target, entries); err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		got, err := os.ReadFile(target)
		if want := unb64(t, c.Bytes); err != nil || string(got) != string(want) {
			t.Errorf("%s:\n got %q %v\nwant %q", c.Name, got, err, want)
		}
		if files := listDir(t, filepath.Dir(target)); !reflect.DeepEqual(files, c.Files) {
			t.Errorf("%s: files %v, want %v", c.Name, files, c.Files)
		}
	}
}

func TestRepositoriesVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Repositories {
		r := repos.New(defined(c.Defined), c.Discovered, c.Hints)
		checkRepos(t, c.Name+" all", r.All(), c.All)
		for name, want := range c.Get {
			got, ok := r.Get(name)
			if want == nil {
				if ok {
					t.Errorf("%s: Get(%q) = %+v, want none", c.Name, name, got)
				}
				continue
			}
			if !ok || !reflect.DeepEqual(got, want.repo()) {
				t.Errorf("%s: Get(%q) = %+v %v, want %+v", c.Name, name, got, ok, want.repo())
			}
		}
		for name, want := range c.Resolve {
			var got []string
			for _, repo := range r.Resolve(name) {
				got = append(got, repo.Name)
			}
			if len(got) == 0 && len(want) == 0 {
				continue
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: Resolve(%q) = %v, want %v", c.Name, name, got, want)
			}
		}
	}
}

func TestRowsVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.Rows {
		r := repos.New(defined(c.Defined), c.Discovered, c.Hints)
		var stats []repos.Stat
		for _, s := range c.Stats {
			stats = append(stats, repos.Stat{Repo: s.Repo, Files: s.Files, Bytes: s.Bytes, NewestMS: s.NewestMS})
		}
		rows := repos.RepositoryRows(r, stats, c.DockerPorts)
		got, err := pyjson.DumpsIndent(repos.RowsDocument(rows), 2)
		if err != nil || string(got) != c.JSON {
			t.Errorf("%s:\n got %s %v\nwant %s", c.Name, got, err, c.JSON)
		}
	}
}

func TestResolveReturnsOnCycles(t *testing.T) {
	r := repos.New([]repos.Repo{
		{Name: "a", Format: "maven2", Type: "group", Members: []string{"b", "a"}, Defined: true},
		{Name: "b", Format: "maven2", Type: "group", Members: []string{"a", "demo-raw"}, Defined: true},
		{Name: "demo-raw", Format: "raw", Type: "hosted", Defined: true},
	}, nil, nil)
	done := make(chan []repos.Repo, 1)
	go func() { done <- r.Resolve("a") }()
	select {
	case got := <-done:
		if len(got) != 1 || got[0].Name != "demo-raw" {
			t.Errorf("Resolve(a) = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve(a) did not return")
	}
}

func TestDiscoveredRepo(t *testing.T) {
	r := repos.New(nil, []string{"old-repo"}, map[string]string{"old-repo": ""})
	got, ok := r.Get("old-repo")
	want := repos.Repo{Name: "old-repo", Format: "unknown", Type: "hosted", Defined: false}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("Get(old-repo) = %+v %v", got, ok)
	}
	// All returns a copy
	all := r.All()
	all[0].Name = "changed"
	if r.All()[0].Name != "old-repo" {
		t.Error("All shares its slice")
	}
}

func TestRowObject(t *testing.T) {
	port := 18080
	r := repos.New([]repos.Repo{{Name: "demo-docker", Format: "docker", Type: "hosted", Defined: true, HTTPPort: &port}}, nil, nil)
	rows := repos.RepositoryRows(r, nil, nil)
	if len(rows) != 1 || rows[0].DockerPort == nil || *rows[0].DockerPort != 18080 || rows[0].NewestMS != nil {
		t.Fatalf("rows %+v", rows)
	}
	got, err := pyjson.Dumps(rows[0].Object())
	want := `{"name": "demo-docker", "format": "docker", "type": "hosted", "defined": true, "access": "docker", ` +
		`"docker_port": 18080, "members": [], "files": 0, "bytes": 0, "newest_ms": null, "version_policy": null, ` +
		`"layout_policy": null, "write_policy": null, "remote_url": null}`
	if err != nil || string(got) != want {
		t.Errorf("Object:\n got %s %v\nwant %s", got, err, want)
	}
}

func TestWriteLeavesNoTemporaryFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "repositories.json")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := repos.WriteDefinitions(target, nil); err == nil {
		t.Fatal("WriteDefinitions over a directory gave no error")
	}
	if got := listDir(t, root); !reflect.DeepEqual(got, []string{"repositories.json"}) {
		t.Errorf("left behind: %v", got)
	}
	// the positive twin
	other := filepath.Join(root, "sub", "repositories.json")
	if err := repos.WriteDefinitions(other, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(other)
	if err != nil || string(data) != "{\n  \"repositories\": []\n}\n" {
		t.Errorf("empty definitions: %q %v", data, err)
	}
}
