package rest

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// blobStoreXO is GenericBlobStoreApiResponse.
type blobStoreXO struct {
	Name                  string           `json:"name"`
	Type                  string           `json:"type"`
	Unavailable           bool             `json:"unavailable"`
	BlobCount             int64            `json:"blobCount"`
	TotalSizeInBytes      int64            `json:"totalSizeInBytes"`
	AvailableSpaceInBytes int64            `json:"availableSpaceInBytes"`
	SoftQuota             *model.SoftQuota `json:"softQuota"`
	// Writable is true for a store this server writes into: local-write
	// and the created stores (SR9); an imported store is read only.
	Writable bool `json:"writable"`
}

// blobStore is one blob store: the stored definition, the store the read
// path attaches under the same name, or both.
type blobStore struct {
	name, kind, path string
	quota            *model.SoftQuota
	attached         *Store
}

// blobStores are the stored definitions and the attached stores, merged by
// name and sorted by name. A store attached without a definition is a file
// store whose path is its local root.
func (env *Env) blobStores() []blobStore {
	byName := map[string]*blobStore{}
	if env.Snapshot != nil {
		for _, b := range env.Snapshot.BlobStores {
			if _, dup := byName[b.Name]; !dup {
				byName[b.Name] = &blobStore{name: b.Name, kind: b.Type, path: b.Path, quota: b.SoftQuota}
			}
		}
	}
	for i := range env.Stores {
		s := &env.Stores[i]
		b, found := byName[s.Name]
		if !found {
			b = &blobStore{name: s.Name, kind: "File", path: s.Root}
			byName[s.Name] = b
		}
		if b.attached == nil {
			b.attached = s
		}
	}
	out := make([]blobStore, 0, len(byName))
	for _, b := range byName {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// freeBytes is the space left to an unprivileged writer below root.
func freeBytes(root string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return 0
	}
	blocks, size := uint64(st.Bavail), uint64(st.Bsize) // #nosec G115 -- block counts and sizes are never negative.
	if size == 0 || blocks > math.MaxInt64/size {
		return math.MaxInt64
	}
	return int64(blocks * size) // #nosec G115 -- checked above against MaxInt64.
}

func (env *Env) blobStoreXOs() ([]blobStoreXO, error) {
	stats, err := env.Index.StoreStats()
	if err != nil {
		return nil, err
	}
	counts := map[string][2]int64{}
	for _, s := range stats {
		counts[s.Store] = [2]int64{s.Files, s.Bytes}
	}
	list := []blobStoreXO{}
	for _, b := range env.blobStores() {
		xo := blobStoreXO{Name: b.name, Type: b.kind, Unavailable: true, SoftQuota: b.quota, Writable: env.writable(b)}
		if b.attached != nil {
			c := counts[b.name]
			xo.BlobCount, xo.TotalSizeInBytes = c[0], c[1]
			// SR11: a readable store is available, an empty one with 0 blobs; one
			// that looks empty while the index holds rows of it is a disk that is
			// not mounted, and one whose folder cannot be read is unavailable
			if b.attached.Available || b.attached.Readable && c[0] == 0 {
				xo.Unavailable = false
				xo.AvailableSpaceInBytes = freeBytes(b.attached.Root)
			}
		} else if b.path != "" && (env.created(b.path) || holdsNoBlob(b.path) && counts[b.name][0] == 0) && ReadableStore(b.path, true) {
			// a record with a folder that config.json does not attach: a store
			// created over REST (ruling ST1), or an empty one of the import of
			// which the index holds no row (SR11, review H4)
			xo.Unavailable = false
			xo.AvailableSpaceInBytes = freeBytes(b.path)
		}
		list = append(list, xo)
	}
	return list, nil
}

// BlobStores is GET /v1/blobstores. The counts are those of the index: the
// live paths of the store, without blobs marked as deleted.
func BlobStores(env *Env) (Answer, error) {
	list, err := env.blobStoreXOs()
	if err != nil {
		return Answer{}, err
	}
	return ok(list)
}

func (env *Env) blobStore(name string) (blobStore, bool) {
	for _, b := range env.blobStores() {
		if b.name == name {
			return b, true
		}
	}
	return blobStore{}, false
}

type fileBlobStoreXO struct {
	Path      string           `json:"path"`
	SoftQuota *model.SoftQuota `json:"softQuota"`
}

// FileBlobStore is GET /v1/blobstores/file/{name}: path and quota of a file
// store, 404 for a store of another type.
func FileBlobStore(env *Env, name string) (Answer, error) {
	b, found := env.blobStore(name)
	if !found || b.kind != "File" {
		return refuse(http.StatusNotFound, "file blob store "+name+" not found")
	}
	return ok(fileBlobStoreXO{Path: b.path, SoftQuota: b.quota})
}

type quotaStatusXO struct {
	IsViolation   bool   `json:"isViolation"`
	Message       string `json:"message"`
	BlobStoreName string `json:"blobStoreName"`
}

// QuotaStatus is GET /v1/blobstores/{name}/quota-status, measured against
// the counts of BlobStores.
func QuotaStatus(env *Env, name string) (Answer, error) {
	list, err := env.blobStoreXOs()
	if err != nil {
		return Answer{}, err
	}
	for _, xo := range list {
		if xo.Name != name {
			continue
		}
		status := quotaStatusXO{BlobStoreName: name}
		q := xo.SoftQuota
		switch {
		case q == nil:
			status.Message = fmt.Sprintf("Blob store %s has no soft quota", name)
		case xo.Unavailable:
			status.Message = fmt.Sprintf("Blob store %s is not available, its soft quota cannot be measured", name)
		case q.Type == "spaceUsedQuota":
			status.IsViolation = xo.TotalSizeInBytes > q.Limit
			status.Message = fmt.Sprintf("Blob store %s uses %d bytes; its soft quota allows %d bytes", name, xo.TotalSizeInBytes, q.Limit)
		case q.Type == "spaceRemainingQuota":
			status.IsViolation = xo.AvailableSpaceInBytes < q.Limit
			status.Message = fmt.Sprintf("Blob store %s has %d bytes free; its soft quota asks for %d bytes", name,
				xo.AvailableSpaceInBytes, q.Limit)
		default:
			status.Message = fmt.Sprintf("Blob store %s has a soft quota of type %s, which this server does not measure", name, q.Type)
		}
		return ok(status)
	}
	return refuse(http.StatusNotFound, "blob store "+name+" not found")
}

// created reports a path of a store created over REST (ruling ST1): a
// folder strictly below the stores folder of the data directory, both with
// their links resolved.
func (env *Env) created(path string) bool {
	if env.StoresRoot == "" {
		return false
	}
	root, err := filepath.EvalSymlinks(env.StoresRoot)
	if err != nil {
		return false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, real)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// writable reports a store this server writes into (SR9): local-write, or
// a record whose path lies below the stores folder and that config.json
// does not attach (admin.WritableStore).
func (env *Env) writable(b blobStore) bool {
	if b.name == admin.WriteStore {
		return true
	}
	if b.path == "" || env.StoresRoot == "" || !env.recorded(b.name) {
		return false
	}
	return admin.WritableStore(filepath.Dir(env.StoresRoot), model.BlobStore{Name: b.name, Path: b.path}, env.Attached, env.Snapshot.BlobStores)
}

// recorded reports a store with a record in the state.
func (env *Env) recorded(name string) bool {
	if env.Snapshot == nil {
		return false
	}
	for _, b := range env.Snapshot.BlobStores {
		if b.Name == name {
			return true
		}
	}
	return false
}

// ReadableDir reports a folder that opens and lists.
func ReadableDir(dir string) bool {
	f, err := os.Open(dir) // #nosec G304 -- the path of a blob store record.
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = f.Readdirnames(1)
	return err == nil || errors.Is(err, io.EOF)
}

// holdsNoBlob reports a store folder whose content/ is missing or holds
// nothing but the folder of the work files (SR11).
func holdsNoBlob(root string) bool {
	entries, err := os.ReadDir(filepath.Join(root, "content"))
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() != "tmp" {
			return false
		}
	}
	return true
}

// ReadableStore reports a store whose folder can be read (SR11, review H2):
// the root opens and lists, and for a blob store content/ is missing or a
// folder that opens and lists, its links resolved.
func ReadableStore(root string, blob bool) bool {
	if !ReadableDir(root) {
		return false
	}
	if !blob {
		return true
	}
	content := filepath.Join(root, "content")
	if _, err := os.Lstat(content); errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return ReadableDir(content)
}
