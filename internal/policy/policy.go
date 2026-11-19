// Package policy holds the write rules of a hosted repository as pure
// functions: the write policy, the version and layout policies of maven2,
// the content check and the stored content type (phase 4 plan, W8 and
// section Refusals). The rules are provisional (W6); the cases are in
// testdata/vectors/policy.json.
package policy

import (
	"bytes"
	"mime"
	"net/http"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/maven"
)

// Rule names the rule that refuses a write.
type Rule string

const (
	RuleReadOnly Rule = "read-only" // write policy DENY, or none known
	RuleRedeploy Rule = "redeploy"  // a second write of a path under ALLOW_ONCE
	RuleVersion  Rule = "version"   // a release into a snapshot repository or the other way round
	RuleLayout   Rule = "layout"    // no Maven path under the strict layout policy
	RuleContent  Rule = "content"   // the first bytes do not match the extension
)

// Write policies of a hosted repository.
const (
	Allow     = "ALLOW"
	AllowOnce = "ALLOW_ONCE"
	Deny      = "DENY"
)

// Version policies of a maven2 repository.
const (
	Release  = "RELEASE"
	Snapshot = "SNAPSHOT"
	Mixed    = "MIXED"
)

// Layout policies of a maven2 repository.
const (
	Strict     = "STRICT"
	Permissive = "PERMISSIVE"
)

// HeadBytes is how much of the start of a body Content and ContentType
// read; a caller peeks this many bytes, or the whole body when it is
// shorter.
const HeadBytes = 512

const (
	snapshotSuffix = "-SNAPSHOT"
	signatureExt   = ".asc"
	catalog        = "archetype-catalog.xml"
)

// Refusal is a write that a rule refuses.
type Refusal struct {
	Rule    Rule
	Message string
}

func (r *Refusal) Error() string { return r.Message }

func refuse(rule Rule, message string) error { return &Refusal{Rule: rule, Message: message} }

// Write applies the write policy to a path that exists already or not.
// exempt marks a path the policy lets through a second time, such as
// MavenMetadata or LatestTag. An empty or unknown policy refuses every write:
// a repository whose policy this server does not know stays read only.
func Write(policy string, exists, exempt bool) error {
	switch strings.ToUpper(policy) {
	case Allow:
		return nil
	case AllowOnce:
		if exists && !exempt {
			return refuse(RuleRedeploy, "the repository does not allow updating assets")
		}
		return nil
	}
	return refuse(RuleReadOnly, "the repository is read only")
}

// lastSegment is the name after the last slash.
func lastSegment(path string) string {
	return path[strings.LastIndex(path, "/")+1:]
}

// MavenMetadata reports whether a maven2 path is maven-metadata.xml or one
// of its checksum files, which ALLOW_ONCE lets through again.
func MavenMetadata(path string) bool {
	base, _ := maven.SplitChecksum(path)
	return maven.IsMetadataPath(base)
}

// LatestTag reports whether a docker path is the manifest of the tag
// latest: v2/<name>/manifests/latest.
func LatestTag(path string) bool {
	name, ok := strings.CutSuffix(path, "/manifests/latest")
	return ok && strings.HasPrefix(name, "v2/") && len(name) > len("v2/")
}

// versionOf is the Maven version a path belongs to: the version of an
// artifact file, or the snapshot version of a snapshot metadata file, after
// one checksum suffix and a signature suffix are taken off.
func versionOf(path string) (string, bool) {
	base, _ := maven.SplitChecksum(path)
	base = strings.TrimSuffix(base, signatureExt)
	if maven.IsMetadataPath(base) {
		dir := strings.TrimSuffix(base, "/"+maven.MetadataName)
		if version := lastSegment(dir); dir != base && strings.HasSuffix(version, snapshotSuffix) {
			return version, true
		}
		return "", false
	}
	if c := maven.CoordinatesOf(base); c != nil {
		return c.Version, true
	}
	return "", false
}

