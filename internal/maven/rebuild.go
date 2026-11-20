package maven

import (
	"archive/zip"
	"cmp"
	"crypto/md5"  // #nosec G501 -- the MD5 checksum file of maven-metadata.xml, not a security use.
	"crypto/sha1" // #nosec G505 -- the SHA-1 checksum file of maven-metadata.xml, not a security use.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The rebuild of maven-metadata.xml from the files a repository holds
// (phase 4 plan, task 10 and W15): the artifact level with the versions,
// the snapshot version level with the newest build of each classifier and
// extension, and the plugin group level with the plugins of the group,
// each with its checksum files. The elements are those of the repository
// metadata model (https://maven.apache.org/ref/3.9.9/maven-repository-metadata/repository-metadata.html,
// read 2026-11-18); the documents are written as the merge of a group
// writes them, the elements in the order of the files the Maven client
// deploys. Every value comes from the files: the same files give the same
// bytes, so a second run changes nothing.

const (
	// pomLimit and descriptorLimit bound what the rebuild reads of a POM
	// and of the plugin descriptor of a jar; a larger one counts as none.
	pomLimit        = 1 << 20
	descriptorLimit = 4 << 20
	descriptorName  = "META-INF/maven/plugin.xml"
	pluginPackaging = "maven-plugin"
	snapshotModel   = "1.1.0" // modelVersion of a document with snapshotVersions
	stampLayout     = "20060102150405"
)

// File is a path of a repository with the creation time of its blob.
type File struct {
	Path      string
	CreatedMS int64
}

// Content is the bytes of a file, read at offsets.
type Content interface {
	io.ReaderAt
	io.Closer
}

// Source is what a rebuild reads: the paths of the repository, and the
// bytes of one of them; Open gives a nil Content for a path that is gone.
type Source struct {
	Files []File
	Open  func(path string) (Content, int64, error)
}

// Scope limits a rebuild to a group, an artifact of it and a base version
// of that; an empty part takes everything below the parts before it.
type Scope struct {
	GroupID, ArtifactID, BaseVersion string
}

// Dir is the directory of the scope, "" for the whole repository. An
// artifact needs its group and a version its artifact.
func (s Scope) Dir() (string, error) {
	switch {
	case s.ArtifactID != "" && s.GroupID == "":
		return "", errors.New("an artifact needs its group")
	case s.BaseVersion != "" && s.ArtifactID == "":
		return "", errors.New("a version needs its artifact")
	}
	parts := strings.Split(s.GroupID, ".")
	if s.GroupID == "" {
		parts = nil
	}
	for _, part := range []string{s.ArtifactID, s.BaseVersion} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\") {
			return "", fmt.Errorf("%q is no part of a Maven path", part)
		}
	}
	return strings.Join(parts, "/"), nil
}

// Document is a file the rebuild writes: a maven-metadata.xml or one of
// its checksum files.
type Document struct {
	Path string
	Data []byte
}

// Rebuilt is the outcome of a rebuild: the documents the repository should
// hold, in path order, and the metadata paths in scope with their checksum
// files that no file below their directory supports any more.
type Rebuilt struct {
	Documents []Document
	Stale     []string
}

// artifactFile is a file of the layout <group>/<artifact>/<version>/<name>.
type artifactFile struct {
	File
	group, artifact, version string // the group path with slashes, the artifact id, the version directory
	classifier, extension    string
	timestamp                string // yyyyMMdd.HHmmss of a timestamped snapshot file, else ""
	build                    int
}

// value is the version of the file: the timestamped one of a snapshot.
func (f *artifactFile) value() string {
	if f.timestamp == "" {
		return f.version
	}
	return strings.TrimSuffix(f.version, snapshotSuffix) + "-" + f.timestamp + "-" + strconv.Itoa(f.build)
}

// newerBuild orders the files of a snapshot version by build number, then
// time stamp, then creation time.
func newerBuild(a, b *artifactFile) int {
	return cmp.Or(cmp.Compare(a.build, b.build), cmp.Compare(a.timestamp, b.timestamp), cmp.Compare(a.CreatedMS, b.CreatedMS))
}

