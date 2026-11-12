// Package listeners holds the ports of one server process: the main port
// and one port per Docker repository.
//
// The port of a Docker repository comes from config.json (docker_ports),
// else from docker.httpPort of the settings import. Every socket binds the
// configured address. The ports are opened at start, all or none, before the
// application is built; a port changed later takes effect at the next start.
package listeners

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/repos"
)

const (
	LowestPort  = 1024
	HighestPort = 65535
)

var reservedPorts = []int{8791} // kept for another service on this machine

// ReservedPorts returns the ports no listener of this server may take.
func ReservedPorts() []int {
	return slices.Clone(reservedPorts)
}

// IsReserved reports whether port is one of ReservedPorts.
func IsReserved(port int) bool {
	return slices.Contains(reservedPorts, port)
}

// DockerPortOf is the port of a Docker repository: the configuration wins
// over the definition. ok is false for other formats and without a port.
func DockerPortOf(cfg *config.Config, repo repos.Repo) (port int, ok bool) {
	if repos.AccessOf(repo) != "docker" {
		return 0, false
	}
	if port, ok := cfg.DockerPorts[repo.Name]; ok {
		return port, true
	}
	if repo.HTTPPort != nil {
		return *repo.HTTPPort, true
	}
	return 0, false
}

// DockerPortMap maps port to repository for every Docker repository with a
// port, also one without data. The error, a ValueError with the port in its
// message, comes for a port equal to cfg.Port, a reserved port, a port of two
// repositories, or one outside LowestPort to HighestPort; the repositories
// are checked in the order of All.
func DockerPortMap(cfg *config.Config, r *repos.Repositories) (map[int]string, error) {
	ports := map[int]string{}
	for _, repo := range r.All() {
		port, ok := DockerPortOf(cfg, repo)
		if !ok {
			continue
		}
		var message string
		switch other, taken := ports[port]; {
		case port < LowestPort || port > HighestPort:
			message = fmt.Sprintf("Docker port %d of %s is outside %d to %d", port, repo.Name, LowestPort, HighestPort)
		case port == cfg.Port:
			message = fmt.Sprintf("Docker port %d of %s is the main port of the server", port, repo.Name)
		case IsReserved(port):
			message = fmt.Sprintf("Docker port %d of %s is reserved on this machine", port, repo.Name)
		case taken:
			message = fmt.Sprintf("Docker port %d is set for both %s and %s", port, other, repo.Name)
		}
		if message != "" {
			return nil, &pyjson.Error{Class: "ValueError", Message: message}
		}
		ports[port] = repo.Name
	}
	return ports, nil
}

// SortedPorts returns the ports of a port map in ascending order.
func SortedPorts(m map[int]string) []int {
	ports := make([]int, 0, len(m))
	for port := range m {
		ports = append(ports, port)
	}
	slices.Sort(ports)
	return ports
}

// IndexReader is what LoadRepositories reads from the index.
type IndexReader interface {
	RepoNames() ([]string, error)
	Hints() (map[string]string, error)
	Close() error
}

// LoadRepositories returns the definitions plus the repositories of the
// index with their hints. It opens the index only when its file exists, so
// it creates none.
func LoadRepositories(cfg *config.Config, open func(path string) (IndexReader, error)) (*repos.Repositories, error) {
	var names []string
	var hints map[string]string
	if _, err := os.Stat(cfg.IndexPath()); err == nil {
		if names, hints, err = readIndex(cfg.IndexPath(), open); err != nil {
			return nil, err
		}
	} else if !absent(err) {
		return nil, err
	}
	defined, err := repos.LoadDefinitions(cfg.RepositoriesPath())
	if err != nil {
		return nil, err
	}
	return repos.New(defined, names, hints), nil
}

// absent is the set of errors for which Path.exists() of Python 3.12 says
// False instead of raising.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EBADF) ||
		errors.Is(err, syscall.ELOOP)
}

func readIndex(path string, open func(string) (IndexReader, error)) ([]string, map[string]string, error) {
	index, err := open(path)
	if err != nil {
		return nil, nil, err
	}
	defer index.Close()
	names, err := index.RepoNames()
	if err != nil {
		return nil, nil, err
	}
	hints, err := index.Hints()
	if err != nil {
		return nil, nil, err
	}
	return names, hints, nil
}

// ListenError is the OSError of Python's open_sockets. Error() is str() of
// that exception: "[Errno <n>] cannot listen on <bind> port <port>: <text>".
type ListenError struct {
	Bind  string
	Port  int
	Errno syscall.Errno // 0 when the system gave none
	Err   error
}

func (e *ListenError) Error() string {
	reason := e.Err.Error()
	if e.Errno != 0 {
		// strerror of C starts with a capital letter, the texts of Go do not
		reason = e.Errno.Error()
		if reason != "" {
			reason = strings.ToUpper(reason[:1]) + reason[1:]
		}
		return fmt.Sprintf("[Errno %d] cannot listen on %s port %d: %s", int(e.Errno), e.Bind, e.Port, reason)
	}
	return fmt.Sprintf("[Errno None] cannot listen on %s port %d: %s", e.Bind, e.Port, reason)
}

func (e *ListenError) Unwrap() error { return e.Err }

// Open returns one listening TCP socket per port on bind, all or none: when
// one port fails, those opened before it are closed again. An address with
// a ':' is IPv6 and only IPv6, any other is IPv4. Go sets SO_REUSEADDR on
// every listener, as uvicorn does for a restart.
func Open(bind string, ports []int) ([]net.Listener, error) {
	network := "tcp4"
	host := bind
	if strings.Contains(bind, ":") {
		network, host = "tcp6", "["+bind+"]"
	}
	opened := make([]net.Listener, 0, len(ports))
	for _, port := range ports {
		l, err := net.Listen(network, host+":"+strconv.Itoa(port))
		if err != nil {
			for _, o := range opened {
				_ = o.Close() // cleanup; the listen error is the one returned
			}
			listenErr := &ListenError{Bind: bind, Port: port, Err: err}
			errors.As(err, &listenErr.Errno)
			return nil, listenErr
		}
		opened = append(opened, l)
	}
	return opened, nil
}
