package server

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/maven"
	"github.com/mirosso25/pipesmith-registry/internal/repos"
	"github.com/mirosso25/pipesmith-registry/internal/stores"
)

// unavailable is StoreUnavailable: a store that holds the asset cannot be
// read now. The row stays.
type unavailable struct{ message string }

func (e *unavailable) Error() string { return e.message }

// fileKey is what changes when a command rewrites a file: modification
// time, size, inode; ok is false while the file is missing.
type fileKey struct {
	ok    bool
	mtime int64
	size  int64
	ino   uint64
}

func keyOf(path string) fileKey {
	info, err := os.Stat(path)
	if err != nil {
		return fileKey{}
	}
	key := fileKey{ok: true, mtime: info.ModTime().UnixNano(), size: info.Size()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		key.ino = uint64(st.Ino)
	}
	return key
}

type reposKey struct {
	file       fileKey
	generation int64
}

// state is one view of the data directory: the attached stores and the
// repositories. A request works on one state from start to end.
type state struct {
	configKey      fileKey
	configs        []config.StoreConfig
	stores         map[string]stores.Store
	definitionsKey fileKey
	reposKey       reposKey
	definitions    []repos.Repo
	repositories   *repos.Repositories
}

// readPath is ReadPath of the Python module: stores, index and
// repositories of one server. sync takes over a change of config.json, of
// repositories.json or of the index generation before each request.
type readPath struct {
	cfg       *config.Config
	openPorts map[string]int // the ports this process listens on, repository -> port
	index     *index.Index
	mu        sync.Mutex
	current   atomic.Pointer[state]
}

func attach(configs []config.StoreConfig, dataDir string) (map[string]stores.Store, error) {
	attached := map[string]stores.Store{}
	for _, sc := range configs {
		s, err := stores.Make(sc, dataDir)
		if err != nil {
			return nil, err
		}
		attached[sc.Name] = s
	}
	return attached, nil
}

