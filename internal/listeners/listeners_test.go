package listeners_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"
	"testing"

	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/listeners"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/repos"
)

// The vectors come from tools/vectors/listeners.py, which runs
// docker_port_of and docker_port_map of the reference.
type vectorFile struct {
	Python        string     `json:"python"`
	ReservedPorts []int      `json:"reserved_ports"`
	LowestPort    int        `json:"lowest_port"`
	HighestPort   int        `json:"highest_port"`
	PortMap       []portCase `json:"port_map"`
}

type repoDict struct {
	Name     string   `json:"name"`
	Format   string   `json:"format"`
	Type     string   `json:"type"`
	Members  []string `json:"members"`
	Defined  bool     `json:"defined"`
	HTTPPort *int     `json:"http_port"`
}

type portCase struct {
	Name        string            `json:"name"`
	Defined     []repoDict        `json:"defined"`
	Discovered  []string          `json:"discovered"`
	Hints       map[string]string `json:"hints"`
	Port        int               `json:"port"`
	DockerPorts map[string]int    `json:"docker_ports"`
	PortOf      map[string]*int   `json:"port_of"`
	Map         [][2]any          `json:"map"`
	Error       *string           `json:"error"`
}

func load(t *testing.T) *vectorFile {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/listeners.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.PortMap) < 10 {
		t.Fatalf("%d port map cases", len(v.PortMap))
	}
	return &v
}

func repositories(c portCase) *repos.Repositories {
	var defined []repos.Repo
	for _, d := range c.Defined {
		defined = append(defined, repos.Repo{Name: d.Name, Format: d.Format, Type: d.Type, Defined: d.Defined,
			HTTPPort: d.HTTPPort})
	}
	return repos.New(defined, c.Discovered, c.Hints)
}

func TestConstants(t *testing.T) {
	v := load(t)
	if !reflect.DeepEqual(listeners.ReservedPorts(), v.ReservedPorts) || listeners.LowestPort != v.LowestPort ||
		listeners.HighestPort != v.HighestPort {
		t.Errorf("constants %v %d %d", listeners.ReservedPorts(), listeners.LowestPort, listeners.HighestPort)
	}
	if !listeners.IsReserved(8791) || listeners.IsReserved(8793) {
		t.Error("IsReserved is wrong on 8791 or 8793")
	}
}

