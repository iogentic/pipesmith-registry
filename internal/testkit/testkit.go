// Package testkit writes synthetic blob stores and plain directory trees for
// Go tests. It is a port of tests/proof/fixtures.py and writes the same bytes
// for the same input: tools/vectors/testkit.py checks the tree digests against
// the Python builder.
package testkit

import (
	"crypto/sha1" // #nosec G505 -- SHA-1 is the checksum of the blob format and of uuid5, not a security use.
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultCreatedMS is the creation time of a spec that sets none.
const DefaultCreatedMS int64 = 1_700_000_000_000

// DefaultUnparsable is the default name of AddUnparsable in Python.
const DefaultUnparsable = "unparsable"

// DefaultManifestStore is the default store column of BlobManifest in Python.
const DefaultManifestStore = "demo-store"

// namespace is the UUID namespace of every invented blob id.
var namespace = [16]byte{0x6f, 0x0f, 0x4a, 0x1c, 0x5d, 0x0e, 0x4f, 0x57, 0x9a, 0x55, 0x0a, 0x1b, 0x2c, 0x3d, 0x4e, 0x5f}

// BlobSpec is one blob of a store. NewBlobSpec gives the defaults of the
// Python dataclass; a literal BlobSpec{} has Go's zero values instead.
type BlobSpec struct {
	Repo, Path   string
	Content      []byte
	Scheme       string // "date" or "vol"
	CreatedMS    int64
	Deleted      bool
	CopyOnly     bool    // properties with originalLocation, no bytes file
	NoBytes      bool    // properties without bytes file and without originalLocation
	ContentType  *string // nil writes no content type
	LeadingSlash bool    // blob name written with a leading slash, as recent versions of the old product do
}

// NewBlobSpec returns a spec with the defaults of BlobSpec(repo, path) in
// Python: content "x", scheme "date", DefaultCreatedMS, content type
// application/octet-stream, a leading slash.
func NewBlobSpec(repo, path string) BlobSpec {
	contentType := "application/octet-stream"
	return BlobSpec{
		Repo: repo, Path: path, Content: []byte("x"), Scheme: "date", CreatedMS: DefaultCreatedMS,
		ContentType: &contentType, LeadingSlash: true,
	}
}

// SHA1Hex is the lower case hex SHA-1 of data.
func SHA1Hex(data []byte) string {
	sum := sha1.Sum(data) // #nosec G401 -- the SHA-1 checksum the blob store and Maven expect.
	return hex.EncodeToString(sum[:])
}

// uuid5 is uuid.uuid5 of Python with the namespace of the fixtures.
func uuid5(name string) string {
	h := sha1.New() // #nosec G401 -- uuid5 is defined over SHA-1.
	h.Write(namespace[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)[:16]
	sum[6] = sum[6]&0x0F | 0x50
	sum[8] = sum[8]&0x3F | 0x80
	text := hex.EncodeToString(sum)
	return text[:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:]
}

// BlobUUID is a stable lower case UUID for a spec.
func BlobUUID(s BlobSpec) string {
	return uuid5(strings.Join([]string{s.Repo, s.Path, s.Scheme, strconv.FormatInt(s.CreatedMS, 10), SHA1Hex(s.Content)}, "|"))
}

// javaHash is String.hashCode over the code points, as the Python port
// computes it.
func javaHash(text string) int64 {
	var h uint32
	for _, r := range text {
		h = 31*h + uint32(r) // #nosec G115 -- String.hashCode is arithmetic modulo 2^32, the wrap is intended.
	}
	return int64(int32(h))
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func utc(ms int64) time.Time {
	return time.UnixMilli(ms).UTC()
}

// stem is the directory and file name stem of a blob below content/.
func stem(s BlobSpec) (string, error) {
	ident := BlobUUID(s)
	switch s.Scheme {
	case "vol":
		h := javaHash(ident)
		// abs of Java's remainder, whose sign follows the dividend.
		vol, chap := abs(h)%43+1, abs(h)%47+1
		return fmt.Sprintf("content/vol-%02d/chap-%02d/%s", vol, chap, ident), nil
	case "date":
		return "content/" + utc(s.CreatedMS).Format("2006/01/02/15/04") + "/" + ident, nil
	}
	return "", fmt.Errorf("unknown scheme '%s'", s.Scheme)
}

func mustStem(s BlobSpec) string {
	text, err := stem(s)
	if err != nil {
		panic(err.Error())
	}
	return text
}

// BytesLocation is the location a store reports for the spec: the .bytes
// path relative to the store root. It panics on an unknown scheme, where
// Python raises ValueError.
func BytesLocation(s BlobSpec) string {
	return mustStem(s) + ".bytes"
}

// PropertiesLocation is the .properties path relative to the store root.
func PropertiesLocation(s BlobSpec) string {
	return mustStem(s) + ".properties"
}

func escape(text string, isKey bool) string {
	var b strings.Builder
	index := 0
	for _, r := range text {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == ':' || r == '=' || r == '#' || r == '!':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == ' ':
			if isKey || index == 0 {
				b.WriteString(`\ `)
			} else {
				b.WriteByte(' ')
			}
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20 || r > 0x7E:
			if r >= 0x10000 {
				r -= 0x10000
				fmt.Fprintf(&b, `\u%04X\u%04X`, 0xD800|r>>10, 0xDC00|r&0x3FF)
			} else {
				fmt.Fprintf(&b, `\u%04X`, r)
			}
		default:
			b.WriteRune(r)
		}
		index++
	}
	return b.String()
}

// EncodeProperties is the Java properties format as the old product writes
// it: two comment lines, sorted keys, every character outside printable ASCII
// as a \u escape.
func EncodeProperties(items map[string]string, createdMS int64) []byte {
	when := utc(createdMS)
	lines := []string{
		fmt.Sprintf("#%s,%03d+0000", when.Format("2006-01-02 15:04:05"), (createdMS%1000+1000)%1000),
		"#" + when.Format("Mon Jan 02 15:04:05") + " UTC " + when.Format("2006"),
	}
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	// Byte order of UTF-8 is code point order, the order of sorted().
	slices.Sort(keys)
	for _, key := range keys {
		lines = append(lines, escape(key, true)+"="+escape(items[key], false))
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// SpecProperties are the properties the old product writes for the spec.
func SpecProperties(s BlobSpec) map[string]string {
	name := s.Path
	if s.LeadingSlash {
		name = "/" + name
	}
	items := map[string]string{
		"@BlobStore.blob-name":     name,
		"@BlobStore.created-by":    "demo-user",
		"@BlobStore.created-by-ip": "192.0.2.10",
		"@Bucket.repo-name":        s.Repo,
		"creationTime":             strconv.FormatInt(s.CreatedMS, 10),
		"sha1":                     SHA1Hex(s.Content),
		"size":                     strconv.Itoa(len(s.Content)),
	}
	if s.ContentType != nil {
		items["@BlobStore.content-type"] = *s.ContentType
	}
	if s.Deleted {
		items["deleted"] = "true"
		items["deletedReason"] = "Removing unused asset blob"
		items["deletedDateTime"] = strconv.FormatInt(s.CreatedMS+86_400_000, 10)
		items["softDeletedLocation"] = "2023-11-15T22:13"
	}
	if s.CopyOnly {
		items["originalLocation"] = ""
	}
	return items
}

func write(root, relative string, data []byte) error {
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o600)
}

// MakeBlobStore writes a store below root: metadata.properties, content/
// and reconciliation/, and returns root. On an unknown scheme it stops at
// that spec, as Python does, with the specs before it written.
func MakeBlobStore(root string, blobs []BlobSpec) (string, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return root, err
	}
	if err := write(root, "metadata.properties", EncodeProperties(map[string]string{"type": "file/1"}, DefaultCreatedMS)); err != nil {
		return root, err
	}
	metrics := EncodeProperties(map[string]string{"blobCount": "0", "totalSize": "0"}, DefaultCreatedMS)
	if err := write(root, "demo-node-metrics.properties", metrics); err != nil {
		return root, err
	}
	var days []string
	reconciliation := map[string][]string{}
	for _, s := range blobs {
		base, err := stem(s)
		if err != nil {
			return root, err
		}
		if err := write(root, base+".properties", EncodeProperties(SpecProperties(s), s.CreatedMS)); err != nil {
			return root, err
		}
		if s.CopyOnly || s.NoBytes {
			continue
		}
		if err := write(root, base+".bytes", s.Content); err != nil {
			return root, err
		}
		if s.Scheme == "date" {
			when := utc(s.CreatedMS)
			day := when.Format("2006-01-02")
			if _, ok := reconciliation[day]; !ok {
				days = append(days, day)
			}
			reconciliation[day] = append(reconciliation[day], when.Format("2006-01-02 15:04:05")+","+BlobUUID(s)+",true")
		}
	}
	for _, day := range days {
		if err := write(root, "reconciliation/"+day, []byte(strings.Join(reconciliation[day], "\n")+"\n")); err != nil {
			return root, err
		}
	}
	return root, writeReaderTraps(root)
}

// writeReaderTraps writes a temporary blob and a direct path blob. A reader
// has to skip both.
func writeReaderTraps(root string) error {
	ident := "11111111-2222-4333-8444-555555555555"
	trap := NewBlobSpec("demo-releases", "tmp/trap.txt")
	trap.Content = []byte("temporary")
	direct := NewBlobSpec("demo-releases", "health-check/demo-releases/report.txt")
	direct.Content = []byte("direct path")
	base := "content/directpath/health-check/demo-releases/report"
	for _, f := range []struct {
		rel  string
		data []byte
	}{
		{"content/tmp/tmp$" + ident + "." + ident + ".bytes", trap.Content},
		{"content/tmp/tmp$" + ident + "." + ident + ".properties", EncodeProperties(SpecProperties(trap), DefaultCreatedMS)},
		{base + ".bytes", direct.Content},
		{base + ".properties", EncodeProperties(SpecProperties(direct), DefaultCreatedMS)},
	} {
		if err := write(root, f.rel, f.data); err != nil {
			return err
		}
	}
	return nil
}

// AddUnparsable adds a properties file that is not a blob description and
// returns its location. Python's default name is DefaultUnparsable.
func AddUnparsable(root, name string) (string, error) {
	relative := "content/vol-01/chap-01/" + name + ".properties"
	return relative, write(root, relative, []byte("\xff\xfe\x00 this is not a properties file \x00\x01"))
}

// SkipCaseBlobs returns one live blob and one of each kind a reader has to
// skip (without the unparsable file).
func SkipCaseBlobs() []BlobSpec {
	live := NewBlobSpec("demo-releases", "com/example/live/1.0/live-1.0.jar")
	live.Content = []byte("live bytes")
	jar := "application/java-archive"
	live.ContentType = &jar
	gone := NewBlobSpec("demo-releases", "com/example/gone/1.0/gone-1.0.jar")
	gone.Content, gone.Scheme, gone.Deleted = []byte("deleted bytes"), "vol", true
	copied := NewBlobSpec("demo-releases", "com/example/copy/1.0/copy-1.0.jar")
	copied.Content, copied.CopyOnly = []byte("copy"), true
	missing := NewBlobSpec("demo-releases", "com/example/nobytes/1.0/nobytes-1.0.jar")
	missing.Content, missing.Scheme, missing.NoBytes = []byte("missing"), "vol", true
	return []BlobSpec{live, gone, copied, missing}
}

// MakeTree writes "<repo>/<path>" -> content below root and returns root.
// A Go map has no order, so the files are written in sorted order.
func MakeTree(root string, files map[string][]byte) (string, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return root, err
	}
	for _, relative := range slices.Sorted(maps.Keys(files)) {
		if err := write(root, relative, files[relative]); err != nil {
			return root, err
		}
	}
	return root, nil
}

// ManifestRow is one row of BlobManifest. SHA1 and ContentType nil are
// written empty.
type ManifestRow struct {
	Repo, Path        string
	Size, CreatedMS   int64
	Deleted           bool
	SHA1, ContentType *string
}

var manifestEscapes = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

// BlobManifest is a blob manifest of nine tab separated columns, one line
// per row, without header: store, blob_rel, repo, path, size, created_ms,
// deleted, sha1, content_type. The blob location is a stable invented one
// below vol-01/chap-01.
func BlobManifest(rows []ManifestRow, store string) []byte {
	var b strings.Builder
	for number, row := range rows {
		ident := uuid5(fmt.Sprintf("manifest|%s|%d|%s|%s", store, number, row.Repo, row.Path))
		deleted, sha, contentType := "0", "", ""
		if row.Deleted {
			deleted = "1"
		}
		if row.SHA1 != nil {
			sha = *row.SHA1
		}
		if row.ContentType != nil {
			contentType = *row.ContentType
		}
		b.WriteString(strings.Join([]string{
			store, "vol-01/chap-01/" + ident, manifestEscapes.Replace(row.Repo), manifestEscapes.Replace(row.Path),
			strconv.FormatInt(row.Size, 10), strconv.FormatInt(row.CreatedMS, 10), deleted, sha,
			manifestEscapes.Replace(contentType),
		}, "\t") + "\n")
	}
	return []byte(b.String())
}

// TreeDigest is SHA-256 over the sorted relative paths, kinds, sizes and
// contents below root. Symlinks count by target. A root that is missing or
// cannot be read gives the digest of nothing, as os.walk yields nothing.
func TreeDigest(root string) (string, error) {
	var lines []string
	top := root
	if info, err := os.Lstat(root); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		// os.walk lists a root that is a symlink; WalkDir does so with a
		// trailing separator.
		top = root + string(filepath.Separator)
	}
	err := filepath.WalkDir(top, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// os.walk skips a directory it cannot list.
			if d == nil || d.IsDir() {
				return nil
			}
			return err
		}
		if path == top {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			lines = append(lines, "L "+rel+" -> "+target)
		case d.IsDir():
			lines = append(lines, "D "+rel)
		default:
			data, err := os.ReadFile(path) // #nosec G122 G304 -- test helper that hashes a tree the test itself wrote.
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			lines = append(lines, fmt.Sprintf("F %s %d %s", rel, len(data), hex.EncodeToString(sum[:])))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

// MetadataXML is a maven-metadata.xml for an artifact. latest is the last
// version, release the last that is not a snapshot.
func MetadataXML(group, artifact string, versions []string, lastUpdated string) []byte {
	var releases []string
	for _, v := range versions {
		if !strings.HasSuffix(v, "-SNAPSHOT") {
			releases = append(releases, v)
		}
	}
	parts := []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		"<metadata>",
		"  <groupId>" + group + "</groupId>",
		"  <artifactId>" + artifact + "</artifactId>",
		"  <versioning>",
	}
	if len(versions) > 0 {
		parts = append(parts, "    <latest>"+versions[len(versions)-1]+"</latest>")
	}
	if len(releases) > 0 {
		parts = append(parts, "    <release>"+releases[len(releases)-1]+"</release>")
	}
	parts = append(parts, "    <versions>")
	for _, v := range versions {
		parts = append(parts, "      <version>"+v+"</version>")
	}
	parts = append(parts, "    </versions>", "    <lastUpdated>"+lastUpdated+"</lastUpdated>", "  </versioning>", "</metadata>")
	return []byte(strings.Join(parts, "\n") + "\n")
}
