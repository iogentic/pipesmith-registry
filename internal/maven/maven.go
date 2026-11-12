// Package maven ports maven.py of the Python server: the Maven layout
// helpers, the version order and the merge of maven-metadata.xml for a
// group.
//
// maven.py reads metadata with xml.etree.ElementTree, that is with the expat
// parser built into Python 3.12 (expat 2.6.3). The files xml*.go port the
// parts of expat and ElementTree the merge depends on, so a member document
// parses, or fails to, as it does in Python; etree.go writes the merged
// document as ElementTree writes it.
//
// Strings are Python str values in the form of internal/pystr. A Python
// exception the functions raise comes back as a *pyjson.Error with its
// class and text.
package maven

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mirosso25/pipesmith-registry/internal/pystr"
)

const (
	MetadataName       = "maven-metadata.xml"
	XMLDeclaration     = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
	DefaultContentType = "application/octet-stream"
	snapshotSuffix     = "-SNAPSHOT"
)

var checksumSuffixes = []string{"md5", "sha1", "sha256", "sha512"}

var contentTypes = map[string]string{
	"pom": "application/xml", "xml": "application/xml", "jar": "application/java-archive",
	"md5": "text/plain", "sha1": "text/plain", "sha256": "text/plain", "sha512": "text/plain",
	"json": "application/json", "module": "application/json",
}

// ChecksumSuffixes is CHECKSUM_SUFFIXES.
func ChecksumSuffixes() []string {
	return slices.Clone(checksumSuffixes)
}

func lastSegment(path string) string {
	return path[strings.LastIndex(path, "/")+1:]
}

// IsMetadataPath is is_metadata_path.
func IsMetadataPath(path string) bool {
	return lastSegment(path) == MetadataName
}

// SplitChecksum is split_checksum: (base, suffix) of a checksum path, else
// (path, "") where Python gives (path, None).
func SplitChecksum(path string) (string, string) {
	if i := strings.LastIndex(path, "."); i >= 0 {
		base, suffix := path[:i], path[i+1:]
		if slices.Contains(checksumSuffixes, suffix) && base != "" && !strings.HasSuffix(base, "/") {
			return base, suffix
		}
	}
	return path, ""
}

// ContentTypeFor is content_type_for.
func ContentTypeFor(path string) string {
	name := lastSegment(path)
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return DefaultContentType
	}
	if contentType, ok := contentTypes[pyLower(name[i+1:])]; ok {
		return contentType
	}
	return DefaultContentType
}

// Coordinates are the Maven coordinates of a file; Classifier is nil where
// Python has None.
type Coordinates struct {
	GroupID, ArtifactID, Version string
	Classifier                   *string
	Extension                    string
}

// CoordinatesOf is coordinates(): the coordinates of a file in the layout
// <group>/<artifact>/<version>/<artifact>-<version>[-<classifier>].<ext>,
// nil for metadata, checksum and other files.
func CoordinatesOf(path string) *Coordinates {
	parts := strings.Split(path, "/")
	if len(parts) < 4 || slices.Contains(parts, "") {
		return nil
	}
	n := len(parts)
	group, artifact, version, name := parts[:n-3], parts[n-3], parts[n-2], parts[n-1]
	if name == MetadataName {
		return nil
	}
	if _, suffix := SplitChecksum(name); suffix != "" {
		return nil
	}
	rest, ok := afterVersion(name, artifact, version)
	if !ok {
		return nil
	}
	c := &Coordinates{GroupID: strings.Join(group, "."), ArtifactID: artifact, Version: version}
	switch {
	case strings.HasPrefix(rest, "-"):
		classifier, extension, dot := strings.Cut(rest[1:], ".")
		if classifier == "" || !dot {
			return nil
		}
		c.Classifier, c.Extension = &classifier, extension
	case strings.HasPrefix(rest, "."):
		c.Extension = rest[1:]
	default:
		return nil
	}
	if c.Extension == "" || strings.HasPrefix(c.Extension, ".") || strings.HasSuffix(c.Extension, ".") {
		return nil
	}
	return c
}

// afterVersion is _after_version: what follows <artifact>-<version> in a
// file name, also for a timestamped snapshot.
func afterVersion(name, artifact, version string) (string, bool) {
	prefix := artifact + "-" + version
	if strings.HasPrefix(name, prefix) {
		return name[len(prefix):], true
	}
	if strings.HasSuffix(version, snapshotSuffix) {
		base := artifact + "-" + strings.TrimSuffix(version, snapshotSuffix) + "-"
		if strings.HasPrefix(name, base) {
			if end, ok := timestampEnd(name, len(base)); ok {
				return name[end:], true
			}
		}
	}
	return "", false
}

// timestampEnd matches \d{8}\.\d{6}-\d+ at start, \d being any decimal
// digit as in Python's re, and returns the end of the match.
func timestampEnd(s string, start int) (int, bool) {
	i := start
	run := func(min, max int) bool {
		count := 0
		for i < len(s) && (max < 0 || count < max) {
			r, width := utf8.DecodeRuneInString(s[i:])
			if pystr.DecimalValue(r) < 0 {
				break
			}
			i += width
			count++
		}
		return count >= min
	}
	char := func(c byte) bool {
		if i < len(s) && s[i] == c {
			i++
			return true
		}
		return false
	}
	if run(8, 8) && char('.') && run(6, 6) && char('-') && run(1, -1) {
		return i, true
	}
	return 0, false
}