// Version applies the version policy of a maven2 repository: RELEASE
// refuses a snapshot version, SNAPSHOT a release version, MIXED neither. A
// path without a version passes, and so does every path under an empty
// policy.
func Version(policy, path string) error {
	version, ok := versionOf(path)
	if !ok {
		return nil
	}
	snapshot := strings.HasSuffix(version, snapshotSuffix)
	switch strings.ToUpper(policy) {
	case Release:
		if snapshot {
			return refuse(RuleVersion, "the repository takes release versions only, not "+version)
		}
	case Snapshot:
		if !snapshot {
			return refuse(RuleVersion, "the repository takes snapshot versions only, not "+version)
		}
	}
	return nil
}

// MavenPath reports whether a path belongs to the Maven repository layout,
// after one checksum suffix and then a signature suffix are taken off: an
// artifact file <group>/<artifact>/<version>/<artifact>-<version>[-<classifier>].<ext>,
// a maven-metadata.xml below at least one directory, or the
// archetype-catalog.xml at the root.
func MavenPath(path string) bool {
	base, _ := maven.SplitChecksum(path)
	base = strings.TrimSuffix(base, signatureExt)
	switch {
	case base == catalog:
		return true
	case maven.IsMetadataPath(base):
		return strings.Contains(base, "/")
	}
	return maven.CoordinatesOf(base) != nil
}

// Layout applies the layout policy of a maven2 repository: STRICT takes
// Maven paths only, any other policy every path.
func Layout(policy, path string) error {
	if strings.ToUpper(policy) == Strict && !MavenPath(path) {
		return refuse(RuleLayout, path+" is not a path of the Maven repository layout")
	}
	return nil
}

// extension is the last extension of the name of a path, in lower case.
func extension(path string) string {
	name := lastSegment(path)
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}

type signature int

const (
	none signature = iota
	zipFamily
	gzipFile
	xmlDocument
	jsonDocument
)

// signatures are the extensions that imply their first bytes (W8).
var signatures = map[string]signature{
	"jar": zipFamily, "war": zipFamily, "ear": zipFamily, "aar": zipFamily, "zip": zipFamily,
	"gz": gzipFile, "tgz": gzipFile,
	"xml": xmlDocument, "pom": xmlDocument,
	"json": jsonDocument, "module": jsonDocument,
}

// contentTypes are the content types stored for an extension.
var contentTypes = map[string]string{
	"jar": "application/java-archive", "war": "application/java-archive", "ear": "application/java-archive",
	"aar": "application/java-archive", "zip": "application/zip",
	"gz": "application/gzip", "tgz": "application/gzip",
	"xml": "application/xml", "pom": "application/xml",
	"json": "application/json", "module": "application/json",
	"md5": "text/plain", "sha1": "text/plain", "sha256": "text/plain", "sha512": "text/plain", "txt": "text/plain",
	"asc": "application/pgp-signature",
}

// ContentType is the content type stored with a new file: from the table of
// extensions, else what http.DetectContentType finds in its first bytes,
// without parameters.
func ContentType(path string, head []byte) string {
	if t, ok := contentTypes[extension(path)]; ok {
		return t
	}
	detected := http.DetectContentType(head)
	if t, _, err := mime.ParseMediaType(detected); err == nil {
		return t
	}
	return detected
}

var zipStarts = [][]byte{[]byte("PK\x03\x04"), []byte("PK\x05\x06"), []byte("PK\x07\x08")}

// document is the first byte of a text document after a UTF-8 byte order
// mark and white space.
func document(head []byte) byte {
	head = bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	head = bytes.TrimLeft(head, " \t\r\n")
	if len(head) == 0 {
		return 0
	}
	return head[0]
}

// Content is the strict content type validation: the first bytes of a file
// whose extension implies a zip archive, gzip, an XML or a JSON document
// must start that way. A file of any other extension passes.
func Content(path string, head []byte) error {
	var ok bool
	switch signatures[extension(path)] {
	case none:
		return nil
	case zipFamily:
		for _, start := range zipStarts {
			ok = ok || bytes.HasPrefix(head, start)
		}
	case gzipFile:
		ok = bytes.HasPrefix(head, []byte("\x1f\x8b"))
	case xmlDocument:
		ok = document(head) == '<'
	case jsonDocument:
		first := document(head)
		ok = first == '{' || first == '['
	}
	if !ok {
		return refuse(RuleContent, "the content of "+lastSegment(path)+" does not match its extension")
	}
	return nil
}
