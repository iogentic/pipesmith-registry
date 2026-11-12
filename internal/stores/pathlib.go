package stores

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"strings"
	"syscall"
)

// This file ports the parts of posixpath and pathlib that the Python module
// uses on paths: PurePosixPath normalisation, os.path.join, Path.expanduser
// and Path.resolve. Python resolves ".." after the symbolic link before it,
// so "dir/link/../x" lands where the link points; Go's filepath.Clean would
// drop "link/.." first.

// maxLinks bounds the symbolic links one resolution follows, where Python's
// recursion limit would stop _joinrealpath.
const maxLinks = 900

// parsePure splits a path as PurePosixPath does: the root ("", "/" or
// exactly two slashes) and the parts without "" and ".".
func parsePure(p string) (string, []string) {
	root, rest := "", p
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		root, rest = "//", p[2:]
	case strings.HasPrefix(p, "/"):
		root, rest = "/", p[1:]
	}
	var parts []string
	for _, part := range strings.Split(rest, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return root, parts
}

func formatPure(root string, parts []string) string {
	if root == "" && len(parts) == 0 {
		return "."
	}
	return root + strings.Join(parts, "/")
}

// purePath is str(PurePosixPath(p)).
func purePath(p string) string {
	return formatPure(parsePure(p))
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

// joinPath is str(Path(a) / b).
func joinPath(a, b string) string {
	return purePath(pyJoin(purePath(a), b))
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

// getwd is os.getcwd: the directory the kernel reports, not $PWD.
func getwd() (string, error) {
	if dir, err := syscall.Getwd(); err == nil {
		return dir, nil
	}
	return os.Getwd()
}

// abspath is os.path.abspath.
func abspath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		cwd, err := getwd()
		if err != nil {
			return "", err
		}
		p = pyJoin(cwd, p)
	}
	return normpath(p), nil
}

// realpath is os.path.realpath of Python 3.12. With strict, a missing
// component or a symlink loop is an error; without it, the rest of the path
// stays as it is.
func realpath(filename string, strict bool) (string, error) {
	links := 0
	path, _, err := joinRealpath("", filename, strict, map[string]*string{}, &links)
	if err != nil {
		return "", err
	}
	return abspath(path)
}

func joinRealpath(path, rest string, strict bool, seen map[string]*string, links *int) (string, bool, error) {
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
		isLink := false
		info, err := os.Lstat(newpath)
		if err != nil {
			if strict {
				return "", false, err
			}
		} else {
			isLink = info.Mode()&fs.ModeSymlink != 0
		}
		if !isLink {
			path = newpath
			continue
		}
		if cached, ok := seen[newpath]; ok {
			if cached != nil {
				path = *cached
				continue
			}
			// A link that is still being resolved: a loop.
			if strict {
				_, err := os.Stat(newpath)
				if err == nil {
					err = &fs.PathError{Op: "stat", Path: newpath, Err: syscall.ELOOP}
				}
				return "", false, err
			}
			return pyJoin(newpath, rest), false, nil
		}
		*links++
		if *links > maxLinks {
			return "", false, errors.New("maximum recursion depth exceeded")
		}
		seen[newpath] = nil
		target, err := os.Readlink(newpath)
		if err != nil {
			return "", false, err
		}
		var ok bool
		path, ok, err = joinRealpath(path, target, strict, seen, links)
		if err != nil {
			return "", false, err
		}
		if !ok {
			return pyJoin(path, rest), false, nil
		}
		resolved := path
		seen[newpath] = &resolved
	}
	return path, true, nil
}

// resolveRoot is Path(root).resolve(): symbolic links resolved as far as the
// path exists, and a RuntimeError for a symlink loop.
func resolveRoot(root string) (string, error) {
	real, err := realpath(root, false)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return "", &Error{Class: "RuntimeError", Message: "Symlink loop from " + repr(root)}
		}
		return "", err
	}
	if _, err := os.Stat(real); err != nil && errors.Is(err, syscall.ELOOP) {
		return "", &Error{Class: "RuntimeError", Message: "Symlink loop from " + repr(real)}
	}
	return real, nil
}

// within is PurePath.is_relative_to for two resolved absolute paths.
func within(path, root string) bool {
	if path == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return strings.HasPrefix(path, prefix)
}

// isDir is Path.is_dir: false for a path that is missing, not a directory or
// a symlink loop, an error for any other failure of stat.
func isDir(p string) (bool, error) {
	info, err := os.Stat(p)
	if err != nil {
		for _, ignored := range []error{syscall.ENOENT, syscall.ENOTDIR, syscall.EBADF, syscall.ELOOP} {
			if errors.Is(err, ignored) {
				return false, nil
			}
		}
		return false, osError(err)
	}
	return info.IsDir(), nil
}

// homeOf is os.path.expanduser of a first part "~" or "~user"; it returns the
// part unchanged when the home directory is unknown.
func homeOf(part string) string {
	var home string
	if part == "~" {
		if value, ok := os.LookupEnv("HOME"); ok {
			home = value
		} else {
			u, err := user.Current()
			if err != nil {
				return part
			}
			home = u.HomeDir
		}
	} else {
		u, err := user.Lookup(part[1:])
		if err != nil {
			return part
		}
		home = u.HomeDir
	}
	if home = strings.TrimRight(home, "/"); home == "" {
		return "/"
	}
	return home
}

// expandUser is str(Path(p).expanduser()).
func expandUser(p string) (string, error) {
	root, parts := parsePure(p)
	if root != "" || len(parts) == 0 || !strings.HasPrefix(parts[0], "~") {
		return formatPure(root, parts), nil
	}
	home := homeOf(parts[0])
	if strings.HasPrefix(home, "~") {
		return "", &Error{Class: "RuntimeError", Message: "Could not determine home directory."}
	}
	homeRoot, homeParts := parsePure(home)
	return formatPure(homeRoot, append(homeParts, parts[1:]...)), nil
}
