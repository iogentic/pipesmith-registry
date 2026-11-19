package npm

import (
	"regexp"
	"strings"
)

// The paths of the write calls of the npm registry protocol as the npm
// command line sends them (npm/cli, branch latest, read 2026-11-18:
// libnpmpublish publish.js and unpublish.js, lib/commands/dist-tag.js and
// deprecate.js). Nothing here reads or writes a store.

// WriteKind is what an npm write names.
type WriteKind int

const (
	// WriteDocument is <name> (publish, deprecate) or <name>/-rev/<rev>
	// (the rewrite of an unpublish of one version, the delete of a package).
	WriteDocument WriteKind = iota + 1
	// WriteTarball is <name>/-/<file>.tgz, with /-rev/<rev> as the npm
	// command line sends its delete.
	WriteTarball
	// WriteDistTag is -/package/<name>/dist-tags/<tag>.
	WriteDistTag
)

// WritePath is the target of one npm write.
type WritePath struct {
	Kind WriteKind
	Name string // the package, scope included
	File string // WriteTarball: the file name
	Tag  string // WriteDistTag: the tag
	Rev  string // the revision the path names, "" for none
}

const (
	revMark      = "/" + TarballDirectory + "rev/"
	distTagsMark = "/dist-tags/"
	packageMark  = TarballDirectory + "/package/"
	tarballExt   = ".tgz"
)

var (
	tagPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	filePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,250}\.tgz$`)
	revPattern  = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)
)

// DistTagsPath is the package of -/package/<name>/dist-tags, the read of
// the tags that npm dist-tag sends before it writes (fetchTags of
// lib/commands/dist-tag.js); false for any other path.
func DistTagsPath(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, packageMark)
	if !ok {
		return "", false
	}
	name, ok := strings.CutSuffix(rest, strings.TrimSuffix(distTagsMark, "/"))
	if !ok {
		return "", false
	}
	if found, ok := PackageName(name); !ok || found != name {
		return "", false
	}
	return name, true
}

// ParseWrite is the target of a write path; false for a path that is no
// npm write call.
func ParseWrite(path string) (WritePath, bool) {
	if rest, ok := strings.CutPrefix(path, packageMark); ok {
		i := strings.LastIndex(rest, distTagsMark)
		if i < 0 {
			return WritePath{}, false
		}
		name, tag := rest[:i], rest[i+len(distTagsMark):]
		if _, ok := PackageName(name); !ok || tag == "" || strings.Contains(tag, "/") {
			return WritePath{}, false
		}
		return WritePath{Kind: WriteDistTag, Name: name, Tag: tag}, true
	}
	head, rev := path, ""
	if i := strings.LastIndex(path, revMark); i >= 0 {
		head, rev = path[:i], path[i+len(revMark):]
		if !revPattern.MatchString(rev) {
			return WritePath{}, false
		}
	}
	if name, ok := PackageName(head); ok {
		return WritePath{Kind: WriteDocument, Name: name, Rev: rev}, true
	}
	i := strings.LastIndex(head, "/"+TarballDirectory+"/")
	if i < 0 {
		return WritePath{}, false
	}
	name, file := head[:i], head[i+len(TarballDirectory)+2:]
	if _, ok := PackageName(name); !ok || !filePattern.MatchString(file) {
		return WritePath{}, false
	}
	return WritePath{Kind: WriteTarball, Name: name, File: file, Rev: rev}, true
}
