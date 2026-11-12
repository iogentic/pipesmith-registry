package pystr

import "strings"

// splitPath is PurePosixPath._parse_path: the root ("", "/" or "//") and
// the parts without empty and "." parts. Exactly two leading slashes stay
// a root of their own, three or more are one.
func splitPath(p string) (string, []string) {
	root := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		root, p = "//", p[2:]
	case strings.HasPrefix(p, "/"):
		root, p = "/", strings.TrimLeft(p, "/")
	}
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return root, parts
}

func formatPath(root string, parts []string) string {
	if root == "" && len(parts) == 0 {
		return "."
	}
	return root + strings.Join(parts, "/")
}

// Path is str(PurePosixPath(p)). Unlike filepath.Clean it keeps ".." parts
// and two leading slashes.
func Path(p string) string {
	return formatPath(splitPath(p))
}

// Parent is str(PurePosixPath(p).parent).
func Parent(p string) string {
	root, parts := splitPath(p)
	if len(parts) > 0 {
		parts = parts[:len(parts)-1]
	}
	return formatPath(root, parts)
}

// Join is str(PurePosixPath(dir).joinpath(*names)): an absolute name
// starts over, as posixpath.join does.
func Join(dir string, names ...string) string {
	path := Path(dir)
	for _, name := range names {
		switch {
		case strings.HasPrefix(name, "/"):
			path = name
		case path == "" || strings.HasSuffix(path, "/"):
			path += name
		default:
			path += "/" + name
		}
	}
	return Path(path)
}
