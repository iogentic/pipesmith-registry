package server

import (
	"net/http"

	"github.com/iogentic/pipesmith-registry/internal/npm"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// npmDistTags is GET /-/package/<name>/dist-tags of an npm repository or
// group while writes are on: the dist-tags object of the document the read
// path serves for the package, which npm dist-tag reads before it writes
// (npm/cli, branch latest, lib/commands/dist-tag.js, fetchTags, read
// 2026-11-18). 404 for a package without a version. With writes off the
// path keeps the npm 404 of 0.1.1.
func (s *Server) npmDistTags(w http.ResponseWriter, req *request, st *state, members []repos.Repo, name string) error {
	document, err := s.rp.npmMerged(st, members, npm.PackagePath{Name: name})
	if err != nil {
		return err
	}
	if document == nil || document.Len() == 0 {
		npmNotFound(w, req)
		return nil
	}
	tags, ok := field(document, "dist-tags").(*pyjson.Object)
	if !ok {
		npmNotFound(w, req)
		return nil
	}
	data, err := pyjson.Dumps(tags)
	if err != nil {
		return err
	}
	npmJSON(w, req, http.StatusOK, data)
	return nil
}
