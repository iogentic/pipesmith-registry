package npm

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// The rules that merge the npm write calls into the stored package
// document (npm/cli, branch latest, read 2026-11-18). Nothing here reads or
// writes a store.

// IsVersion is the version grammar of the read path.
func IsVersion(version string) bool { return isVersion(version) }

// IsTag is the grammar of a dist-tag this server takes.
func IsTag(tag string) bool { return tagPattern.MatchString(tag) }

// TarballPath is where the tarball of a version is stored:
// <name>/-/<unscoped name>-<version>.tgz, the name the old product gives it.
func TarballPath(name, version string) string {
	return name + "/" + TarballDirectory + "/" + Unscoped(name) + "-" + version + tarballExt
}

// Refusal is a write the npm rules refuse; the server answers it with 400.
type Refusal struct{ Message string }

func (e *Refusal) Error() string { return e.Message }

func refused(message string) error { return &Refusal{Message: message} }

// Rev is the _rev of a document, "" for none.
func Rev(document *pyjson.Object) string {
	if document == nil {
		return ""
	}
	rev, _ := get(document, "_rev").(string)
	return rev
}

// NextRev is the revision of a document after a change: one more than the
// count of the previous revision, and 32 hex digits of the SHA-256 of the
// document without its revision, in the form CouchDB gives a _rev.
func NextRev(previous string, document []byte) string {
	count := int64(0)
	if head, _, ok := strings.Cut(previous, "-"); ok {
		count, _ = strconv.ParseInt(head, 10, 64)
	}
	sum := sha256.Sum256(document)
	return strconv.FormatInt(max(count, 0)+1, 10) + "-" + hex.EncodeToString(sum[:16])
}

// Integrity are the digests of an integrity string of a version, by
// algorithm in hex: "<algorithm>-<base64>" items separated by white space;
// algorithms other than sha1, sha256 and sha512 are left out.
func Integrity(value string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range strings.Fields(value) {
		algorithm, encoded, ok := strings.Cut(item, "-")
		if !ok {
			return nil, refused("dist.integrity holds " + strconv.Quote(item) + ", which is no <algorithm>-<base64>")
		}
		if algorithm != "sha1" && algorithm != "sha256" && algorithm != "sha512" {
			continue
		}
		encoded, _, _ = strings.Cut(encoded, "?")
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, refused("dist.integrity holds a " + algorithm + " digest that is not base64")
		}
		digest := hex.EncodeToString(raw)
		if old, seen := out[algorithm]; seen && old != digest {
			return nil, refused("dist.integrity holds two " + algorithm + " digests")
		}
		out[algorithm] = digest
	}
	return out, nil
}

// Published is the version a publish document adds: the version entry, its
// tags and its tarball digests as the document gives them.
type Published struct {
	Version string
	Entry   *pyjson.Object
	Tags    []string
	Digests map[string]string // sha1 from dist.shasum and those of dist.integrity, in hex
}

// CheckPublish checks a publish document for the package name of its path
// with the names of its attachments: one version, whose entry names the
// package and the version; dist-tags that name that version; one attachment,
// the tarball of that version.
func CheckPublish(document *pyjson.Object, name string, attachments []string) (*Published, error) {
	for _, key := range []string{"_id", "name"} {
		if value, ok := document.Get(key); ok && value != name {
			return nil, refused(key + " of the document is not " + name + ", the package of the path")
		}
	}
	versions, ok := get(document, "versions").(*pyjson.Object)
	if !ok || versions.Len() != 1 {
		return nil, refused("a publish carries exactly one version")
	}
	version := versions.Keys()[0]
	if !isVersion(version) {
		return nil, refused(strconv.Quote(version) + " is not a version this server takes")
	}
	entry, ok := get(versions, version).(*pyjson.Object)
	if !ok {
		return nil, refused("the version " + version + " is not an object")
	}
	if value, ok := entry.Get("name"); ok && value != name {
		return nil, refused("the name of version " + version + " is not " + name)
	}
	if value, ok := entry.Get("version"); ok && value != version {
		return nil, refused("the version field of " + version + " differs from its key")
	}
	p := &Published{Version: version, Entry: deepCopy(entry).(*pyjson.Object), Digests: map[string]string{}}
	if tags, ok := get(document, "dist-tags").(*pyjson.Object); ok {
		for tag, value := range tags.All() {
			if !IsTag(tag) || value != version {
				return nil, refused("the dist-tag " + strconv.Quote(tag) + " is no tag of version " + version)
			}
			p.Tags = append(p.Tags, tag)
		}
	}
	if len(attachments) != 1 {
		return nil, refused("a publish carries exactly one attachment, the tarball of its version")
	}
	file := attachments[0]
	if file != Unscoped(name)+"-"+version+tarballExt && file != name+"-"+version+tarballExt {
		return nil, refused("the attachment " + strconv.Quote(file) + " is not the tarball of " + name + "@" + version)
	}
	if dist, ok := get(entry, "dist").(*pyjson.Object); ok {
		if shasum, ok := get(dist, "shasum").(string); ok && shasum != "" {
			p.Digests["sha1"] = strings.ToLower(shasum)
		}
		if integrity, ok := get(dist, "integrity").(string); ok {
			digests, err := Integrity(integrity)
			if err != nil {
				return nil, err
			}
			for algorithm, digest := range digests {
				if old, seen := p.Digests[algorithm]; seen && old != digest {
					return nil, refused("dist.shasum and dist.integrity name two SHA-1 digests")
				}
				p.Digests[algorithm] = digest
			}
		}
	}
	return p, nil
}

