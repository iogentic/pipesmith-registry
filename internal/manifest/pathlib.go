package manifest

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/pystr"
)

// This file ports what the Python module takes from pathlib: Path.is_dir,
// Path.is_file, Path.name and Path.resolve(strict=True). internal/stores
// imports this package, so the strict branch of its realpath is repeated
// here.

// maxLinks bounds the symbolic links one resolution follows, as in stores.
const maxLinks = 900

// ignoredStat are the errors for which Path.is_dir and Path.is_file of
// Python 3.12 answer False instead of raising.
var ignoredStat = []error{syscall.ENOENT, syscall.ENOTDIR, syscall.EBADF, syscall.ELOOP}

func statMode(p string) (fs.FileMode, bool, error) {
	info, err := os.Stat(p)
	if err != nil {
		for _, ignored := range ignoredStat {
			if errors.Is(err, ignored) {
				return 0, false, nil
			}
		}
		return 0, false, err
	}
	return info.Mode(), true, nil
}

func isDir(p string) (bool, error) {
	mode, ok, err := statMode(p)
	return ok && mode.IsDir(), err
}

func isFile(p string) (bool, error) {
	mode, ok, err := statMode(p)
	return ok && mode.IsRegular(), err
}

// baseName is PurePosixPath(p).name.
func baseName(p string) string {
	p = pystr.Path(p)
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	if p == "." {
		return ""
	}
	return p
}

// fsDecode is os.fsdecode of a file name in the str form of internal/pystr:
// every byte outside valid UTF-8 becomes U+DC00 plus the byte.
func fsDecode(name string) string {
	if utf8.ValidString(name) {
		return name
	}
	runes := make([]rune, 0, len(name))
	for i := 0; i < len(name); {
		r, width := utf8.DecodeRuneInString(name[i:])
		if r == utf8.RuneError && width == 1 {
			r = 0xDC00 + rune(name[i])
		}
		runes = append(runes, r)
		i += width
	}
	return pystr.FromRunes(runes)
}

// pyJoin is os.path.join.
func pyJoin(a string, more ...string) string {
	path := a
	for _, b := range more {
		switch {
		case strings.HasPrefix(b, "/"):
			path = b
		case path == "" || strings.HasSuffix(path, "/"):
			path += b
		default:
			path += "/" + b
		}
	}
	return path
}

// pySplit is os.path.split.
func pySplit(p string) (string, string) {
	i := strings.LastIndex(p, "/") + 1
	head, tail := p[:i], p[i:]
	if head != "" && head != strings.Repeat("/", len(head)) {
		head = strings.TrimRight(head, "/")
	}
	return head, tail
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
		if comp != ".." || (initial == 0 && len(comps) == 0) || (len(comps) > 0 && comps[len(comps)-1] == "..") {
			comps = append(comps, comp)
		} else if len(comps) > 0 {
			comps = comps[:len(comps)-1]
		}
	}
	out := strings.Repeat("/", initial) + strings.Join(comps, "/")
	if out == "" {
		return "."
	}
	return out
}

// abspath is os.path.abspath, with the directory the kernel reports.
func abspath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		cwd, err := syscall.Getwd()
		if err != nil {
			if cwd, err = os.Getwd(); err != nil {
				return "", err
			}
		}
		p = pyJoin(cwd, p)
	}
	return normpath(p), nil
}

// realpath is os.path.realpath(filename, strict=True) of Python 3.12: a
// missing component or a symlink loop is an error, and ".." applies to the
// path after the links before it are resolved.
func realpath(filename string) (string, error) {
	links := 0
	path, err := joinRealpath("", filename, map[string]*string{}, &links)
	if err != nil {
		return "", err
	}
	return abspath(path)
}

func joinRealpath(path, rest string, seen map[string]*string, links *int) (string, error) {
	if strings.HasPrefix(rest, "/") {
		rest = rest[1:]
		path = "/"
	}
	for rest != "" {
		var name string
		name, rest, _ = strings.Cut(rest, "/")
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			if path != "" {
				path, name = pySplit(path)
				if name == ".." {
					path = pyJoin(path, "..", "..")
				}
			} else {
				path = ".."
			}
			continue
		}
		newpath := pyJoin(path, name)
		info, err := os.Lstat(newpath)
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			path = newpath
			continue
		}
		if cached, ok := seen[newpath]; ok {
			if cached != nil {
				path = *cached
				continue
			}
			return "", &fs.PathError{Op: "stat", Path: newpath, Err: syscall.ELOOP}
		}
		*links++
		if *links > maxLinks {
			return "", errors.New("maximum recursion depth exceeded")
		}
		seen[newpath] = nil
		target, err := os.Readlink(newpath)
		if err != nil {
			return "", err
		}
		if path, err = joinRealpath(path, target, seen, links); err != nil {
			return "", err
		}
		resolved := path
		seen[newpath] = &resolved
	}
	return path, nil
}
