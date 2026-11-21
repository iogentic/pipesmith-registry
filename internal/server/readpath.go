package server

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/stores"
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
	snapshot   *model.Snapshot // the state the attributes of the repositories come from; nil without security
	routing    *routingNow     // the links of the routing rules; nil without them
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
	// createdGen and createdNames are the generation of the created stores
	// this state attaches and their names (attachCreated, SR1).
	createdGen   uint64
	createdNames []string
}

// readPath is ReadPath of the Python module: stores, index and
// repositories of one server. sync takes over a change of config.json, of
// repositories.json or of the index generation before each request.
type readPath struct {
	cfg        *config.Config
	openPorts  map[string]int         // the ports this process listens on, repository -> port
	writeStore stores.Store           // the write store of the data directory, attached beside config.json; nil for none
	snapshot   func() *model.Snapshot // the state of the security; nil without security
	routing    *liveRouting           // the routing rule each repository names (ruling P5-4 of the phase 5 session, SY3); nil for none
	index      *index.Index
	mu         sync.Mutex
	current    atomic.Pointer[state]
	// lateWrite gives the write store once it exists, for a server without
	// writes that found none at its start (attachLate); nil otherwise.
	lateWrite func() stores.Store
	// dropped removes the side data of a row the read path dropped
	// (catalog.Drop, leg F1b); nil for none.
	dropped func(repo, path, store string) error
	// created gives the created stores to attach beside config.json, and
	// createdAt their generation, an atomic read for the fast path of sync
	// (writestores.go, SR1, review R10); nil for none.
	created   func() []stores.Store
	createdAt func() uint64
}

// attach makes the stores of config.json, and adds extra, the write store,
// under its name when no store of config.json has it.
func attach(configs []config.StoreConfig, dataDir string, extra stores.Store) (map[string]stores.Store, error) {
	attached := map[string]stores.Store{}
	for _, sc := range configs {
		s, err := stores.Make(sc, dataDir)
		if err != nil {
			return nil, err
		}
		attached[sc.Name] = s
	}
	if extra != nil && attached[extra.Name()] == nil {
		attached[extra.Name()] = extra
	}
	return attached, nil
}

func newReadPath(cfg *config.Config, dockerPorts map[int]string, writeStore stores.Store, snapshot func() *model.Snapshot,
	routes *liveRouting) (*readPath, error) {
	rp := &readPath{cfg: cfg, openPorts: map[string]int{}, writeStore: writeStore, snapshot: snapshot, routing: routes}
	for _, port := range sortedPorts(dockerPorts) {
		rp.openPorts[dockerPorts[port]] = port
	}
	st := &state{configKey: keyOf(cfg.ConfigPath()), configs: append([]config.StoreConfig(nil), cfg.Stores...)}
	attached, err := attach(cfg.Stores, cfg.DataDir, writeStore)
	if err != nil {
		return nil, err
	}
	st.stores = attached
	ix, err := index.OpenRead(cfg.IndexPath())
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
	st.reposKey = reposKey{st.definitionsKey, generation, rp.currentSnapshot(), rp.routing.links()}
	if st.definitions, err = repos.LoadDefinitions(cfg.RepositoriesPath()); err != nil {
		return fail(err)
	}
	if st.repositories, err = rp.repositoriesOf(st.definitions, st.reposKey); err != nil {
		return fail(err)
	}
	rp.current.Store(st)
	return rp, nil
}

// snapshotOf is the source of the state of a security, nil without one.
func snapshotOf(sec model.Security) func() *model.Snapshot {
	if sec == nil {
		return nil
	}
	return sec.Snapshot
}

// currentSnapshot is the state of the security now, nil without one.
func (rp *readPath) currentSnapshot() *model.Snapshot {
	if rp.snapshot == nil {
		return nil
	}
	return rp.snapshot()
}