// MergePublish is the stored document after a publish: the stored one, or a
// new one when there is none, with the version entry, its tags and the time
// of the publish. The entry gets the tarball path relative to the
// repository and the digests of the stored tarball. Other fields of the
// publish document (description, readme, access) replace those of the
// stored document when the version is the latest one after the merge, and
// are added where the stored document lacks them. A document without the
// tag latest gets it for this version. The result shares nothing with the
// inputs.
func MergePublish(stored, document *pyjson.Object, name string, p *Published, sha1Hex, integrity, at string) *pyjson.Object {
	var out *pyjson.Object
	if stored != nil {
		out = deepCopy(stored).(*pyjson.Object)
	} else {
		out = pyjson.NewObject().Set("_id", name).Set("_rev", "").Set("name", name)
	}
	out.Set("_id", name)
	out.Set("name", name)
	entry := deepCopy(p.Entry).(*pyjson.Object)
	dist, ok := get(entry, "dist").(*pyjson.Object)
	if !ok {
		dist = pyjson.NewObject()
	}
	dist.Set("shasum", sha1Hex)
	dist.Set("integrity", integrity)
	dist.Set("tarball", TarballPath(name, p.Version))
	entry.Set("dist", dist)
	versions, ok := get(out, "versions").(*pyjson.Object)
	if !ok {
		versions = pyjson.NewObject()
	}
	versions.Set(p.Version, entry)
	out.Set("versions", versions)
	tags, ok := get(out, "dist-tags").(*pyjson.Object)
	if !ok {
		tags = pyjson.NewObject()
	}
	for _, tag := range p.Tags {
		tags.Set(tag, p.Version)
	}
	if !tags.Has("latest") {
		tags.Set("latest", p.Version)
	}
	out.Set("dist-tags", tags)
	latest := get(tags, "latest") == p.Version
	for key, value := range document.All() {
		switch key {
		case "_id", "_rev", "name", "versions", "dist-tags", "time", "_attachments":
			continue
		}
		if latest || !out.Has(key) {
			out.Set(key, deepCopy(value))
		}
	}
	times, ok := get(out, "time").(*pyjson.Object)
	if !ok {
		times = pyjson.NewObject()
	}
	if !times.Has("created") {
		times.Set("created", at)
	}
	times.Set("modified", at)
	times.Set(p.Version, at)
	out.Set("time", times)
	return out
}

func versionsOf(document *pyjson.Object) *pyjson.Object {
	versions, _ := get(document, "versions").(*pyjson.Object)
	if versions == nil {
		return pyjson.NewObject()
	}
	return versions
}

