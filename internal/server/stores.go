package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"time"

	adminrules "github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"

	"github.com/iogentic/pipesmith-registry/internal/rest"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The writes of the blob stores and of the repositories (leg 6-stores).
// They change records of state.sqlite through Options.Identity, need
// security on and work with writes on or off (ruling ST4); a write into the
// content still needs writes. Each route asks the permission the contract
// names for its action (ST10). internal/rest builds the answers.

// storeBuild builds the answer of one write of a store or repository.
type storeBuild func(env *rest.Env, w rest.StoreWrite, match []string, body []byte) (rest.Answer, error)

// storeRoute is a write with the permission rule r.
func storeRoute(method, path string, r rule, build storeBuild) route {
	return entry(exact(path), []string{method}, r, func(s *Server, w *recorder, req *request, match []string) error {
		return s.identityWrite(w, req, match, func(env *rest.Env, iw rest.IdentityWrite, m []string, _ url.Values, body []byte) (rest.Answer, error) {
			sw, err := s.storeWrite(iw)
			if err != nil {
				return rest.Answer{}, err
			}
			address := peerAddress(w)
			sw.Rebuild = func(repo repos.Repo) (rest.Answer, error) { return s.rebuildIndex(repo, iw.Subject, address) }
			return build(env, sw, m, body)
		})
	})
}

// storeWrite is what internal/rest needs for a write of a store: the data
// directory and the roots and names of the attached stores, the write
// store among them.
func (s *Server) storeWrite(iw rest.IdentityWrite) (rest.StoreWrite, error) {
	st, err := s.rp.sync()
	if err != nil {
		return rest.StoreWrite{}, err
	}
	w := rest.StoreWrite{Store: iw.Store, Subject: iw.Subject, DataDir: s.rp.cfg.DataDir,
		Roots: []string{writeStoreRoot(s.rp.cfg.DataDir)}, Lookup: func(host string) ([]netip.Addr, error) { return lookupHost(host) }, Erase: s.eraseRepository,
		OldRows: s.oldRows}
	for _, sc := range st.configs {
		w.Attached = append(w.Attached, sc.Name)
		if sc.Root != "" && !slices.Contains(w.Roots, sc.Root) {
			w.Roots = append(w.Roots, sc.Root)
		}
	}
	return w, nil
}

// blobStoreRoot is the root of the store name for the count of its marked
// blobs: the write store, a store config.json attaches, else the path of
// its record.
func (s *Server) blobStoreRoot(name string) (string, bool, error) {
	st, err := s.rp.sync()
	if err != nil {
		return "", false, err
	}
	if store := st.stores[name]; store != nil && store.Kind() == stores.KindBlobStore {
		return store.Root(), true, nil
	}
	if snap := s.sec.Snapshot(); snap != nil {
		for _, b := range snap.BlobStores {
			if b.Name == name {
				return b.Path, true, nil
			}
		}
	}
	return "", false, nil
}

const blobStoresBase = restBase + "/blobstores"

// storeRoutes are the writes of the blob stores and of the repositories
// in the table with security on, writes on or off. They come after
// proxyRoutes, and rebuild-index before the create, whose pattern also
// matches the paths of both.
var storeRoutes = []route{
	storeRoute("POST", blobStoresBase+"/file", perm("nexus:blobstores:create"),
		func(env *rest.Env, w rest.StoreWrite, _ []string, body []byte) (rest.Answer, error) {
			return rest.CreateFileStore(env, w, body)
		}),
	storeRoute("PUT", blobStoresBase+"/file/"+segment, perm("nexus:blobstores:update"),
		func(env *rest.Env, w rest.StoreWrite, m []string, body []byte) (rest.Answer, error) {
			return rest.SaveFileStore(env, w, m[1], body)
		}),
	storeRoute("DELETE", blobStoresBase+"/"+segment, perm("nexus:blobstores:delete"),
		func(env *rest.Env, w rest.StoreWrite, m []string, _ []byte) (rest.Answer, error) {
			return rest.DeleteStore(env, w, m[1])
		}),
	storeRoute("POST", restBase+"/repositories/"+segment+"/rebuild-index", rule{kind: ruleAdmin, group: 1, action: "edit"},
		func(env *rest.Env, w rest.StoreWrite, m []string, _ []byte) (rest.Answer, error) {
			return rest.RebuildIndex(env, w, m[1])
		}),
	storeRoute("POST", restBase+"/repositories/"+segment+"/"+segment, rule{kind: ruleCreate, group: 1},
		func(env *rest.Env, w rest.StoreWrite, m []string, body []byte) (rest.Answer, error) {
			return rest.CreateRepository(env, w, m[1], m[2], body)
		}),
	storeRoute("PUT", restBase+"/repositories/"+segment+"/"+segment+"/"+segment, rule{kind: ruleAdmin, group: 3, action: "edit"},
		func(env *rest.Env, w rest.StoreWrite, m []string, body []byte) (rest.Answer, error) {
			return rest.SaveRepository(env, w, m[1], m[2], m[3], body)
		}),
	storeRoute("DELETE", restBase+"/repositories/"+segment, rule{kind: ruleAdmin, group: 1, action: "delete"},
		func(env *rest.Env, w rest.StoreWrite, m []string, _ []byte) (rest.Answer, error) {
			return rest.DeleteRepository(env, w, m[1])
		}),
	get("/api/blob-stores/"+segment+"/deleted", perm("nexus:blobstores:read"), func(s *Server, w *recorder, req *request, m []string) error {
		return s.restAnswer(w, req, func(*rest.Env, url.Values) (rest.Answer, error) {
			root, found, err := s.blobStoreRoot(m[1])
			if err != nil {
				return rest.Answer{}, err
			}
			return rest.DeletedBlobs(m[1], root, found)
		})
	}),
}