func TestPortMapVectors(t *testing.T) {
	v := load(t)
	for _, c := range v.PortMap {
		cfg := config.New("unused")
		cfg.Port = c.Port
		for name, port := range c.DockerPorts {
			cfg.DockerPorts[name] = port
		}
		r := repositories(c)
		for name, want := range c.PortOf {
			repo, ok := r.Get(name)
			if !ok {
				t.Fatalf("%s: no repository %s", c.Name, name)
			}
			port, has := listeners.DockerPortOf(cfg, repo)
			if (want == nil) == has || (want != nil && port != *want) {
				t.Errorf("%s: DockerPortOf(%s) = %d %v, want %v", c.Name, name, port, has, want)
			}
		}
		got, err := listeners.DockerPortMap(cfg, r)
		if c.Error != nil {
			if err == nil || err.Error() != *c.Error || !pyjson.IsValueError(err) {
				t.Errorf("%s: %v %v, want %q", c.Name, got, err, *c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		var pairs []string
		for _, port := range listeners.SortedPorts(got) {
			pairs = append(pairs, fmt.Sprintf("%d=%s", port, got[port]))
		}
		var want []string
		for _, pair := range c.Map {
			want = append(want, fmt.Sprintf("%d=%s", int(pair[0].(float64)), pair[1].(string)))
		}
		if len(got) != len(c.Map) || !reflect.DeepEqual(pairs, want) {
			t.Errorf("%s: %v, want %v", c.Name, pairs, want)
		}
	}
}

// freePorts finds ports of this repository's test range that nothing holds now.
func freePorts(t *testing.T, count int) []int {
	t.Helper()
	var ports []int
	start := 14600 + rand.IntN(300)
	for offset := 0; offset < 400 && len(ports) < count; offset++ {
		port := 14600 + (start-14600+offset)%400
		probe, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		probe.Close()
		ports = append(ports, port)
	}
	if len(ports) < count {
		t.Fatalf("only %d free ports in 14600 to 14999", len(ports))
	}
	return ports
}

func canBind(port int) bool {
	probe, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	probe.Close()
	return true
}

func portOf(l net.Listener) int {
	return l.Addr().(*net.TCPAddr).Port
}

func TestOpenAllOrNone(t *testing.T) {
	holder, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	taken := portOf(holder)
	ports := freePorts(t, 2)
	first, third := ports[0], ports[1]
	opened, err := listeners.Open("127.0.0.1", []int{first, taken, third})
	if err == nil {
		for _, l := range opened {
			l.Close()
		}
		t.Fatal("Open over a taken port gave no error")
	}
	want := fmt.Sprintf("[Errno %d] cannot listen on 127.0.0.1 port %d: Address already in use", int(syscall.EADDRINUSE), taken)
	if err.Error() != want {
		t.Errorf("error %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("%v is not EADDRINUSE", err)
	}
	var le *listeners.ListenError
	if !errors.As(err, &le) || le.Port != taken || le.Bind != "127.0.0.1" {
		t.Errorf("ListenError %+v", le)
	}
	if opened != nil {
		t.Errorf("Open returned listeners with the error: %v", opened)
	}
	if !canBind(first) || !canBind(third) {
		t.Error("a port opened before the failure is still held")
	}
}

func TestOpenOnePerPort(t *testing.T) {
	ports := freePorts(t, 3)
	opened, err := listeners.Open("127.0.0.1", ports)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, l := range opened {
		got = append(got, portOf(l))
	}
	if !reflect.DeepEqual(got, ports) {
		t.Errorf("ports %v, want %v", got, ports)
	}
	for _, port := range ports {
		if canBind(port) {
			t.Errorf("port %d is not held", port)
		}
	}
	for _, l := range opened {
		l.Close()
	}
	for _, port := range ports {
		if !canBind(port) {
			t.Errorf("port %d is held after Close", port)
		}
	}
}

func TestOpenPortZero(t *testing.T) {
	opened, err := listeners.Open("localhost", []int{0, 0})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, l := range opened {
			l.Close()
		}
	}()
	if len(opened) != 2 || portOf(opened[0]) == portOf(opened[1]) {
		t.Errorf("listeners %v", opened)
	}
	for _, l := range opened {
		if ip := l.Addr().(*net.TCPAddr).IP.String(); ip != "127.0.0.1" {
			t.Errorf("localhost bound %s", ip)
		}
	}
}

func TestBindAddress(t *testing.T) {
	for _, bind := range []string{"127.0.0.1", "::1"} {
		if bind == "::1" {
			probe, err := net.Listen("tcp6", "[::1]:0")
			if err != nil {
				t.Logf("no IPv6 loopback here: %v", err)
				continue
			}
			probe.Close()
		}
		opened, err := listeners.Open(bind, []int{0, 0, 0})
		if err != nil {
			t.Fatalf("%s: %v", bind, err)
		}
		if len(opened) != 3 {
			t.Errorf("%s: %d listeners", bind, len(opened))
		}
		for _, l := range opened {
			if ip := l.Addr().(*net.TCPAddr).IP.String(); ip != bind {
				t.Errorf("bound %s, want %s", ip, bind)
			}
			l.Close()
		}
	}
}

type fakeIndex struct {
	names  []string
	hints  map[string]string
	err    error
	closed bool
}

func (f *fakeIndex) RepoNames() ([]string, error)      { return f.names, f.err }
func (f *fakeIndex) Hints() (map[string]string, error) { return f.hints, nil }
func (f *fakeIndex) Close() error                      { f.closed = true; return nil }

func TestLoadRepositories(t *testing.T) {
	root := t.TempDir()
	cfg := config.New(root)
	definitions := `{"repositories": [{"name": "demo-docker", "format": "docker", "http_port": 18080}]}`
	if err := os.WriteFile(cfg.RepositoriesPath(), []byte(definitions), 0o644); err != nil {
		t.Fatal(err)
	}
	opened := 0
	index := &fakeIndex{names: []string{"demo-docker", "demo-docker-old"}, hints: map[string]string{"demo-docker-old": "docker"}}
	open := func(path string) (listeners.IndexReader, error) {
		opened++
		if path != cfg.IndexPath() {
			t.Errorf("opened %s", path)
		}
		return index, nil
	}
	// no index file: nothing is opened and the definitions alone count
	r, err := listeners.LoadRepositories(cfg, open)
	if err != nil || opened != 0 || len(r.All()) != 1 {
		t.Fatalf("without index: %v %d %v", r, opened, err)
	}
	if err := os.WriteFile(cfg.IndexPath(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err = listeners.LoadRepositories(cfg, open)
	if err != nil || opened != 1 || !index.closed {
		t.Fatalf("with index: %v %d %v closed=%v", r, opened, err, index.closed)
	}
	old, ok := r.Get("demo-docker-old")
	if !ok || old.Format != "docker" || old.Defined {
		t.Errorf("demo-docker-old = %+v %v", old, ok)
	}
	// an error of the index comes back, and the index is closed all the same
	failing := &fakeIndex{err: errors.New("demo index error")}
	_, err = listeners.LoadRepositories(cfg, func(string) (listeners.IndexReader, error) { return failing, nil })
	if err == nil || err.Error() != "demo index error" || !failing.closed {
		t.Errorf("failing index: %v closed=%v", err, failing.closed)
	}
	_, err = listeners.LoadRepositories(cfg, func(string) (listeners.IndexReader, error) { return nil, errors.New("demo open") })
	if err == nil || err.Error() != "demo open" {
		t.Errorf("failing open: %v", err)
	}
	// broken definitions come back as the error of LoadDefinitions
	if err := os.WriteFile(filepath.Join(root, "repositories.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := listeners.LoadRepositories(cfg, open); err == nil || !pyjson.IsValueError(err) {
		t.Errorf("broken definitions: %v", err)
	}
}
