package stores

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mirosso25/pipesmith-registry/internal/blobprops"
	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/manifest"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
)

// Store kinds.
const (
	KindBlobStore = "nexus-file"
	KindDirectory = "directory"
)

// Layout and limits of the stores.
const (
	ContentDir         = "content"
	PropertiesSuffix   = ".properties"
	BytesSuffix        = ".bytes"
	MaxPropertiesBytes = 1 << 20
	MaxSHA1FileBytes   = 1024
	MaxManifestBytes   = 4 * 1024 * 1024
	// ManifestsDir holds the lookup tables of blob manifests below the data
	// directory (MANIFESTS_DIR of the Python config module).
	ManifestsDir = "manifests"
)

// Docker and OCI media types.
const (
	DockerConfig        = "application/vnd.docker.container.image.v1+json"
	DockerManifest      = "application/vnd.docker.distribution.manifest.v2+json"
	DockerSchema1Signed = "application/vnd.docker.distribution.manifest.v1+prettyjws"
	OCIIndex            = "application/vnd.oci.image.index.v1+json"
	OCIManifest         = "application/vnd.oci.image.manifest.v1+json"
)

var skippedContentDirs = map[string]bool{"tmp": true, "directpath": true}

const loggerName = "pipesmith_registry.stores"

func warn(format string, args ...any) {
	slog.Warn(fmt.Sprintf(format, args...), "logger", loggerName)
}

// NotFoundError is a FileNotFoundError of the Python module. It matches
// fs.ErrNotExist.
type NotFoundError struct {
	Message string
}

func (e *NotFoundError) Error() string { return e.Message }

func (e *NotFoundError) Is(target error) bool { return target == fs.ErrNotExist }

func notFound(format string, args ...any) error {
	return &NotFoundError{Message: fmt.Sprintf(format, args...)}
}

// Error is another exception of the Python module, named by Class:
// RuntimeError and ValueError when a store is made, and OverflowError or
// UnicodeEncodeError for a live entry the index of the Python server cannot
// store, which fails its scan.
type Error struct {
	Class, Message string
}

func (e *Error) Error() string { return e.Message }

// ErrNoManifestLookup is the error of a directory store with a blob manifest
// while OpenManifestLookup is nil.
var ErrNoManifestLookup = errors.New("no blob manifest lookup is set")

// ManifestRow is the newest live row of a blob manifest for a path.
type ManifestRow struct {
	Repo, Path        string
	Size              int64
	SHA1, ContentType *string
	CreatedMS         int64
}

// ManifestLookup answers rows of a blob manifest.
type ManifestLookup interface {
	// Get returns nil, nil when the manifest has no live row for the path.
	Get(repo, path string) (*ManifestRow, error)
	Close() error
}

// OpenManifestLookup opens the lookup of the manifest at source, whose table
// lives in cacheDir: internal/manifest by default. Tests may replace it;
// while it is nil, a directory store with a manifest fails its walk with
// ErrNoManifestLookup.
var OpenManifestLookup = openManifestLookup

func openManifestLookup(source, cacheDir string) (ManifestLookup, error) {
	l, err := manifest.Open(source, cacheDir)
	if err != nil {
		return nil, manifestError(err)
	}
	return manifestLookup{l}, nil
}

type manifestLookup struct {
	l *manifest.Lookup
}

func (m manifestLookup) Get(repo, path string) (*ManifestRow, error) {
	row, err := m.l.Get(repo, path)
	if err != nil {
		return nil, manifestError(err)
	}
	if row == nil {
		return nil, nil
	}
	return &ManifestRow{Repo: row.Repo, Path: row.Path, Size: row.Size, SHA1: row.SHA1,
		ContentType: row.ContentType, CreatedMS: row.CreatedMS}, nil
}

func (m manifestLookup) Close() error { return m.l.Close() }

// manifestError gives an error of the manifest the type this package uses
// for the same Python exception, and an OSError its Python text.
func manifestError(err error) error {
	var me *manifest.Error
	var pathErr *fs.PathError
	switch {
	case errors.As(err, &me) && me.Class == "FileNotFoundError":
		return &NotFoundError{Message: me.Message}
	case errors.As(err, &me):
		return &Error{Class: me.Class, Message: me.Message}
	case errors.As(err, &pathErr):
		return osError(err)
	}
	return err
}