func newReadPath(cfg *config.Config, dockerPorts map[int]string) (*readPath, error) {
	rp := &readPath{cfg: cfg, openPorts: map[string]int{}}
	for _, port := range sortedPorts(dockerPorts) {
		rp.openPorts[dockerPorts[port]] = port
	}
	st := &state{configKey: keyOf(cfg.ConfigPath()), configs: append([]config.StoreConfig(nil), cfg.Stores...)}
	attached, err := attach(cfg.Stores, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	st.stores = attached
	ix, err := index.Open(cfg.IndexPath())
	if err != nil {
		return nil, err
	}
	rp.index = ix
	fail := func(err error) (*readPath, error) {
		ix.Close()
		return nil, err
	}
	st.definitionsKey = keyOf(cfg.RepositoriesPath())
	generation, err := ix.Generation()
	if err != nil {
		return fail(err)
	}
	st.reposKey = reposKey{st.definitionsKey, generation}
	if st.definitions, err = repos.LoadDefinitions(cfg.RepositoriesPath()); err != nil {
		return fail(err)
	}
	if st.repositories, err = rp.repositoriesOf(st.definitions); err != nil {
		return fail(err)
	}
	rp.current.Store(st)
	return rp, nil
}

func (rp *readPath) repositoriesOf(definitions []repos.Repo) (*repos.Repositories, error) {
	names, err := rp.index.RepoNames()
	if err != nil {
		return nil, err
	}
	hints, err := rp.index.Hints()
	if err != nil {
		return nil, err
	}
	return repos.New(definitions, names, hints), nil
}

// sync returns the state after taking over what changed since the last
// call. A failure Python does not catch is returned; the keys that changed
// are taken over all the same, as Python stores them before it reloads.
func (rp *readPath) sync() (*state, error) {
	configKey := keyOf(rp.cfg.ConfigPath())
	generation, err := rp.index.Generation()
	if err != nil {
		return nil, err
	}
	key := reposKey{keyOf(rp.cfg.RepositoriesPath()), generation}
	st := rp.current.Load()
	if configKey == st.configKey && key == st.reposKey {
		return st, nil
	}
	rp.mu.Lock()
	defer rp.mu.Unlock()
	next := *rp.current.Load()
	defer func() { rp.current.Store(&next) }()
	if configKey != next.configKey {
		next.configKey = configKey
		if err := rp.reloadStores(&next); err != nil {
			return nil, err
		}
	}
	if key != next.reposKey {
		next.reposKey = key
		if err := rp.reloadRepositories(&next, key.file); err != nil {
			return nil, err
		}
	}
	return &next, nil
}

func (rp *readPath) reloadStores(next *state) error {
	cfg, err := config.Load(rp.cfg.DataDir, config.Env(nil))
	var attached map[string]stores.Store
	if err == nil {
		attached, err = attach(cfg.Stores, rp.cfg.DataDir)
	}
	if err != nil {
		if Caught(err) {
			logAt(slogWarn, serverLogger, "configuration not reloaded, the attached stores stay: %s", PyText(err))
			return nil
		}
		return err
	}
	next.configs, next.stores = cfg.Stores, attached
	return nil
}

func (rp *readPath) reloadRepositories(next *state, definitionsKey fileKey) error {
	if definitionsKey != next.definitionsKey {
		next.definitionsKey = definitionsKey
		definitions, err := repos.LoadDefinitions(rp.cfg.RepositoriesPath())
		switch {
		case err == nil:
			next.definitions = definitions
		case Caught(err):
			logAt(slogWarn, serverLogger, "repository definitions not reloaded, the previous ones stay: %s", PyText(err))
		default:
			return err
		}
	}
	repositories, err := rp.repositoriesOf(next.definitions)
	if err != nil {
		return err
	}
	next.repositories = repositories
	return nil
}

// open is _open: the winning row of a path with its open bytes file and
// real size, nil when no row is left. A bytes file that has vanished drops
// its row and the next row of the path is tried. A store whose root is gone
// gives *unavailable and keeps every row: an unmounted disk must not empty
// the index.
func (rp *readPath) open(st *state, repo, path string) (*index.Asset, *os.File, int64, error) {
	tried := map[[2]string]bool{}
	for {
		asset, err := rp.index.Lookup(repo, path)
		if err != nil || asset == nil {
			return nil, nil, 0, err
		}
		key := [2]string{asset.Store, asset.Location}
		if tried[key] {
			return nil, nil, 0, nil // the drop did not happen: another write held the index
		}
		tried[key] = true
		store := st.stores[asset.Store]
		if store == nil {
			return nil, nil, 0, &unavailable{fmt.Sprintf("Store %s is not configured", asset.Store)}
		}
		f, err := store.Open(asset.Location)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				if stores.LooksUnmounted(store) {
					return nil, nil, 0, &unavailable{fmt.Sprintf("Store %s is not available", store.Name())}
				}
				logAt(slogInfo, serverLogger, "bytes of %s/%s in store %s are gone, row dropped", repo, path, asset.Store)
				if err := rp.index.Drop(asset.Repo, asset.Path, asset.Store); err != nil {
					return nil, nil, 0, err
				}
				continue
			}
			return nil, nil, 0, &unavailable{fmt.Sprintf("Store %s cannot be read: %s", store.Name(), strerrorOf(err))}
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, 0, &unavailable{fmt.Sprintf("Store %s cannot be read: %s", store.Name(), strerrorOf(err))}
		}
		return asset, f, info.Size(), nil
	}
}