// RemoveVersions is the stored document without the versions: their
// entries and times go, every tag of one of them goes, and latest moves to
// the highest version left. The result shares nothing with the input.
func RemoveVersions(stored *pyjson.Object, removed []string, at string) (*pyjson.Object, error) {
	out := deepCopy(stored).(*pyjson.Object)
	gone := map[string]bool{}
	for _, v := range removed {
		gone[v] = true
	}
	versions := versionsOf(out)
	for _, v := range removed {
		versions.Delete(v)
	}
	out.Set("versions", versions)
	if times, ok := get(out, "time").(*pyjson.Object); ok {
		for _, v := range removed {
			times.Delete(v)
		}
		times.Set("modified", at)
	}
	tags, ok := get(out, "dist-tags").(*pyjson.Object)
	if !ok {
		tags = pyjson.NewObject()
	}
	for _, tag := range tags.Keys() {
		if value, ok := get(tags, tag).(string); ok && gone[value] {
			tags.Delete(tag)
		}
	}
	if !tags.Has("latest") && versions.Len() > 0 {
		latest, err := HighestVersion(versions.Keys())
		if err != nil {
			return nil, err
		}
		tags.Set("latest", latest)
	}
	out.Set("dist-tags", tags)
	return out, nil
}

// Unpublished are the versions of the stored document that a rewrite of an
// unpublish leaves out. A version the rewrite names that is not stored is
// refused: versions are added by a publish only.
func Unpublished(stored, rewrite *pyjson.Object) ([]string, error) {
	kept, ok := get(rewrite, "versions").(*pyjson.Object)
	if !ok {
		return nil, refused("the rewrite of a document under its revision lists the versions that stay; this one lists none")
	}
	have := versionsOf(stored)
	for _, v := range kept.Keys() {
		if !have.Has(v) {
			return nil, refused("version " + v + " is not published; a version is added by a publish with its tarball")
		}
	}
	var removed []string
	for _, v := range have.Keys() {
		if !kept.Has(v) {
			removed = append(removed, v)
		}
	}
	return removed, nil
}

// SetTag is the stored document with a dist-tag on a published version.
func SetTag(stored *pyjson.Object, tag, version, at string) (*pyjson.Object, error) {
	if !versionsOf(stored).Has(version) {
		return nil, refused("version " + version + " is not published")
	}
	out := deepCopy(stored).(*pyjson.Object)
	tags, ok := get(out, "dist-tags").(*pyjson.Object)
	if !ok {
		tags = pyjson.NewObject()
	}
	tags.Set(tag, version)
	out.Set("dist-tags", tags)
	touch(out, at)
	return out, nil
}

// HasTag reports whether the stored document holds a dist-tag.
func HasTag(stored *pyjson.Object, tag string) bool {
	tags, _ := get(stored, "dist-tags").(*pyjson.Object)
	return tags != nil && tags.Has(tag)
}

// DropTag is the stored document without a dist-tag; latest stays, as
// every package document has it.
func DropTag(stored *pyjson.Object, tag, at string) (*pyjson.Object, error) {
	if tag == "latest" {
		return nil, refused("the tag latest stays; move it to another version instead")
	}
	out := deepCopy(stored).(*pyjson.Object)
	if tags, ok := get(out, "dist-tags").(*pyjson.Object); ok {
		tags.Delete(tag)
	}
	touch(out, at)
	return out, nil
}

// Deprecate is the stored document with the deprecated field of every
// version the update names as the update gives it: a text sets it, an
// empty text removes it. Every other field of the update is left out.
func Deprecate(stored, update *pyjson.Object, at string) (*pyjson.Object, error) {
	named, ok := get(update, "versions").(*pyjson.Object)
	if !ok || named.Len() == 0 {
		return nil, refused("a document without a tarball is taken as a deprecation of its versions; this one names none")
	}
	out := deepCopy(stored).(*pyjson.Object)
	versions := versionsOf(out)
	for v, value := range named.All() {
		entry, ok := get(versions, v).(*pyjson.Object)
		if !ok {
			return nil, refused("version " + v + " is not published; a version is added by a publish with its tarball")
		}
		given, ok := value.(*pyjson.Object)
		if !ok {
			return nil, refused("version " + v + " of the update is not an object")
		}
		message, has := given.Get("deprecated")
		if !has {
			continue
		}
		text, ok := message.(string)
		if !ok {
			return nil, refused("deprecated of version " + v + " is not a text")
		}
		if text == "" {
			entry.Delete("deprecated")
		} else {
			entry.Set("deprecated", text)
		}
	}
	out.Set("versions", versions)
	touch(out, at)
	return out, nil
}

func touch(document *pyjson.Object, at string) {
	times, ok := get(document, "time").(*pyjson.Object)
	if !ok {
		times = pyjson.NewObject()
		document.Set("time", times)
	}
	times.Set("modified", at)
}