type base struct {
	name, root, realRoot string
}

func newBase(name, root string) (base, error) {
	root = purePath(root)
	real, err := resolveRoot(root)
	if err != nil {
		return base{}, err
	}
	return base{name: name, root: root, realRoot: real}, nil
}

func (b *base) Name() string { return b.name }

func (b *base) Root() string { return b.root }

// Open opens a location of an entry, read only.
func (b *base) Open(location string) (*os.File, error) {
	real, err := b.resolve(location)
	if err != nil {
		return nil, err
	}
	return os.Open(real) // #nosec G304 -- resolve gave the real path of a regular file inside the resolved store root.
}

// resolve is the real path of a regular file inside the root.
func (b *base) resolve(location string) (string, error) {
	if location == "" || strings.Contains(location, "\x00") {
		return "", notFound("no file at %s", repr(location))
	}
	real, err := realpath(joinPath(b.realRoot, location), true)
	if err != nil {
		return "", notFound("no file at %s", repr(location))
	}
	if !within(real, b.realRoot) {
		return "", notFound("%s is outside the store root", repr(location))
	}
	// Stat before open: opening a FIFO would block.
	info, err := os.Stat(real)
	if err != nil {
		return "", notFound("no file at %s", repr(location))
	}
	if !info.Mode().IsRegular() {
		return "", notFound("%s is not a regular file", repr(location))
	}
	return real, nil
}

// requireRoot refuses a missing root: it must not look like an empty store,
// as a scan would drop every row.
func (b *base) requireRoot() error {
	ok, err := isDir(b.root)
	if err != nil {
		return err
	}
	if !ok {
		return notFound("store root %s is not a directory", b.root)
	}
	return nil
}

// BlobStore is a file blob store: pairs of <uuid>.properties and
// <uuid>.bytes below content/.
type BlobStore struct {
	base
}

// NewBlobStore returns the file blob store at root.
func NewBlobStore(name, root string) (*BlobStore, error) {
	b, err := newBase(name, root)
	if err != nil {
		return nil, err
	}
	return &BlobStore{base: b}, nil
}

func (s *BlobStore) Kind() string { return KindBlobStore }