// readUpTo reads at most limit bytes of an open file and closes it.
func readUpTo(f *os.File, limit int64) ([]byte, error) {
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// file is _file: the first member that has the path wins; a ".sha1" that
// no member stores comes from the index row of the file it names. The
// maven-metadata.xml of a maven2 group and its checksums are merged.
func (rp *readPath) file(st *state, repo repos.Repo, members []repos.Repo, path string) (*body, error) {
	base, checksum := maven.SplitChecksum(path)
	if repo.Type == "group" && repo.Format == "maven2" && maven.IsMetadataPath(base) {
		return rp.groupMetadata(st, members, base, checksum)
	}
	for _, member := range members {
		asset, f, size, err := rp.open(st, member.Name, path)
		if err != nil {
			return nil, err
		}
		if asset != nil {
			return fileBody(asset, f, size), nil
		}
		if checksum == "sha1" {
			b, err := rp.indexedSHA1(st, member.Name, base)
			if err != nil || b != nil {
				return b, err
			}
		}
	}
	return nil, nil
}

// indexedSHA1 is the ".sha1" of a file that has none stored, from the SHA1
// of its index row, only while the bytes file has the indexed size.
func (rp *readPath) indexedSHA1(st *state, repo, path string) (*body, error) {
	asset, f, size, err := rp.open(st, repo, path)
	if err != nil || asset == nil {
		return nil, err
	}
	f.Close()
	if asset.SHA1 == nil || *asset.SHA1 == "" || size != asset.Size {
		return nil, nil
	}
	sha1 := maven.Lower(*asset.SHA1)
	for i := 0; i < len(sha1); i++ {
		if sha1[i] >= 0x80 {
			return nil, &unicodeEncodeError{"'ascii' codec can't encode the SHA1 of " + path}
		}
	}
	data := []byte(sha1)
	created := asset.CreatedMS
	return &body{size: int64(len(data)), etag: strongEtag(sha1Hex(data)), modifiedMS: &created, contentType: "text/plain", data: data}, nil
}

// groupMetadata is _group_metadata: the merge over every leaf member that
// has the document, or a checksum of the merged bytes; nil when no member
// has a document that parses.
func (rp *readPath) groupMetadata(st *state, members []repos.Repo, path, checksum string) (*body, error) {
	var documents [][]byte
	var newest *int64
	for _, member := range members {
		asset, f, size, err := rp.open(st, member.Name, path)
		if err != nil {
			return nil, err
		}
		if asset == nil {
			continue
		}
		if size > maxMetadataBytes {
			f.Close()
			logAt(slogWarn, serverLogger, "%s/%s in store %s has %d bytes, skipped in the merge", asset.Repo, path, asset.Store, size)
			continue
		}
		data, err := readUpTo(f, maxMetadataBytes+1)
		if err != nil {
			return nil, err
		}
		documents = append(documents, data)
		if newest == nil || asset.CreatedMS > *newest {
			created := asset.CreatedMS
			newest = &created
		}
	}
	if len(documents) == 0 {
		return nil, nil
	}
	merged, err := maven.MergeMetadata(documents)
	if err != nil || merged == nil {
		return nil, err
	}
	data, contentType := merged, "application/xml"
	if checksum != "" {
		data, contentType = []byte(hexDigest(checksum, merged)), "text/plain"
	}
	sha1 := sha1Hex(data)
	etag := mergedEtag(sha1)
	if checksum != "" {
		etag = strongEtag(sha1)
	}
	return &body{size: int64(len(data)), etag: etag, modifiedMS: newest, contentType: contentType, data: data}, nil
}

// unicodeEncodeError is the UnicodeEncodeError of str.encode("ascii"),
// which Python does not catch in the read path.
type unicodeEncodeError struct{ message string }

func (e *unicodeEncodeError) Error() string { return e.message }

// allChildren pages through every child of a directory.
func (rp *readPath) allChildren(repo, directory string) ([]index.Child, error) {
	var out []index.Child
	for offset := 0; ; {
		page, err := rp.index.Children(repo, directory, listingPage, offset)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < listingPage {
			return out, nil
		}
		offset += len(page)
	}
}

func (rp *readPath) anyIsDir(members []repos.Repo, directory string) (bool, error) {
	for _, m := range members {
		found, err := rp.index.IsDir(m.Name, directory)
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}
