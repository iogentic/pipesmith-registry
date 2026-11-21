package rest

import (
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// The writes of the blob stores (leg 6-stores, contract S13): create,
// save and delete of a file store, and the count of its marked blobs. A
// created store is a record of state.sqlite with a path below
// <data>/stores/ (ruling ST1); a hosted repository that names it writes
// into it (SR1, SR5). Every change passes admin.Change.

// StoreWrite is what a write of a blob store or of a repository needs
// besides Env.
type StoreWrite struct {
	Store   admin.IdentityStore
	Subject *model.Subject
	// DataDir is the data directory of the server.
	DataDir string
	// Roots are the roots of the stores the server attaches, the write
	// store among them; Attached names the stores config.json attaches.
	Roots    []string
	Attached []string
	// Lookup resolves the host of a remote address (ruling ST8); nil
	// resolves nothing.
	Lookup admin.Lookup
	// Erase removes the content of a repository before its record goes
	// (A7, ruling ST5); nil removes nothing.
	Erase func(name string) error
	// OldRows names the stores of the old product that hold rows of a
	// repository (ruling ST17); nil names none.
	OldRows func(name string) ([]string, error)
	// Rebuild writes the maven metadata of a hosted maven2 repository again
	// (ruling ST6).
	Rebuild func(r repos.Repo) (Answer, error)
	// Release closes the writer of a deleted created store (SR8); nil
	// closes none.
	Release func(name string) error
}

// storeBody is the body of a create or save of a file store.
type storeBody struct {
	Name      string           `json:"name"`
	Path      *string          `json:"path"`
	SoftQuota *model.SoftQuota `json:"softQuota"`
}

// takenPaths are the roots of the attached stores and the paths of the
// stored ones, except the store name.
func (w StoreWrite) takenPaths(s *model.Snapshot, name string) []string {
	taken := slices.Clone(w.Roots)
	for _, b := range s.BlobStores {
		if b.Name != name && b.Path != "" {
			taken = append(taken, b.Path)
		}
	}
	return taken
}

// storeExists reports a store of the name, without regard to letter case
// (ST14): a record, an attached store or the write store.
func (w StoreWrite) storeExists(s *model.Snapshot, name string) bool {
	names := append([]string{admin.WriteStore}, w.Attached...)
	for _, b := range s.BlobStores {
		names = append(names, b.Name)
	}
	return admin.Taken(names, name)
}

// CreateFileStore is POST /v1/blobstores/file: 204. An absent path is
// <data>/stores/<name>; the folder is made.
func CreateFileStore(env *Env, w StoreWrite, body []byte) (Answer, error) {
	var b storeBody
	if err := decode(body, '{', &b); err != nil {
		return refused(err)
	}
	_, err := admin.Change(w.Store, []state.Kind{state.KindBlobStores}, func(s *model.Snapshot) error {
		if err := admin.CheckCreate(admin.KindBlobStore, b.Name, w.storeExists(s, b.Name)); err != nil {
			return err
		}
		sent := "<data>/stores/" + b.Name
		if b.Path != nil {
			sent = *b.Path
		}
		if err := admin.CheckStoreCreate("file", sent, b.SoftQuota); err != nil {
			return err
		}
		resolved, err := admin.StorePath(w.DataDir, b.Name, b.Path, w.takenPaths(s, b.Name))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(resolved, 0o750); err != nil {
			return err
		}
		s.BlobStores = append(slices.Clone(s.BlobStores), model.BlobStore{Name: b.Name, Type: "File", Path: resolved, SoftQuota: b.SoftQuota})
		return nil
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}

// SaveFileStore is PUT /v1/blobstores/file/{name}: 204. The path stays
// (A6); the soft quota is set, null removes it. A store that config.json
// attaches without a record gets a record with its root.
func SaveFileStore(env *Env, w StoreWrite, name string, body []byte) (Answer, error) {
	var b storeBody
	if err := decode(body, '{', &b); err != nil {
		return refused(err)
	}
	_, err := admin.Change(w.Store, []state.Kind{state.KindBlobStores}, func(s *model.Snapshot) error {
		i := slices.IndexFunc(s.BlobStores, func(x model.BlobStore) bool { return x.Name == name })
		var stored model.BlobStore
		switch {
		case i >= 0:
			stored = s.BlobStores[i]
		default:
			found, ok := env.blobStore(name)
			if !ok {
				return notFound("blob store " + name)
			}
			stored = model.BlobStore{Name: name, Type: found.kind, Path: found.path}
		}
		if err := admin.CheckStoreSave(stored, b.Path, b.SoftQuota); err != nil {
			return err
		}
		stored.SoftQuota = b.SoftQuota
		list := slices.Clone(s.BlobStores)
		if i >= 0 {
			list[i] = stored
		} else {
			list = append(list, stored)
		}
		s.BlobStores = list
		return nil
	})
	if err != nil {
		return refused(err)
	}
	return noContent()
}

// DeleteStore is DELETE /v1/blobstores/{name}: 204. local-write and a
// store config.json attaches are kept (409, ST2), and so is a store a
// repository names (409, A6). The folder of a created store stays.
func DeleteStore(env *Env, w StoreWrite, name string) (Answer, error) {
	_, err := admin.Change(w.Store, []state.Kind{state.KindBlobStores}, func(s *model.Snapshot) error {
		if name != admin.WriteStore && !slices.Contains(w.Attached, name) && !admin.Exists(s, admin.KindBlobStore, name) {
			return notFound("blob store " + name)
		}
		if err := admin.CheckStoreProtected(name, slices.Contains(w.Attached, name)); err != nil {
			return err
		}
		if err := admin.CheckStoreDelete(s, name); err != nil {
			return err
		}
		s.BlobStores = slices.DeleteFunc(slices.Clone(s.BlobStores), func(b model.BlobStore) bool { return b.Name == name })
		return nil
	})
	if err != nil {
		return refused(err)
	}
	if w.Release != nil {
		if err := w.Release(name); err != nil {
			return Answer{}, err
		}
	}
	return noContent()
}

// deletedXO is the answer of GET /api/blob-stores/{name}/deleted (SR12):
// blobCount and bytes are null before the first count of the store ends,
// counting is true while a count runs, countedAt is the end of the count
// given.
type deletedXO struct {
	Name      string  `json:"name"`
	BlobCount *int64  `json:"blobCount"`
	Bytes     *int64  `json:"bytes"`
	Counting  bool    `json:"counting"`
	CountedAt *string `json:"countedAt"`
	Reason    *string `json:"reason,omitempty"` // why there is no count, when that is no failure (SC1)
}

// MarkedCount is a count of the blobs of a store that carry the delete
// mark, as the server keeps it.
type MarkedCount struct {
	Have, Counting bool
	Count, Bytes   int64
	At             time.Time
	Reason         string // why there is no count, as a folder that does not exist
}

// DeletedBlobs is GET /api/blob-stores/{name}/deleted: the count of the
// marked blobs of the store as the server has it now; found false is 404.
func DeletedBlobs(name string, found bool, c MarkedCount) Answer {
	if !found {
		a, _ := refuse(http.StatusNotFound, "blob store "+name+" not found")
		return a
	}
	xo := deletedXO{Name: name, Counting: c.Counting}
	if c.Reason != "" && !c.Have {
		reason := c.Reason
		xo.Reason = &reason
	}
	if c.Have {
		count, size, at := c.Count, c.Bytes, c.At.UTC().Format(time.RFC3339)
		xo.BlobCount, xo.Bytes, xo.CountedAt = &count, &size, &at
	}
	a, _ := ok(xo)
	return a
}