// repositoriesOf are the repositories of the definitions and the index,
// with the stored attributes of the state when there is one.
func (rp *readPath) repositoriesOf(definitions []repos.Repo, key reposKey) (*repos.Repositories, error) {
	snapshot := key.snapshot
	names, err := rp.index.RepoNames()
	if err != nil {
		return nil, err
	}
	hints, err := rp.index.Hints()
	if err != nil {
		return nil, err
	}
	repositories := repos.New(definitions, names, hints)
	if snapshot != nil {
		repositories = repositories.WithAttributes(snapshot.Repositories)
	}
	if key.routing != nil && key.routing.links != nil {
		repositories = repositories.WithRoutingRules(key.routing.links)
	}
	return repositories, nil
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
	key := reposKey{keyOf(rp.cfg.RepositoriesPath()), generation, rp.currentSnapshot(), rp.routing.links()}
	st := rp.current.Load()
	if configKey == st.configKey && key == st.reposKey && rp.createdGen() == st.createdGen {
		return st, nil
	}
	rp.mu.Lock()
	defer rp.mu.Unlock()
	next := *rp.current.Load()
	defer func() { rp.current.Store(&next) }()
	rp.attachLate(&next)
	reloaded := false
	if configKey != next.configKey {
		next.configKey = configKey
		if err := rp.reloadStores(&next); err != nil {
			return nil, err
		}
		reloaded = true
	}
	if gen := rp.createdGen(); reloaded || gen != next.createdGen {
		rp.attachCreated(&next, gen)
	}
	if key != next.reposKey {
		next.reposKey = key
		if err := rp.reloadRepositories(&next, key); err != nil {
			return nil, err
		}
	}
	return &next, nil
}

// createdGen is the generation of the created stores, 0 for none.
func (rp *readPath) createdGen() uint64 {
	if rp.createdAt == nil {
		return 0
	}
	return rp.createdAt()
}

// attachCreated attaches the created stores beside the stores of
// config.json (SR1, SR3): a store a writer opened since, and none that was
// closed. A store of config.json keeps its name.
func (rp *readPath) attachCreated(next *state, gen uint64) {
	if rp.created == nil {
		return
	}
	list := rp.created()
	attached := maps.Clone(next.stores)
	for _, name := range next.createdNames {
		if !slices.ContainsFunc(next.configs, func(sc config.StoreConfig) bool { return sc.Name == name }) {
			delete(attached, name)
		}
	}
	next.createdNames = nil
	for _, s := range list {
		if attached[s.Name()] == nil {
			attached[s.Name()] = s
			next.createdNames = append(next.createdNames, s.Name())
		}
	}
	next.stores, next.createdGen = attached, gen
}

// attachCreatedNow attaches the created stores to the state of now, at a
// start once rp.created is set.
func (rp *readPath) attachCreatedNow() {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	next := *rp.current.Load()
	rp.attachCreated(&next, rp.createdGen())
	rp.current.Store(&next)
}

// attachLate attaches the write store when it appeared after the start of a
// server without writes: the command maven rebuild-metadata makes it beside
// such a server and writes rows of it into the index, and a row of a store
// that is not attached answers 503 (finding S4 of review 4). The rows come
// with a new generation of the index, and its take over calls this first.
func (rp *readPath) attachLate(next *state) {
	if rp.writeStore != nil || rp.lateWrite == nil {
		return
	}
	s := rp.lateWrite()
	if s == nil {
		return
	}
	rp.writeStore = s
	if next.stores[s.Name()] == nil {
		attached := maps.Clone(next.stores)
		attached[s.Name()] = s
		next.stores = attached
	}
}

func (rp *readPath) reloadStores(next *state) error {
	cfg, err := config.Load(rp.cfg.DataDir, config.Env(nil))
	if err == nil && stores.Writable(rp.writeStore) &&
		slices.ContainsFunc(cfg.Stores, func(sc config.StoreConfig) bool { return sc.Name == rp.writeStore.Name() }) {
		// as at a start with writes on: the store would take the place of the
		// write store in the read path (finding N6 of review 4)
		logAt(slogWarn, serverLogger, "configuration not reloaded, the attached stores stay: store %s of %s has the name of the write store of this server; remove it",
			rp.writeStore.Name(), rp.cfg.ConfigPath())
		return nil
	}
	var attached map[string]stores.Store
	if err == nil {
		attached, err = attach(cfg.Stores, rp.cfg.DataDir, rp.writeStore)
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

func (rp *readPath) reloadRepositories(next *state, key reposKey) error {
	definitionsKey := key.file
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
	repositories, err := rp.repositoriesOf(next.definitions, key)
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
				dropped, err := rp.index.DropRow(asset.Repo, asset.Path, asset.Store)
				if err != nil {
					return nil, nil, 0, err
				}
				if dropped && rp.dropped != nil {
					if err := rp.dropped(asset.Repo, asset.Path, asset.Store); err != nil {
						logAt(slogWarn, serverLogger, "the catalog keeps the row of %s/%s in store %s until the next scan: %v", asset.Repo, asset.Path, asset.Store, err)
					}
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

// attachedNames are the names of the stores config.json attaches now.
func (rp *readPath) attachedNames() []string {
	st := rp.current.Load()
	names := make([]string, 0, len(st.configs))
	for _, sc := range st.configs {
		names = append(names, sc.Name)
	}
	return names
}