var stampedName = regexp.MustCompile(`^([0-9]{8}\.[0-9]{6})-([0-9]{1,9})`)

// parseFile reads a file of the layout; nil for metadata, checksum and
// other files.
func parseFile(f File) *artifactFile {
	c := CoordinatesOf(f.Path)
	if c == nil {
		return nil
	}
	parts := strings.Split(f.Path, "/")
	n := len(parts)
	a := &artifactFile{File: f, group: strings.Join(parts[:n-3], "/"), artifact: parts[n-3], version: parts[n-2], extension: c.Extension}
	if c.Classifier != nil {
		a.classifier = *c.Classifier
	}
	if base, ok := strings.CutSuffix(a.version, snapshotSuffix); ok {
		if rest, ok := strings.CutPrefix(parts[n-1], a.artifact+"-"+base+"-"); ok {
			if m := stampedName.FindStringSubmatch(rest); m != nil {
				a.timestamp = m[1]
				a.build, _ = strconv.Atoi(m[2]) // nine digits at most
			}
		}
	}
	return a
}

// artifact holds the files of one artifact by version directory.
type artifact struct {
	group, id string
	versions  map[string][]*artifactFile
}

func stamp(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(stampLayout)
}

func groupID(groupPath string) string {
	return strings.ReplaceAll(groupPath, "/", ".")
}

// newestMS is the latest creation time of files.
func newestMS(files []*artifactFile) int64 {
	var newest int64
	for _, f := range files {
		newest = max(newest, f.CreatedMS)
	}
	return newest
}

// artifactLevel is the document of <group>/<artifact>: the versions in
// version order, the greatest as latest, the greatest release as release.
func artifactLevel(a *artifact) (*element, error) {
	versions := make([]string, 0, len(a.versions))
	var all []*artifactFile
	for v, files := range a.versions {
		versions = append(versions, v)
		all = append(all, files...)
	}
	slices.Sort(versions)
	if err := sortVersions(versions); err != nil {
		return nil, fmt.Errorf("the versions of %s:%s: %w", groupID(a.group), a.id, err)
	}
	release := ""
	for _, v := range versions {
		if !strings.HasSuffix(v, snapshotSuffix) {
			release = v
		}
	}
	root := &element{tag: "metadata"}
	root.addLeaf("groupId", groupID(a.group))
	root.addLeaf("artifactId", a.id)
	versioning := root.addNode("versioning")
	versioning.addLeaf("latest", versions[len(versions)-1])
	versioning.addLeaf("release", release)
	list := versioning.addNode("versions")
	for _, v := range versions {
		list.addLeaf("version", v)
	}
	versioning.addLeaf("lastUpdated", stamp(newestMS(all)))
	return root, nil
}

// snapshotLevel is the document of <group>/<artifact>/<version> of a
// snapshot version with timestamped files: the newest build, and for each
// classifier and extension its newest file; nil without timestamped files.
func snapshotLevel(a *artifact, version string) *element {
	var stamped []*artifactFile
	for _, f := range a.versions[version] {
		if f.timestamp != "" {
			stamped = append(stamped, f)
		}
	}
	if len(stamped) == 0 {
		return nil
	}
	newest := slices.MaxFunc(stamped, newerBuild)
	latest := map[[2]string]*artifactFile{}
	for _, f := range stamped {
		key := [2]string{f.classifier, f.extension}
		if old := latest[key]; old == nil || newerBuild(f, old) > 0 {
			latest[key] = f
		}
	}
	keys := make([][2]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(x, y [2]string) int { return cmp.Or(strings.Compare(x[0], y[0]), strings.Compare(x[1], y[1])) })
	root := &element{tag: "metadata", attrs: []attr{{"modelVersion", snapshotModel}}}
	root.addLeaf("groupId", groupID(a.group))
	root.addLeaf("artifactId", a.id)
	root.addLeaf("version", version)
	versioning := root.addNode("versioning")
	snap := versioning.addNode("snapshot")
	snap.addLeaf("timestamp", newest.timestamp)
	snap.addLeaf("buildNumber", strconv.Itoa(newest.build))
	versioning.addLeaf("lastUpdated", stamp(newestMS(stamped)))
	list := versioning.addNode("snapshotVersions")
	for _, key := range keys {
		f := latest[key]
		entry := list.addNode("snapshotVersion")
		entry.addLeaf("classifier", f.classifier)
		entry.addLeaf("extension", f.extension)
		entry.addLeaf("value", f.value())
		entry.addLeaf("updated", stamp(f.CreatedMS))
	}
	return root
}

