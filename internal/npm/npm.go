// Package npm is npm.py of the reference: package and version paths, the
// merge of the package documents of a group, the document built from stored
// tarballs, the tarball URL rewrite and the version order. Documents are
// *pyjson.Object values as pyjson.Decode returns them, so key order and the
// literal form of values stay; pyjson.Dumps writes a result as server.py
// writes it with json.dumps. Nothing here reads a store or an index.
package npm

import (
	"crypto/sha1" // #nosec G505 -- dist.shasum of an npm document is the SHA-1 of the tarball, not a security use.
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"strings"

	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
)

const (
	TarballDirectory    = "-" // <name>/-/<file>.tgz holds the tarballs of a package
	PackageJSON         = "package/package.json"
	MaxPackageJSONBytes = 4 * 1024 * 1024
	MaxNameLength       = 214 // the npm limit for a package name, scope included
)

// MergedKeys are merged key by key; the first member wins a key.
var MergedKeys = [...]string{"versions", "dist-tags", "time"}

// PackagePath is a request path of one package; Version is empty for the
// document itself.
type PackagePath struct {
	Name, Version string
}

func alnum(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

// isSegment matches [A-Za-z0-9][A-Za-z0-9._-]*.
func isSegment(s string) bool {
	if s == "" || !alnum(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !alnum(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// isVersion matches [A-Za-z0-9][A-Za-z0-9.+_-]{0,127}.
func isVersion(s string) bool {
	if s == "" || len(s) > 128 || !alnum(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !alnum(c) && c != '.' && c != '+' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// ParsePath is parse_path: the package of a request path. A tarball path, a
// path under "-/" and a name outside the grammar give false.
func ParsePath(path string) (PackagePath, bool) {
	segments := strings.Split(path, "/")
	scoped := strings.HasPrefix(segments[0], "@")
	head := 1
	if scoped {
		head = 2
	}
	if len(segments) < head || len(segments) > head+1 {
		return PackagePath{}, false
	}
	if scoped && !isSegment(segments[0][1:]) {
		return PackagePath{}, false
	}
	if !isSegment(segments[head-1]) {
		return PackagePath{}, false
	}
	name := strings.Join(segments[:head], "/")
	if len(name) > MaxNameLength { // ASCII only here, so bytes are code points
		return PackagePath{}, false
	}
	if len(segments) == head {
		return PackagePath{Name: name}, true
	}
	if !isVersion(segments[head]) {
		return PackagePath{}, false
	}
	return PackagePath{Name: name, Version: segments[head]}, true
}

// PackageName is package_name: the name of a document path.
func PackageName(path string) (string, bool) {
	found, ok := ParsePath(path)
	if !ok || found.Version != "" {
		return "", false
	}
	return found.Name, true
}

// Unscoped is the name after the scope.
func Unscoped(name string) string {
	return name[strings.LastIndex(name, "/")+1:]
}

func get(o *pyjson.Object, key string) any {
	v, _ := o.Get(key)
	return v
}

// fileName is _file_name: the last path segment of a tarball URL, decoded
// once, or false for a segment that is no file name.
func fileName(url string) (string, bool) {
	core, _, _ := strings.Cut(url, "#")
	core, _, _ = strings.Cut(core, "?")
	found := unquote(core[strings.LastIndex(core, "/")+1:])
	if found == "" || found == "." || found == ".." || strings.ContainsAny(found, "\\/\x00") {
		return "", false
	}
	return found, true
}

// tarballFile is _tarball_file: the file name from the stored URL of a
// version, else the usual name.
func tarballFile(entry any, name, version string) (string, bool) {
	var url any
	if e, ok := entry.(*pyjson.Object); ok {
		if dist, ok := get(e, "dist").(*pyjson.Object); ok {
			url = get(dist, "tarball")
		}
	}
	if u, ok := url.(string); ok && u != "" {
		return fileName(u)
	}
	if version == "" {
		return "", false
	}
	return Unscoped(name) + "-" + version + ".tgz", true
}

// VersionPath is the repository path of the tarball of one version.
type VersionPath struct {
	Version, Path string
}

// TarballPaths is tarball_paths: the path of the tarball of every version,
// in the order of the document. The file name comes from the stored
// dist.tarball; a version without a usable file name is left out.
func TarballPaths(document *pyjson.Object) []VersionPath {
	name, ok := get(document, "name").(string)
	versions, ok2 := get(document, "versions").(*pyjson.Object)
	if !ok || !ok2 {
		return nil
	}
	var out []VersionPath
	for version, entry := range versions.All() {
		if file, ok := tarballFile(entry, name, version); ok {
			out = append(out, VersionPath{version, name + "/" + TarballDirectory + "/" + file})
		}
	}
	return out
}

func shallowCopy(o *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for k, v := range o.All() {
		out.Set(k, v)
	}
	return out
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case *pyjson.Object:
		if x == nil {
			return x
		}
		out := pyjson.NewObject()
		for k, val := range x.All() {
			out.Set(k, deepCopy(val))
		}
		return out
	case []any:
		if x == nil {
			return x
		}
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = deepCopy(val)
		}
		return out
	case *big.Int:
		if x == nil {
			return x
		}
		return new(big.Int).Set(x)
	}
	return v
}

// SelectVersions is select_versions: a copy with the kept versions only, or
// nil when none is left. time loses the dropped versions, dist-tags loses
// the tags that name one, and latest is set to the highest kept version
// when it is gone. A tag whose value is an object or a list is the
// TypeError of Python's set lookup.
func SelectVersions(document *pyjson.Object, keep []string) (*pyjson.Object, error) {
	versions, ok := get(document, "versions").(*pyjson.Object)
	if !ok {
		return nil, nil
	}
	wanted := map[string]bool{}
	for _, k := range keep {
		wanted[k] = true
	}
	kept := pyjson.NewObject()
	dropped := map[string]bool{}
	for version, entry := range versions.All() {
		if wanted[version] {
			kept.Set(version, entry)
		} else {
			dropped[version] = true
		}
	}
	if kept.Len() == 0 {
		return nil, nil
	}
	out := shallowCopy(document)
	out.Set("versions", kept)
	if times, ok := get(document, "time").(*pyjson.Object); ok {
		left := pyjson.NewObject()
		for key, value := range times.All() {
			if !dropped[key] {
				left.Set(key, value)
			}
		}
		out.Set("time", left)
	}
	tags := pyjson.NewObject()
	if stored, ok := get(document, "dist-tags").(*pyjson.Object); ok {
		for tag, version := range stored.All() {
			switch version.(type) {
			case *pyjson.Object:
				return nil, &pyjson.Error{Class: "TypeError", Message: "unhashable type: 'dict'"}
			case []any:
				return nil, &pyjson.Error{Class: "TypeError", Message: "unhashable type: 'list'"}
			}
			if s, ok := version.(string); ok && dropped[s] {
				continue
			}
			tags.Set(tag, version)
		}
	}
	if !tags.Has("latest") {
		latest, err := HighestVersion(kept.Keys())
		if err != nil {
			return nil, err
		}
		tags.Set("latest", latest)
	}
	out.Set("dist-tags", tags)
	return out, nil
}

// MergeDocuments is merge_documents: the documents of the leaf members
// merged, or nil for an empty list. versions, dist-tags and time are merged
// key by key and the first document wins a key; every other field comes
// from the first document. The result shares nothing with the inputs.
func MergeDocuments(documents []*pyjson.Object) *pyjson.Object {
	if len(documents) == 0 {
		return nil
	}
	merged := deepCopy(documents[0]).(*pyjson.Object)
	for _, document := range documents[1:] {
		for _, key := range MergedKeys {
			extra, ok := get(document, key).(*pyjson.Object)
			if !ok {
				continue
			}
			base, ok := get(merged, key).(*pyjson.Object)
			if !ok {
				base = pyjson.NewObject()
				merged.Set(key, base)
			}
			for inner, value := range extra.All() {
				if !base.Has(inner) {
					base.Set(inner, deepCopy(value))
				}
			}
		}
	}
	return merged
}

// Tarball is one stored tarball of a package: file name, bytes and creation
// time in milliseconds.
type Tarball struct {
	FileName  string
	Data      []byte
	CreatedMS int64
}

// DocumentFromTarballs is document_from_tarballs: a document built from the
// stored tarballs of a package, or nil when no tarball holds a readable
// manifest. A version comes from package/package.json of its tarball,
// shasum and integrity are taken over the tarball bytes, and latest is the
// highest version without a pre-release part. The first tarball of a
// version wins. The error is one Python raises: an exception of
// PackageManifest, or the ValueError of a creation time outside the years
// 1 to 9999.
func DocumentFromTarballs(name string, tarballs []Tarball) (*pyjson.Object, error) {
	entries := pyjson.NewObject()
	created := map[string]int64{}
	for _, tb := range tarballs {
		manifest, err := PackageManifest(tb.Data)
		if err != nil {
			return nil, err
		}
		if manifest == nil {
			continue
		}
		version, ok := get(manifest, "version").(string)
		if !ok || !isVersion(version) || entries.Has(version) {
			continue
		}
		entry := shallowCopy(manifest)
		entry.Set("name", name)
		entry.Set("version", version)
		sum := sha1.Sum(tb.Data) // #nosec G401 -- dist.shasum of npm; integrity carries the SHA-512.
		long := sha512.Sum512(tb.Data)
		entry.Set("dist", pyjson.NewObject().
			Set("tarball", name+"/"+TarballDirectory+"/"+tb.FileName).
			Set("shasum", hex.EncodeToString(sum[:])).
			Set("integrity", "sha512-"+base64.StdEncoding.EncodeToString(long[:])))
		entries.Set(version, entry)
		created[version] = tb.CreatedMS
	}
	if entries.Len() == 0 {
		return nil, nil
	}
	order, err := SortVersions(entries.Keys())
	if err != nil {
		return nil, err
	}
	times := pyjson.NewObject()
	for _, version := range order {
		stamp, err := IsoTime(created[version])
		if err != nil {
			return nil, err
		}
		times.Set(version, stamp)
	}
	latest, err := HighestVersion(order)
	if err != nil {
		return nil, err
	}
	versions := pyjson.NewObject()
	first, last := created[order[0]], created[order[0]]
	for _, version := range order {
		versions.Set(version, get(entries, version))
		first, last = min(first, created[version]), max(last, created[version])
	}
	createdStamp, err := IsoTime(first)
	if err != nil {
		return nil, err
	}
	modifiedStamp, err := IsoTime(last)
	if err != nil {
		return nil, err
	}
	timeObject := pyjson.NewObject().Set("created", createdStamp).Set("modified", modifiedStamp)
	for version, stamp := range times.All() {
		timeObject.Set(version, stamp)
	}
	return pyjson.NewObject().
		Set("name", name).
		Set("dist-tags", pyjson.NewObject().Set("latest", latest)).
		Set("versions", versions).
		Set("time", timeObject), nil
}

// RewriteTarballs is rewrite_tarballs: a copy whose every dist.tarball
// points at baseURL, which ends with a slash. A file name that cannot be
// encoded as UTF-8 is the UnicodeEncodeError of urllib.parse.quote.
func RewriteTarballs(document *pyjson.Object, baseURL string) (*pyjson.Object, error) {
	out := deepCopy(document).(*pyjson.Object)
	name, ok := get(out, "name").(string)
	versions, ok2 := get(out, "versions").(*pyjson.Object)
	if !ok || !ok2 {
		return out, nil
	}
	for version, value := range versions.All() {
		entry, ok := value.(*pyjson.Object)
		if !ok {
			continue
		}
		file, ok := tarballFile(entry, name, version)
		if !ok {
			continue
		}
		dist, ok := get(entry, "dist").(*pyjson.Object)
		if !ok {
			dist = pyjson.NewObject()
			entry.Set("dist", dist)
		}
		quoted, err := quote(name + "/" + TarballDirectory + "/" + file)
		if err != nil {
			return nil, err
		}
		dist.Set("tarball", baseURL+quoted)
	}
	return out, nil
}

// VersionObject is version_object: the version object of a version or of
// a dist-tag, else nil. A version whose value is null falls back to the
// tags, as Python's None does.
func VersionObject(document *pyjson.Object, reference string) *pyjson.Object {
	versions, ok := get(document, "versions").(*pyjson.Object)
	if !ok {
		return nil
	}
	entry := get(versions, reference)
	if entry == nil {
		if tags, ok := get(document, "dist-tags").(*pyjson.Object); ok {
			if named, ok := get(tags, reference).(string); ok {
				entry = get(versions, named)
			}
		}
	}
	found, _ := entry.(*pyjson.Object)
	return found
}
