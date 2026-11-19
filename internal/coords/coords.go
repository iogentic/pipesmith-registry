// Package coords gives the component coordinates of an asset path by the
// format of its repository, after the table Coordinates of the phase 1c
// plan: maven2, npm, docker and raw. A path a parser cannot place gives no
// component. Every value of the coordinates is a piece of the path, so a
// version is never made up.
package coords

import (
	"regexp"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/docker"
	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/npm"
)

// Coordinates are those of one asset path. Group and Version are empty
// where the coordinates have none; Classifier and Extension are those of a
// maven2 file and empty for the other formats.
type Coordinates struct {
	Group      string
	Name       string
	Version    string
	Classifier string
	Extension  string
	Prerelease bool
}

// Of is the coordinates of path in a repository of format. It is false for
// a path of no component and for a format without a parser here.
func Of(format, path string) (Coordinates, bool) {
	switch format {
	case "maven2":
		return Maven(path)
	case "npm":
		return Npm(path)
	case "docker":
		return Docker(path)
	case "raw":
		return Raw(path)
	}
	return Coordinates{}, false
}

// snapshotTimestamp is the end of a timestamped snapshot version.
var snapshotTimestamp = regexp.MustCompile(`-[0-9]{8}\.[0-9]{6}-[0-9]+$`)

// Maven is a file <group dirs>/<artifact>/<version>/<artifact>-<version>
// [-<classifier>].<extension> as maven.CoordinatesOf reads it: the group
// directories joined by dots, the version of the version directory, also
// for a snapshot file with a timestamp. A checksum file of such a file
// belongs to the same component and carries its suffix in the extension, as
// in jar.sha1. Metadata and its checksums give no component. A version that
// ends in -SNAPSHOT or in a snapshot timestamp is a prerelease.
func Maven(path string) (Coordinates, bool) {
	base, suffix := maven.SplitChecksum(path)
	m := maven.CoordinatesOf(base)
	if m == nil {
		return Coordinates{}, false
	}
	c := Coordinates{Group: m.GroupID, Name: m.ArtifactID, Version: m.Version, Extension: m.Extension}
	if m.Classifier != nil {
		c.Classifier = *m.Classifier
	}
	if suffix != "" {
		c.Extension += "." + suffix
	}
	c.Prerelease = strings.HasSuffix(c.Version, "-SNAPSHOT") || snapshotTimestamp.MatchString(c.Version)
	return c, true
}

// Npm is a tarball <name>/-/<name>-<version>.tgz, with or without @<scope>/
// before the name, or the package document <name>. The group is the scope
// without its @, the name the package name without the scope; the document
// has no version. A tarball whose file name is not that of its package and
// a version outside the grammar of npm.ParsePath give no component. A
// version with a prerelease part (a hyphen before any build part) is a
// prerelease.
func Npm(path string) (Coordinates, bool) {
	name, file, tarball := strings.Cut(path, "/-/")
	pkg, ok := npm.ParsePath(name)
	if !ok || pkg.Version != "" {
		return Coordinates{}, false
	}
	c := Coordinates{Name: npm.Unscoped(pkg.Name)}
	if scope, _, scoped := strings.Cut(pkg.Name, "/"); scoped {
		c.Group = strings.TrimPrefix(scope, "@")
	}
	if !tarball {
		return c, true
	}
	version, ok := strings.CutSuffix(file, ".tgz")
	if ok {
		version, ok = strings.CutPrefix(version, c.Name+"-")
	}
	if !ok || version == "" {
		return Coordinates{}, false
	}
	if checked, ok := npm.ParsePath(pkg.Name + "/" + version); !ok || checked.Version != version {
		return Coordinates{}, false
	}
	c.Version = version
	core, _, _ := strings.Cut(version, "+")
	c.Prerelease = strings.Contains(core, "-")
	return c, true
}

// Docker is a manifest v2/<image>/manifests/<reference> as docker.ParseV2
// reads it: the image is the name, the tag or digest the version, no group.
// A blob and any other path give no component.
func Docker(path string) (Coordinates, bool) {
	rest, ok := strings.CutPrefix(path, "v2/")
	if !ok {
		return Coordinates{}, false
	}
	p, err := docker.ParseV2(rest)
	if err != nil || p == nil || p.Kind != "manifest" {
		return Coordinates{}, false
	}
	return Coordinates{Name: p.Name, Version: p.Reference}, true
}

// Raw is any path of an asset: the group is its directory with a leading
// slash, "/" at the top, the name is the path, no version. A path that is
// empty or starts or ends with a slash names no asset.
func Raw(path string) (Coordinates, bool) {
	if path == "" || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return Coordinates{}, false
	}
	directory := ""
	if cut := strings.LastIndexByte(path, '/'); cut >= 0 {
		directory = path[:cut]
	}
	return Coordinates{Group: "/" + directory, Name: path}, true
}