// readAll is at most limit bytes of a file; nil when it is gone or larger.
func readAll(src Source, p string, limit int64) ([]byte, error) {
	content, size, err := src.Open(p)
	if err != nil || content == nil {
		return nil, err
	}
	defer content.Close()
	if size > limit {
		return nil, nil
	}
	return io.ReadAll(io.NewSectionReader(content, 0, size))
}

type pomFields struct {
	Packaging string `xml:"packaging"`
	Name      string `xml:"name"`
}

type descriptorFields struct {
	GoalPrefix string `xml:"goalPrefix"`
}

// The default goal prefix of a plugin without one in its descriptor: the
// artifact id without the words maven and plugin, as the Maven plugin
// descriptor derives it (maven-plugin-plugin is "plugin").
var (
	mavenWord  = regexp.MustCompile(`-?maven-?`)
	pluginWord = regexp.MustCompile(`-?plugin-?`)
)

func derivedPrefix(artifactID string) string {
	if artifactID == "maven-plugin-plugin" {
		return "plugin"
	}
	return pluginWord.ReplaceAllString(mavenWord.ReplaceAllString(artifactID, ""), "")
}

// maxJarEntries and maxJarDirectory bound the central directory of a jar
// that the rebuild reads for its plugin descriptor (N4): zip.NewReader holds
// every entry of it in memory. A jar beyond either counts as no archive.
var (
	maxJarEntries   uint64 = 100_000
	maxJarDirectory int64  = 32 << 20
)

// goalPrefix is the goalPrefix of the plugin descriptor in a jar, "" when
// the jar has none or is no archive.
func goalPrefix(src Source, jar string) (string, error) {
	content, size, err := src.Open(jar)
	if err != nil || content == nil {
		return "", err
	}
	defer content.Close()
	entries, directory, ok := jarDirectory(content, size)
	if !ok || entries > maxJarEntries || directory > uint64(maxJarDirectory) { // #nosec G115 -- maxJarDirectory is a positive bound.
		return "", nil // no archive this rebuild reads: the prefix of the artifact id
	}
	archive, err := zip.NewReader(content, size)
	if err != nil {
		return "", nil // no archive: the prefix of the artifact id
	}
	for _, f := range archive.File {
		if f.Name != descriptorName || f.UncompressedSize64 > descriptorLimit {
			continue
		}
		r, err := f.Open()
		if err != nil {
			return "", nil
		}
		data, err := io.ReadAll(io.LimitReader(r, descriptorLimit))
		_ = r.Close() // an entry of the archive in memory; the read error is the one that counts
		var d descriptorFields
		if err != nil || xml.Unmarshal(data, &d) != nil {
			return "", nil
		}
		return strings.TrimSpace(d.GoalPrefix), nil
	}
	return "", nil
}

// The end records of a zip archive (APPNOTE.TXT 4.3.14 to 4.3.16).
const (
	endSignature       = 0x06054b50
	endLength          = 22
	endSearch          = endLength + 0xffff // the record and its longest comment
	end64Signature     = 0x06064b50
	end64Length        = 56
	locatorSignature   = 0x07064b50
	locatorLength      = 20
	end64NumberEntries = 32
	end64DirectorySize = 40
)

