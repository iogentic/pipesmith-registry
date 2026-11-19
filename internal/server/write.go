package server

import (
	"net/http"

	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// The write dispatch (seam Write dispatch of the phase 4 execution plan):
// every PUT, POST, DELETE and PATCH below /repository/, and on a Docker
// port, comes to (*Server).write. The handlers of the formats live in
// write_raw.go, write_maven.go, write_npm.go and write_docker.go; each of
// them answers the refusal of the port until its leg builds it.

// write answers the 405 of the port while writes are off (W17). With writes
// on it hands a write to a hosted repository to the handler of its access
// kind: maven, npm, docker, and files for the format raw. A write to a
// group or a proxy (P10), by its definition or by its stored record, to a
// repository that its record says is offline, to a repository of another
// format or to no repository keeps the refusal of the port.
func (s *Server) write(w *recorder, req *request, match []string) error {
	if s.writes.writer == nil {
		return s.refuseWrite(w, req, match)
	}
	t, err := parseTarget(req.raw, repositoryPrefix)
	if err != nil || t == nil || t.bare {
		return s.refuseWrite(w, req, match)
	}
	st, err := s.rp.sync()
	if err != nil {
		return err
	}
	repo, ok := st.repositories.Get(t.repo)
	if !ok || !repo.TakesWrites() {
		return s.refuseWrite(w, req, match)
	}
	switch repos.AccessOf(repo) {
	case "maven":
		return s.writeMaven(w, req, st, repo, t)
	case "npm":
		return s.writeNpm(w, req, st, repo, t)
	case "docker":
		return s.writeDocker(w, req, st, repo, t)
	case "files":
		if repo.Format == "raw" {
			return s.writeRaw(w, req, st, repo, t)
		}
	}
	return s.refuseWrite(w, req, match)
}

// writeAction is the action a request below /repository/ asks of the view
// permission while writes are on, by the rule of docs/plans/phases.md,
// section Requests: DELETE asks delete; PUT, POST and PATCH ask edit for a
// path the repository holds and add for a new one; GET and HEAD ask read.
// With writes off it is not asked: every method asks read, as in 0.1.1,
// and the write route answers the refusal of the port.
func (s *Server) writeAction(method string, t *target) (string, error) {
	if action, ok, err := s.ociAction(method, t); ok || err != nil {
		return action, err // npm and Docker paths that change another stored path (write_oci.go)
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		return "read", nil
	case http.MethodDelete:
		return "delete", nil
	}
	if t.path == "" {
		return "add", nil
	}
	row, err := s.rp.index.Lookup(t.repo, t.path)
	if err != nil {
		return "", err
	}
	if row != nil {
		return "edit", nil
	}
	return "add", nil
}
