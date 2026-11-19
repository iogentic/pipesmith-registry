package server

import (
	"net/http"

	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// writeRaw takes a write to a hosted raw repository (task 7 of the phase 4
// plan): PUT of a file by its path, 201; DELETE by path, 204, or 404 when
// no store holds the path. Any other method answers 405 with the methods
// the repository takes.
func (s *Server) writeRaw(w *recorder, req *request, _ *state, repo repos.Repo, t *target) error {
	switch req.r.Method {
	case http.MethodPut:
		result, err := s.putFile(w, req, repo, t)
		if err == nil && result != nil {
			created(w, req)
		}
		return err
	case http.MethodDelete:
		_, err := s.deleteFile(w, req, repo, t)
		return err
	}
	methodRefused(w, req, fileWriteMethods)
	return nil
}