// jarDirectory reads the end records of a zip archive, as zip.NewReader
// finds them, and gives the number of entries and the size in bytes of its
// central directory; ok is false for no archive. It reads at most the last
// 65,557 bytes and one ZIP64 record.
func jarDirectory(r io.ReaderAt, size int64) (entries, directory uint64, ok bool) {
	tail := min(size, endSearch)
	if tail < endLength {
		return 0, 0, false
	}
	buf := make([]byte, tail)
	if _, err := r.ReadAt(buf, size-tail); err != nil {
		return 0, 0, false
	}
	at := -1
	for i := len(buf) - endLength; i >= 0; i-- {
		if binary.LittleEndian.Uint32(buf[i:]) == endSignature && int(binary.LittleEndian.Uint16(buf[i+20:]))+endLength+i <= len(buf) {
			at = i
			break
		}
	}
	if at < 0 {
		return 0, 0, false
	}
	record := buf[at:]
	entries = uint64(binary.LittleEndian.Uint16(record[10:]))
	directory = uint64(binary.LittleEndian.Uint32(record[12:]))
	offset := binary.LittleEndian.Uint32(record[16:])
	if entries != 0xffff && directory != 0xffffffff && offset != 0xffffffff {
		return entries, directory, true
	}
	position := size - tail + int64(at) - locatorLength
	if position < 0 {
		return entries, directory, true
	}
	locator := make([]byte, locatorLength)
	if _, err := r.ReadAt(locator, position); err != nil || binary.LittleEndian.Uint32(locator) != locatorSignature {
		return entries, directory, true // no ZIP64 locator: zip.NewReader takes the record as it is
	}
	end64At := binary.LittleEndian.Uint64(locator[8:])
	if end64At > uint64(size) { // #nosec G115 -- size is the length of a file, not negative.
		return 0, 0, false
	}
	end64 := make([]byte, end64Length)
	if _, err := r.ReadAt(end64, int64(end64At)); err != nil || binary.LittleEndian.Uint32(end64) != end64Signature { // #nosec G115 -- end64At is at most size.
		return 0, 0, false
	}
	return binary.LittleEndian.Uint64(end64[end64NumberEntries:]), binary.LittleEndian.Uint64(end64[end64DirectorySize:]), true
}

type plugin struct{ name, prefix, artifactID string }

// pluginOf is the plugin entry of an artifact whose newest version has a
// POM with the packaging maven-plugin, nil for any other artifact.
func pluginOf(src Source, a *artifact) (*plugin, error) {
	versions := make([]string, 0, len(a.versions))
	for v := range a.versions {
		versions = append(versions, v)
	}
	slices.Sort(versions)
	if err := sortVersions(versions); err != nil {
		return nil, fmt.Errorf("the versions of %s:%s: %w", groupID(a.group), a.id, err)
	}
	files := a.versions[versions[len(versions)-1]]
	var pom, jar *artifactFile
	for _, f := range files {
		if f.classifier != "" {
			continue
		}
		switch f.extension {
		case "pom":
			if pom == nil || newerBuild(f, pom) > 0 {
				pom = f
			}
		case "jar":
			if jar == nil || newerBuild(f, jar) > 0 {
				jar = f
			}
		}
	}
	if pom == nil {
		return nil, nil
	}
	data, err := readAll(src, pom.Path, pomLimit)
	if err != nil || data == nil {
		return nil, err
	}
	var fields pomFields
	if xml.Unmarshal(data, &fields) != nil || strings.TrimSpace(fields.Packaging) != pluginPackaging {
		return nil, nil
	}
	p := &plugin{name: strings.TrimSpace(fields.Name), artifactID: a.id}
	if jar != nil {
		if p.prefix, err = goalPrefix(src, jar.Path); err != nil {
			return nil, err
		}
	}
	if p.prefix == "" {
		p.prefix = derivedPrefix(a.id)
	}
	return p, nil
}

// inScope reports a directory at or below the directory of the scope.
func inScope(dir, scope string) bool {
	return scope == "" || dir == scope || strings.HasPrefix(dir, scope+"/")
}

