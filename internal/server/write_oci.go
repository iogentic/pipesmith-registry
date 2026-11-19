package server

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/docker"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/npm"
	"github.com/iogentic/pipesmith-registry/internal/policy"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// What the npm writes (write_npm.go) and the Docker writes (write_docker.go,
// write_docker_upload.go) share, leg 4c of the phase 4 execution plan: the
// state of the process (Docker upload sessions, W3), the action a write asks
// of the view permission, and the copy of a blob as a hard link (section
// Layout of the phase 4 plan, row Copy). Their bodies are read as every
// write reads one (write_body.go).

// ociWrites is the write state of the npm and Docker handlers. The zero
// value is ready for use.
type ociWrites struct {
	mu       sync.Mutex
	sessions map[string]*uploadSession // the open Docker upload sessions by id
	now      func() time.Time          // time.Now when nil; tests move it
}

func (o *ociWrites) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

// ociAction is the action of the view permission that a write to an npm or
// a Docker repository asks where the rule of writeAction, by the path of
// the request, would ask another (seam Permission of a write): a dist-tag
// changes the package document, so it asks edit once the document exists;
// the rewrite of a document under its revision removes versions, so it asks
// delete; the cancel of a Docker upload session ends the caller's own
// session, so it asks add as its start did. ok is false for every other
// request.
func (s *Server) ociAction(method string, t *target) (action string, ok bool, err error) {
	if method == http.MethodGet || method == http.MethodHead {
		return "", false, nil
	}
	st, err := s.rp.sync()
	if err != nil {
		return "", false, err
	}
	repo, found := st.repositories.Get(t.repo)
	if !found {
		return "", false, nil
	}
	switch repos.AccessOf(repo) {
	case "npm":
		call, isWrite := npm.ParseWrite(t.path)
		switch {
		case !isWrite:
		case call.Kind == npm.WriteDistTag:
			row, err := s.rp.index.Lookup(t.repo, call.Name)
			if err != nil {
				return "", false, err
			}
			if row != nil {
				return "edit", true, nil
			}
			return "add", true, nil
		case call.Kind == npm.WriteDocument && call.Rev != "" && method == http.MethodPut:
			return "delete", true, nil
		}
	case "docker":
		if method == http.MethodDelete && strings.HasPrefix(t.path, "v2/") {
			call, err := docker.ParseWrite(t.path[len("v2/"):])
			if err == nil && call != nil && call.Kind == "upload" && call.Reference != "" {
				return "add", true, nil
			}
		}
	}
	return "", false, nil
}

// linkCopy writes a copy of a blob of the write store under another path of
// a repository: a hard link of its bytes file with a properties file of its
// own (section Layout of the phase 4 plan, row Copy), for the tag of a
// manifest and for a mount. ingest has no such mode; this follows its order
// for all that comes after a body: the policies, the lock of repository and
// path, the policies again, the blob, the row, the catalog hook and the
// delete mark of the blob it replaced. digests are those of the bytes as far
// as the caller knows them; the SHA-1 always comes from the source. addOnly
// refuses a path that exists under the lock, as ingest does.
func (s *Server) linkCopy(repo repos.Repo, path string, source *index.Asset, contentType string, digests stores.Digests, addOnly bool,
	subject *model.Subject, address string) (*ingestResult, error) {
	if s.writes.writer == nil {
		return nil, errors.New("writes are off")
	}
	if source.Store != s.writes.writer.Name() {
		return nil, errors.New("a copy is a hard link of a blob of the write store")
	}
	settings, err := s.writeSettingsOf(repo)
	if err != nil {
		return nil, err
	}
	exempt := repo.Format == "docker" && settings.LatestRedeploy && policy.LatestTag(path)
	in := ingestRequest{repo: repo, path: path}
	if _, err := s.checkWrite(&in, settings, exempt); err != nil {
		return nil, err
	}
	unlock := s.writes.locks.lock(repo.Name, path)
	defer unlock()
	row, err := s.checkWrite(&in, settings, exempt)
	if err != nil {
		return nil, err
	}
	if addOnly && row != nil {
		return nil, editRefused()
	}
	uploader := s.uploader(subject)
	attributes, err := s.bounds(repo.Name, path, stores.Attributes{Repo: repo.Name, Path: path, ContentType: contentType,
		CreatedBy: uploader, CreatedByIP: address})
	if err != nil {
		return nil, err
	}
	var written stores.Written
	replaced, err := s.rp.index.PutAfter(s.writes.writer.Name(), indexWaitOf(), func() (stores.Entry, error) {
		var err error
		written, err = s.writes.writer.Link(source.Location, attributes)
		return written.Entry, err
	})
	if err != nil {
		return nil, s.writes.rowRefused(written, err)
	}
	replaced = replacedBlob(replaced, written.Entry)
	digests.SHA1 = written.Digests.SHA1
	asset := assetOf(s.writes.writer.Name(), written.Entry)
	if s.writes.catalog != nil {
		if err := s.writes.catalog.Put(asset, digests, uploader, address); err != nil {
			logAt(slogWarn, serverLogger, "the catalog row of %s/%s is not written: %v", repo.Name, path, err)
		}
	}
	if replaced != nil {
		s.writes.markReplaced(repo.Name, path, replaced.Location)
	}
	s.writes.retryPath(repo.Name, path)
	s.writes.forgetDeleted(repo.Name, path)
	return &ingestResult{asset: asset, replaced: replaced, digests: digests}, nil
}

// heldRows are the rows of the paths that lie in a store this server does
// not write; a delete of several paths checks them all before it erases
// one, so that a refusal leaves every path as it was.
func (s *Server) heldRows(repo string, paths []string) (*index.Asset, error) {
	for _, path := range paths {
		rows, err := s.rp.index.Rows(repo, path)
		if err != nil {
			return nil, err
		}
		for i := range rows {
			if rows[i].Store != s.writes.writer.Name() {
				return &rows[i], nil
			}
		}
	}
	return nil, nil
}

// readOnlyRefusal is the 409 of erase for a path of a read only store.
func readOnlyRefusal(repo string, row *index.Asset) error {
	return writeRefused(http.StatusConflict, ruleReadOnlyStore, "%s of repository %s lies in store %s, which is read only",
		row.Path, repo, row.Store)
}