// Entries walks content/ depth first in name order, the files of a
// directory before its subdirectories, and skips content/tmp and
// content/directpath.
func (s *BlobStore) Entries(counts *ScanCounts, yield func(Entry) error) error {
	if counts == nil {
		counts = &ScanCounts{}
	}
	if err := s.requireRoot(); err != nil {
		return err
	}
	content := joinPath(s.root, ContentDir)
	ok, err := isDir(content)
	if err != nil {
		return err
	}
	if !ok {
		return notFound("store root %s has no %s/ directory", s.root, ContentDir)
	}
	stack := []string{""}
	for len(stack) > 0 {
		rel := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		directory := content
		if rel != "" {
			directory = pyJoin(content, rel)
		}
		listing, err := listDir(directory, rel == "")
		if err != nil {
			return err
		}
		files := map[string]bool{}
		var subdirs []string
		for _, item := range listing {
			switch kind := item.Type(); {
			case kind.IsDir():
				if rel != "" || !skippedContentDirs[item.Name()] {
					subdirs = append(subdirs, item.Name())
				}
			case kind.IsRegular():
				files[item.Name()] = true
			}
		}
		slices.SortFunc(subdirs, fsCompare)
		for i := len(subdirs) - 1; i >= 0; i-- {
			if rel != "" {
				stack = append(stack, rel+"/"+subdirs[i])
			} else {
				stack = append(stack, subdirs[i])
			}
		}
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		slices.SortFunc(names, fsCompare)
		for _, name := range names {
			if !strings.HasSuffix(name, PropertiesSuffix) {
				continue
			}
			stem := strings.TrimSuffix(name, PropertiesSuffix)
			entry, err := s.entry(pyJoin(directory, name), rel, stem, files[stem+BytesSuffix], counts)
			if err != nil {
				return err
			}
			if entry != nil {
				if err := yield(*entry); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *BlobStore) entry(propertiesPath, rel, stem string, hasBytes bool, counts *ScanCounts) (*Entry, error) {
	data, err := readUpTo(propertiesPath, MaxPropertiesBytes+1)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // removed by a compact run during the walk
		}
		warn("cannot read %s: %s", propertiesPath, osErrorText(err))
		counts.Unparsable++
		return nil, nil
	}
	var props *blobprops.BlobProps
	if len(data) <= MaxPropertiesBytes {
		if props, err = blobprops.Parse(data); err != nil {
			return nil, err
		}
	}
	switch {
	case props == nil:
		counts.Unparsable++
		return nil, nil
	case props.IsCopy:
		counts.Copies++
		return nil, nil
	case props.Deleted:
		counts.Deleted++
		return nil, nil
	case !hasBytes:
		counts.NoBytes++
		return nil, nil
	}
	counts.Live++
	directory := ContentDir
	if rel != "" {
		directory = ContentDir + "/" + rel
	}
	location := directory + "/" + stem + BytesSuffix
	// The Python index cannot bind these values and fails the scan there.
	if !utf8.ValidString(location) {
		return nil, encodeError(location)
	}
	if props.Overflow {
		return nil, &Error{Class: "OverflowError", Message: "Python int too large to convert to SQLite INTEGER"}
	}
	return &Entry{
		Repo:        props.Repo,
		Path:        props.Path,
		Size:        props.Size,
		SHA1:        props.SHA1,
		ContentType: props.ContentType,
		CreatedMS:   props.CreatedMS,
		Location:    location,
	}, nil
}

// DirectoryStore is a plain tree <root>/<repo>/<path>. It can name a blob
// manifest for SHA1, content type and creation time.
type DirectoryStore struct {
	base
	repos              []string
	manifest, cacheDir *string
}

// NewDirectoryStore returns the tree at root. repos limits the walk to these
// top level names; nil means no limit. A manifest needs cacheDir for its
// lookup table.
func NewDirectoryStore(name, root string, repos []string, manifest, cacheDir *string) (*DirectoryStore, error) {
	b, err := newBase(name, root)
	if err != nil {
		return nil, err
	}
	s := &DirectoryStore{base: b}
	if repos != nil {
		s.repos = append([]string{}, repos...)
	}
	if manifest != nil && cacheDir == nil {
		return nil, &Error{Class: "ValueError",
			Message: fmt.Sprintf("store %s: a manifest needs a directory for its lookup table", name)}
	}
	if manifest != nil {
		m, c := purePath(*manifest), purePath(*cacheDir)
		s.manifest, s.cacheDir = &m, &c
	}
	return s, nil
}

func (s *DirectoryStore) Kind() string { return KindDirectory }

// Repos is the limit to top level names, nil for none.
func (s *DirectoryStore) Repos() []string { return s.repos }

// Manifest is the blob manifest file or directory, nil for none.
func (s *DirectoryStore) Manifest() *string { return s.manifest }

// CacheDir holds the lookup table of the manifest, nil without one.
func (s *DirectoryStore) CacheDir() *string { return s.cacheDir }

// Entries yields the files of the tree. With a manifest, a file whose row
// has the same size takes SHA1, content type and creation time from the
// row; a Docker manifest without a content type gets one by its content.
func (s *DirectoryStore) Entries(counts *ScanCounts, yield func(Entry) error) error {
	if counts == nil {
		counts = &ScanCounts{}
	}
	if err := s.requireRoot(); err != nil {
		return err
	}
	var wanted map[string]bool
	if s.repos != nil {
		wanted = map[string]bool{}
		for _, repo := range s.repos {
			wanted[repo] = true
		}
	}
	listing, err := listDir(s.root, true)
	if err != nil {
		return err
	}
	slices.SortFunc(listing, func(a, b os.DirEntry) int { return fsCompare(a.Name(), b.Name()) })
	var lookup ManifestLookup
	if s.manifest != nil {
		if OpenManifestLookup == nil {
			return fmt.Errorf("store %s names a blob manifest: %w", s.name, ErrNoManifestLookup)
		}
		if lookup, err = OpenManifestLookup(*s.manifest, *s.cacheDir); err != nil {
			return err
		}
		defer lookup.Close()
	}
	for _, item := range listing {
		name := item.Name()
		if strings.HasPrefix(name, ".") || !item.Type().IsDir() {
			continue
		}
		if wanted != nil && !wanted[name] {
			continue
		}
		if !utf8.ValidString(name) {
			counts.Unparsable++
			continue
		}
		if err := s.walk(name, counts, lookup, yield); err != nil {
			return err
		}
	}
	return nil
}

func (s *DirectoryStore) walk(repo string, counts *ScanCounts, lookup ManifestLookup, yield func(Entry) error) error {
	stack := []string{""}
	for len(stack) > 0 {
		rel := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		directory := pyJoin(s.root, repo)
		if rel != "" {
			directory = pyJoin(directory, rel)
		}
		listing, _ := listDir(directory, false)
		items := map[string]os.DirEntry{}
		for _, item := range listing {
			items[item.Name()] = item
		}
		names := make([]string, 0, len(items))
		for name := range items {
			names = append(names, name)
		}
		slices.SortFunc(names, fsCompare)
		var subdirs []string
		var found []Entry
		for _, name := range names {
			item := items[name]
			if name == ".DS_Store" || strings.HasPrefix(name, ".nxb-") {
				continue
			}
			if !utf8.ValidString(name) {
				counts.Unparsable++
				continue
			}
			path := name
			if rel != "" {
				path = rel + "/" + name
			}
			itemPath := pyJoin(directory, name)
			info := s.fileStat(item, itemPath)
			if info == nil {
				if item.Type().IsDir() {
					subdirs = append(subdirs, path)
				}
				continue
			}
			counts.Live++
			var sha1, contentType *string
			mtime := info.ModTime()
			createdMS := mtime.Unix()*1000 + int64(mtime.Nanosecond())/1_000_000
			if lookup != nil {
				row, err := lookup.Get(repo, path)
				if err != nil {
					return err
				}
				if row != nil && row.Size == info.Size() {
					sha1, contentType, createdMS = row.SHA1, row.ContentType, row.CreatedMS
				}
			}
			if sha1 == nil {
				sha1 = neighbourSHA1(items, name, directory)
			}
			if contentType == nil && dockerManifestPath(path) && info.Size() <= MaxManifestBytes {
				contentType = sniffManifest(itemPath)
			}
			found = append(found, Entry{
				Repo:        repo,
				Path:        path,
				Size:        info.Size(),
				SHA1:        sha1,
				ContentType: contentType,
				CreatedMS:   createdMS,
				Location:    repo + "/" + path,
			})
		}
		for i := len(subdirs) - 1; i >= 0; i-- {
			stack = append(stack, subdirs[i])
		}
		for _, entry := range found {
			if err := yield(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// fileStat is the stat of a regular file, or of a file link whose target
// is inside the root; nil for anything else.
func (s *DirectoryStore) fileStat(item os.DirEntry, itemPath string) fs.FileInfo {
	if item.Type()&fs.ModeSymlink == 0 {
		if !item.Type().IsRegular() {
			return nil
		}
		info, err := os.Lstat(itemPath)
		if err != nil {
			return nil
		}
		return info
	}
	real, err := realpath(purePath(itemPath), true)
	if err != nil || !within(real, s.realRoot) {
		return nil
	}
	info, err := os.Stat(real)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	return info
}

// LooksUnmounted reports whether the root, for a file blob store its
// content/ directory, is missing or holds nothing. That is how an unmounted
// disk looks, also when its mount point stays as an empty directory.
func LooksUnmounted(s Store) bool {
	dir := s.Root()
	if s.Kind() == KindBlobStore {
		dir = joinPath(dir, ContentDir)
	}
	f, err := os.Open(dir) // #nosec G304 -- the store root the operator configured, or its content/ directory.
	if err != nil {
		return true
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	return err != nil || len(names) == 0
}

// Make returns the store of a configuration entry. A manifest needs the
// data directory, which holds its lookup table; "" means none.
func Make(cfg config.StoreConfig, dataDir string) (Store, error) {
	root, err := expandUser(cfg.Root)
	if err != nil {
		return nil, err
	}
	switch cfg.Kind {
	case KindBlobStore:
		if cfg.Manifest != nil {
			return nil, &Error{Class: "ValueError",
				Message: fmt.Sprintf("store %s: only directory stores take a manifest", cfg.Name)}
		}
		s, err := NewBlobStore(cfg.Name, root)
		if err != nil {
			return nil, err
		}
		return s, nil
	case KindDirectory:
		var manifest, cacheDir *string
		if cfg.Manifest != nil {
			if dataDir == "" {
				return nil, &Error{Class: "ValueError", Message: fmt.Sprintf(
					"store %s names a manifest, which needs the data directory for its lookup table", cfg.Name)}
			}
			m, err := expandUser(*cfg.Manifest)
			if err != nil {
				return nil, err
			}
			c := joinPath(dataDir, ManifestsDir)
			manifest, cacheDir = &m, &c
		}
		s, err := NewDirectoryStore(cfg.Name, root, cfg.Repos, manifest, cacheDir)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	return nil, &Error{Class: "ValueError", Message: "unknown store kind " + repr(cfg.Kind)}
}

// ManifestMediaType is the Docker or OCI media type of a manifest document,
// "" for none. The first rule that applies: the mediaType field; a
// manifests list gives an OCI image index; config and layers give a Docker
// schema 2 manifest when the config has the Docker config type, else an OCI
// image manifest; schemaVersion 1 gives Docker schema 1 signed.
func ManifestMediaType(data []byte) string {
	value, err := pyjson.Decode(data)
	if err != nil {
		return ""
	}
	document, ok := value.(*pyjson.Object)
	if !ok {
		return ""
	}
	if mediaType, ok := get(document, "mediaType").(string); ok && strings.Contains(mediaType, "/") && printable(mediaType) {
		return mediaType
	}
	if _, ok := get(document, "manifests").([]any); ok {
		return OCIIndex
	}
	if cfg, ok := get(document, "config").(*pyjson.Object); ok {
		if _, ok := get(document, "layers").([]any); ok {
			if mediaType, ok := get(cfg, "mediaType").(string); ok && mediaType == DockerConfig {
				return DockerManifest
			}
			return OCIManifest
		}
	}
	switch version := get(document, "schemaVersion").(type) {
	case int64:
		if version == 1 {
			return DockerSchema1Signed
		}
	case *big.Int:
		if version.IsInt64() && version.Int64() == 1 {
			return DockerSchema1Signed
		}
	}
	return ""
}

func get(o *pyjson.Object, key string) any {
	value, _ := o.Get(key)
	return value
}

// printable reports whether every character is printable ASCII without space.
func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] >= 0x7F {
			return false
		}
	}
	return true
}

// dockerManifestPath is true for v2/<name>/manifests/<reference>.
func dockerManifestPath(path string) bool {
	parent, reference := "", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		parent, reference = path[:i], path[i+1:]
	}
	return reference != "" && strings.HasPrefix(parent, "v2/") && strings.HasSuffix(parent, "/manifests") &&
		len(parent) > len("v2//manifests")
}

func sniffManifest(path string) *string {
	data, err := readUpTo(path, MaxManifestBytes+1)
	if err != nil || len(data) > MaxManifestBytes {
		return nil
	}
	if mediaType := ManifestMediaType(data); mediaType != "" {
		return &mediaType
	}
	return nil
}

// listDir returns the entries of a directory. A failure is an error when
// strict, else it is logged and the listing is empty.
func listDir(path string, strict bool) ([]os.DirEntry, error) {
	f, err := os.Open(path) // #nosec G304 -- the store root, its content/ directory, or a directory below them that the walk listed as a directory and not as a link.
	var listing []os.DirEntry
	if err == nil {
		listing, err = f.ReadDir(-1)
		f.Close()
	}
	if err != nil {
		if strict {
			return nil, osError(err)
		}
		warn("cannot list %s: %s", path, osErrorText(err))
		return nil, nil
	}
	return listing, nil
}

func readUpTo(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- a file the walk found below the store root: a regular file, or a link fileStat resolved inside the root.
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

func asciiSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
}

// neighbourSHA1 is the SHA1 written in <name>.sha1 next to the file, when
// that regular file holds one as its first word.
func neighbourSHA1(items map[string]os.DirEntry, name, directory string) *string {
	item, ok := items[name+".sha1"]
	if !ok || !item.Type().IsRegular() {
		return nil
	}
	data, err := readUpTo(pyJoin(directory, item.Name()), MaxSHA1FileBytes)
	if err != nil {
		return nil
	}
	words := strings.FieldsFunc(string(data), asciiSpace)
	if len(words) == 0 || len(words[0]) != 40 {
		return nil
	}
	word := []byte(words[0])
	for i, c := range word {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
			word[i] = c + 'a' - 'A'
		default:
			return nil
		}
	}
	text := string(word)
	return &text
}
