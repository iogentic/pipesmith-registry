package catalog

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

const loggerName = "pipesmith_registry.catalog"

// FormatsOf gives each repository the format the read path gives it: its
// definition in repositories.json of dataDir, else the hint of the index,
// else "unknown".
func FormatsOf(dataDir string, ix *index.Index) (Formats, error) {
	defined, err := repos.LoadDefinitions(filepath.Join(dataDir, "repositories.json"))
	if err != nil {
		return nil, err
	}
	names, err := ix.RepoNames()
	if err != nil {
		return nil, err
	}
	hints, err := ix.Hints()
	if err != nil {
		return nil, err
	}
	all := repos.New(defined, names, hints)
	return func(repo string) string {
		if r, ok := all.Get(repo); ok {
			return r.Format
		}
		if hint := hints[repo]; hint != "" {
			return hint
		}
		return "unknown"
	}, nil
}

// Scan is the catalog side of one scan of one store as internal/scan runs
// it: catalog.sqlite beside index.sqlite takes the entries the index takes,
// once the index holds them. It never fails the scan. When the catalog
// cannot take the scan, a warning names the cause and the catalog keeps the
// rows it had, which the next scan brings up to date.
type Scan struct {
	dir, store string
	c          *Catalog
	fill       *Fill
	err        error
}

// StartScan opens the catalog beside the index for a scan of store.
func StartScan(ix *index.Index, store string) *Scan {
	s := &Scan{dir: filepath.Dir(ix.Path()), store: store}
	if s.c, s.err = Open(s.dir); s.err == nil {
		s.fill, s.err = s.c.Begin(store)
	}
	return s
}

// Add hands over one entry the index took.
func (s *Scan) Add(e stores.Entry) {
	if s.err == nil {
		s.err = s.fill.Add(e)
	}
}

// Finish writes the entries into the catalog when the index holds them
// (written), else drops them, and closes the catalog.
func (s *Scan) Finish(ix *index.Index, written bool) {
	if s.err == nil && written {
		var formats Formats
		var stats []index.StoreStat
		if formats, s.err = FormatsOf(s.dir, ix); s.err == nil {
			stats, s.err = ix.StoreStats()
		}
		if s.err == nil {
			held := make([]string, len(stats))
			for i, stat := range stats {
				held[i] = stat.Store
			}
			s.err = s.fill.Commit(formats, held)
		}
	}
	if s.fill != nil {
		_ = s.fill.Discard() // after Commit it does nothing; the staging file of a failed fill is left to removeStale
	}
	if s.c != nil {
		if err := s.c.Close(); err != nil && s.err == nil {
			s.err = err
		}
	}
	if s.err != nil && written {
		slog.Warn(fmt.Sprintf("the catalog %s did not take the scan of store %s and keeps its earlier rows: %v",
			Path(s.dir), s.store, s.err), "logger", loggerName)
	}
}
