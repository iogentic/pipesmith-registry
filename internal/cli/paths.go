package cli

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"strings"
	"syscall"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/pystr"
)

// osExpandUser is os.path.expanduser of a string.
func osExpandUser(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	i := strings.Index(path[1:], "/") + 1
	if i == 0 {
		i = len(path)
	}
	var home string
	if i == 1 {
		if value, ok := os.LookupEnv("HOME"); ok {
			home = value
		} else if u, err := user.Current(); err == nil {
			home = u.HomeDir
		} else {
			return path
		}
	} else {
		u, err := user.Lookup(path[1:i])
		if err != nil {
			return path
		}
		home = u.HomeDir
	}
	if out := strings.TrimRight(home, "/") + path[i:]; out != "" {
		return out
	}
	return "/"
}

// pathExpandUser is str(Path(path).expanduser()).
func pathExpandUser(path string) (string, error) {
	path = pystr.Path(path)
	if strings.HasPrefix(path, "/") || !strings.HasPrefix(path, "~") {
		return path, nil
	}
	first, rest, _ := strings.Cut(path, "/")
	home := osExpandUser(first)
	if strings.HasPrefix(home, "~") {
		return "", &pyjson.Error{Class: "RuntimeError", Message: "Could not determine home directory."}
	}
	if rest == "" {
		return pystr.Path(home), nil
	}
	return pystr.Join(home, rest), nil
}

// normpath is os.path.normpath.
func normpath(path string) string {
	if path == "" {
		return "."
	}
	initial := 0
	if strings.HasPrefix(path, "/") {
		initial = 1
		if strings.HasPrefix(path, "//") && !strings.HasPrefix(path, "///") {
			initial = 2
		}
	}
	var comps []string
	for _, comp := range strings.Split(path, "/") {
		if comp == "" || comp == "." {
			continue
		}
		if comp != ".." || initial == 0 && len(comps) == 0 || len(comps) > 0 && comps[len(comps)-1] == ".." {
			comps = append(comps, comp)
		} else if len(comps) > 0 {
			comps = comps[:len(comps)-1]
		}
	}
	if out := strings.Repeat("/", initial) + strings.Join(comps, "/"); out != "" {
		return out
	}
	return "."
}

// abspath is str(Path(os.path.abspath(path))). The working directory is
// the one the kernel reports, as os.getcwd gives it.
func abspath(path string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		cwd, err := syscall.Getwd()
		if err != nil {
			if cwd, err = os.Getwd(); err != nil {
				return "", err
			}
		}
		if strings.HasSuffix(cwd, "/") {
			path = cwd + path
		} else {
			path = cwd + "/" + path
		}
	}
	return pystr.Path(normpath(path)), nil
}

// ignored is the set of errors for which Path.exists, is_dir and is_file
// of Python 3.12 answer False instead of raising.
func ignored(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EBADF) ||
		errors.Is(err, syscall.ELOOP)
}

func stat(path string) (fs.FileInfo, error) {
	if strings.Contains(path, "\x00") {
		return nil, nil // ValueError: False
	}
	info, err := os.Stat(path)
	if err != nil {
		if ignored(err) {
			return nil, nil
		}
		return nil, err
	}
	return info, nil
}

func exists(path string) (bool, error) {
	info, err := stat(path)
	return info != nil, err
}

func isDir(path string) (bool, error) {
	info, err := stat(path)
	return info != nil && info.IsDir(), err
}

func isFile(path string) (bool, error) {
	info, err := stat(path)
	return info != nil && info.Mode().IsRegular(), err
}