// lookupHost resolves the host of a remote address for the rules ST8 and
// ST11, within 5 s; tests replace it.
var lookupHost = func(host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// unavailableRefusal is the 503 of a repository change the server cannot
// make now.
func unavailableRefusal(format string, args ...any) error {
	return &adminrules.Refusal{Code: adminrules.CodeUnavailable, Message: fmt.Sprintf(format, args...)}
}

// oldRows names the stores other than the write store that hold rows of
// the repository name (ruling ST17), sorted.
func (s *Server) oldRows(name string) ([]string, error) {
	rows, err := s.rp.index.RepoRows(name)
	if err != nil {
		return nil, err
	}
	writeStore := repos.DefaultWriteStore
	if s.writes.writer != nil {
		writeStore = s.writes.writer.Name()
	}
	var names []string
	for _, row := range rows {
		if row.Store != writeStore && !slices.Contains(names, row.Store) {
			names = append(names, row.Store)
		}
	}
	slices.Sort(names)
	return names, nil
}

// eraseRepository removes the content of the repository name before its
// record goes (A7, ruling ST5): every row of the index, the blob of a row
// of the write store with the delete mark first, the rows of the catalog
// and the proxy state. A blob of a store of the old product stays as it
// is; only its row goes. A row in the write store needs the writer: without
// it the answer is 503 and nothing is removed.
func (s *Server) eraseRepository(name string) error {
	rows, err := s.rp.index.RepoRows(name)
	if err != nil {
		return err
	}
	writeStore := repos.DefaultWriteStore
	if s.writes.writer != nil {
		writeStore = s.writes.writer.Name()
	}
	if s.writes.writer == nil && slices.ContainsFunc(rows, func(a index.Asset) bool { return a.Store == writeStore }) {
		return unavailableRefusal("the repository %s holds blobs in %s, which get the delete mark, and the store is not open; start the server with writes or upstream on", name, writeStore)
	}
	for _, row := range rows {
		if row.Store != writeStore {
			if _, err := s.rp.index.Delete(name, row.Path, row.Store); err != nil {
				return err
			}
			continue
		}
		unlock := s.writes.locks.lock(name, row.Path)
		_, err := s.rp.index.DeleteWithin(name, row.Path, row.Store, indexWaitOf(), func() error {
			if err := s.writes.mark(row.Location, reasonDeleted); err != nil {
				return err
			}
			s.writes.noteDeleted(name, row.Path, row.CreatedMS)
			return nil
		})
		unlock()
		if errors.Is(err, index.ErrBusy) {
			return unavailableRefusal("the index stayed busy; the repository %s is partly removed, delete it again", name)
		}
		if err != nil {
			return err
		}
	}
	for _, row := range rows {
		if s.writes.catalog != nil {
			if err := s.writes.catalog.Delete(name, row.Path); err != nil {
				return err
			}
		}
	}
	if s.writes.file != nil {
		return s.writes.file.DropRepository(name)
	}
	if _, err := os.Stat(catalog.Path(s.rp.cfg.DataDir)); err == nil {
		c, err := catalog.Open(s.rp.cfg.DataDir)
		if err != nil {
			return err
		}
		defer c.Close()
		return c.DropRepository(name)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// rebuildIndex writes the maven metadata of the hosted maven2 repository
// again from its files (ruling ST6). It writes blobs into the write store:
// without the writer the answer is 503, for an offline repository 409.
func (s *Server) rebuildIndex(repo repos.Repo, subject *model.Subject, address string) (rest.Answer, error) {
	switch {
	case s.writes.writer == nil:
		return rest.Answer{Status: http.StatusServiceUnavailable, Body: map[string]string{"message": "the rebuild writes the maven metadata into " +
			repos.DefaultWriteStore + ", and the store is not open; start the server with writes on"}}, nil
	case !repo.TakesWrites():
		return rest.Answer{Status: http.StatusConflict, Body: map[string]string{"message": "the repository " + repo.Name +
			" takes no write, so its maven metadata is not written again"}}, nil
	}
	if _, err := s.RebuildMavenMetadata(repo.Name, maven.Scope{}, subject, address); err != nil {
		var refusal *writeRefusal
		if errors.As(err, &refusal) {
			return rest.Answer{Status: refusal.status, Body: map[string]string{"message": refusal.message}}, nil
		}
		return rest.Answer{}, err
	}
	return rest.Answer{Status: http.StatusNoContent}, nil
}
