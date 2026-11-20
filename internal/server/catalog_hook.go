package server

import (
	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/scan"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// catalogHook is the catalog hook of a server that holds catalog.sqlite
// open (leg F1b): the rows of a write and of a delete go into the file, with
// the format of the repository as the read path knows it, the format a scan
// gives the repository too.
type catalogHook struct {
	file   *catalog.Catalog
	format func(repo string) string
}

var _ CatalogHook = catalogHook{}

func (h catalogHook) Put(asset index.Asset, d stores.Digests, uploader, address string) error {
	return h.file.Put(asset, h.format(asset.Repo), catalog.Digests(d), uploader, address)
}

func (h catalogHook) Delete(repo, path string) error {
	return h.file.Delete(repo, path)
}

// formatOf is the format of a repository of the read path, "unknown" for a
// name it does not know.
func (rp *readPath) formatOf(repo string) string {
	st, err := rp.sync()
	if err != nil {
		return "unknown"
	}
	if r, ok := st.repositories.Get(repo); ok && r.Format != "" {
		return r.Format
	}
	return "unknown"
}

// scanIn is scan.Run with the catalog file of the server.
func scanIn(file *catalog.Catalog) scanRunner {
	return func(s stores.Store, ix *index.Index, progress func(live int)) (scan.Result, error) {
		return scan.RunIn(file, s, ix, progress, nil)
	}
}