var checksumHashes = map[string]func() hash.Hash{
	"md5": md5.New, "sha1": sha1.New, "sha256": sha256.New, "sha512": sha512.New, // #nosec G401 -- see the imports.
}

// Rebuild writes the metadata of the files of src in scope.
func Rebuild(src Source, scope Scope) (*Rebuilt, error) {
	dir, err := scope.Dir()
	if err != nil {
		return nil, err
	}
	artifacts := map[[2]string]*artifact{}
	held := map[string]bool{}     // every path in scope
	metadata := map[string]bool{} // the maven-metadata.xml paths in scope
	occupied := map[string]bool{} // the directories that hold an artifact file, and their parents
	for _, f := range src.Files {
		if !inScope(path.Dir(f.Path), dir) {
			continue
		}
		held[f.Path] = true
		if IsMetadataPath(f.Path) {
			metadata[f.Path] = true
		}
		a := parseFile(f)
		if a == nil {
			continue
		}
		key := [2]string{a.group, a.artifact}
		if artifacts[key] == nil {
			artifacts[key] = &artifact{group: a.group, id: a.artifact, versions: map[string][]*artifactFile{}}
		}
		artifacts[key].versions[a.version] = append(artifacts[key].versions[a.version], a)
		for d := path.Dir(f.Path); d != "." && !occupied[d]; d = path.Dir(d) {
			occupied[d] = true
		}
	}
	documents := map[string]*element{}
	plugins := map[string][]plugin{} // by group path
	keys := make([][2]string, 0, len(artifacts))
	for key := range artifacts {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(x, y [2]string) int { return cmp.Or(strings.Compare(x[0], y[0]), strings.Compare(x[1], y[1])) })
	for _, key := range keys {
		a := artifacts[key]
		base := a.group + "/" + a.id
		if inScope(base, dir) {
			root, err := artifactLevel(a)
			if err != nil {
				return nil, err
			}
			documents[base+"/"+MetadataName] = root
		}
		for version := range a.versions {
			if strings.HasSuffix(version, snapshotSuffix) && inScope(base+"/"+version, dir) {
				if root := snapshotLevel(a, version); root != nil {
					documents[base+"/"+version+"/"+MetadataName] = root
				}
			}
		}
		if inScope(a.group, dir) {
			p, err := pluginOf(src, a)
			if err != nil {
				return nil, err
			}
			if p != nil {
				plugins[a.group] = append(plugins[a.group], *p)
			}
		}
	}
	for group, list := range plugins {
		slices.SortFunc(list, func(x, y plugin) int { return strings.Compare(x.artifactID, y.artifactID) })
		p := group + "/" + MetadataName
		root := documents[p]
		if root == nil {
			root = &element{tag: "metadata"}
			documents[p] = root
		}
		node := root.addNode("plugins")
		for _, entry := range list {
			e := node.addNode("plugin")
			e.addLeaf("name", entry.name)
			e.addLeaf("prefix", entry.prefix)
			e.addLeaf("artifactId", entry.artifactID)
		}
	}
	out := &Rebuilt{}
	for p, root := range documents {
		data := serialize(root)
		out.Documents = append(out.Documents, Document{Path: p, Data: data})
		for _, algorithm := range checksumSuffixes {
			if algorithm == "md5" || algorithm == "sha1" || held[p+"."+algorithm] {
				h := checksumHashes[algorithm]()
				h.Write(data)
				out.Documents = append(out.Documents, Document{Path: p + "." + algorithm, Data: []byte(hex.EncodeToString(h.Sum(nil)))})
			}
		}
	}
	for p := range metadata {
		if documents[p] != nil || occupied[path.Dir(p)] {
			continue
		}
		out.Stale = append(out.Stale, p)
		for _, algorithm := range checksumSuffixes {
			if held[p+"."+algorithm] {
				out.Stale = append(out.Stale, p+"."+algorithm)
			}
		}
	}
	slices.SortFunc(out.Documents, func(x, y Document) int { return strings.Compare(x.Path, y.Path) })
	slices.Sort(out.Stale)
	return out, nil
}
