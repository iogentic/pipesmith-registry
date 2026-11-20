package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// The writes of one file by its path, which raw (write_raw.go) and maven2
// (write_maven.go) share (leg 4b of the phase 4 execution plan): PUT through
// ingest, DELETE through erase. The body streams from the connection into
// the work file of ingest, with the limit of the repository and a read
// deadline for each chunk (write_body.go).

// fileWriteMethods are the methods a hosted raw or maven2 repository takes
// with writes on.
const fileWriteMethods = "GET, HEAD, PUT, DELETE"

// controlCharacter reports a character below U+0020 or U+007F in a path.
func controlCharacter(path string) bool {
	return strings.IndexFunc(path, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0
}

// filePath refuses a write target that names no file: the root or a folder
// of the repository, or a path with a control character. False means the
// answer is written.
func filePath(w *recorder, req *request, t *target) bool {
	switch {
	case t.path == "" || t.directory:
		writeText(w, req, http.StatusBadRequest, "the path names a folder; a write needs the path of a file")
	case controlCharacter(t.path):
		writeText(w, req, http.StatusBadRequest, "the path holds a control character")
	default:
		return true
	}
	return false
}

// methodRefused is the 405 of a method a repository with writes on does
// not take.
func methodRefused(w *recorder, req *request, methods string) {
	writeBody(w, req, http.StatusMethodNotAllowed, [][2]string{{"Content-Type", textType}, {"X-Content-Type-Options", "nosniff"},
		{"Allow", methods}}, []byte("Method not allowed\n"))
}

// noContent is the answer 204 of a delete; it has no body and no length.
func noContent(w *recorder) {
	w.Header().Del("Content-Length")
	w.WriteHeader(http.StatusNoContent)
}

// putFile is the PUT of one file: its body through ingest. The caller
// answers 201 once it has the result; a nil result means the answer is
// written, a refusal.
func (s *Server) putFile(w *recorder, req *request, repo repos.Repo, t *target) (*ingestResult, error) {
	if !filePath(w, req, t) {
		return nil, nil
	}
	subject, err := s.subjectOf(req)
	if err != nil {
		return nil, err
	}
	body := newTimedBody(w, req)
	result, err := s.ingest(ingestRequest{repo: repo, path: t.path, body: body, size: req.r.ContentLength,
		addOnly: !s.mayEdit(subject, addressed(req, repo)), editOnly: !s.mayAdd(subject, addressed(req, repo)), subject: subject,
		address: peerAddress(w)})
	if err != nil {
		return nil, s.writeFailed(w, req, repo, t.path, body, err)
	}
	return result, nil
}

// created is the answer 201 of a PUT that wrote its blob.
func created(w *recorder, req *request) {
	writeBody(w, req, http.StatusCreated, nil, nil)
}

// deleteFile is the DELETE of one file: the delete mark through erase, 204
// once the row is gone, 404 when no store holds the path. A nil row means
// the answer is a refusal or the 404.
func (s *Server) deleteFile(w *recorder, req *request, repo repos.Repo, t *target) (*index.Asset, error) {
	if !filePath(w, req, t) {
		return nil, nil
	}
	removed, err := s.erase(repo, t.path)
	if err != nil {
		return nil, s.writeFailed(w, req, repo, t.path, nil, err)
	}
	if removed == nil {
		writeText(w, req, http.StatusNotFound, "Not found")
		return nil, nil
	}
	noContent(w)
	return removed, nil
}

// writeFailed answers a write that ingest or erase refused or could not
// do. A refusal of a rule or a limit gets its status of section Refusals
// of the phase 4 plan with its text. A body that did not arrive in full,
// because the client left or stalled, gets 400 or 408 and a warning; its
// work file is gone already. Any other error is one of the server.
func (s *Server) writeFailed(w *recorder, req *request, repo repos.Repo, path string, body *timedBody, err error) error {
	var refusal *writeRefusal
	if errors.As(err, &refusal) {
		retryAfter(w, refusal)
		writeText(w, req, refusal.status, refusal.message)
		return nil
	}
	if status, cut := body.cut(); cut {
		logAt(slogWarn, serverLogger, "%s of %s/%s: the body did not arrive in full, nothing is written: %v", req.r.Method, repo.Name, path, body.err)
		w.Header().Set("Connection", "close")
		writeText(w, req, status, "the body did not arrive in full; nothing is written")
		return nil
	}
	return err
}

// rollBack takes back the blobs that a request of several writes stored
// before a later write of it failed (note N5 of the 0.2.0 plan): a blob of a
// new path gets its delete mark through erase, so that neither the read path
// nor a scan serves it. A blob that replaced another stays: the blob it
// replaced carries its delete mark already. It is best effort, no
// transaction: an erase that fails, a busy index for one, leaves its blob
// live, and a warning names it.
func (s *Server) rollBack(repo repos.Repo, results []*ingestResult, cause error) {
	for i := len(results) - 1; i >= 0; i-- {
		path := results[i].asset.Path
		if results[i].replaced != nil {
			logAt(slogWarn, serverLogger, "%s/%s stays live: it replaced a blob, and a later write of the same request failed: %v", repo.Name, path, cause)
			continue
		}
		if _, err := s.erase(repo, path); err != nil {
			logAt(slogWarn, serverLogger, "%s/%s stays live: a later write of the same request failed (%v), and its delete mark failed: %v",
				repo.Name, path, cause, err)
		}
	}
}
