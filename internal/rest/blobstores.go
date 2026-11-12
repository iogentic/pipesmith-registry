package rest

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"syscall"

	"github.com/mirosso25/pipesmith-registry/internal/model"
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
		xo := blobStoreXO{Name: b.name, Type: b.kind, Unavailable: true, SoftQuota: b.quota}
		if b.attached != nil {
			c := counts[b.name]
			xo.BlobCount, xo.TotalSizeInBytes = c[0], c[1]
			if b.attached.Available {
				xo.Unavailable = false
				xo.AvailableSpaceInBytes = freeBytes(b.attached.Root)
			}
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
