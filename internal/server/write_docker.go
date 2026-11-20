package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/docker"
	"github.com/iogentic/pipesmith-registry/internal/policy"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// writeDocker takes a write to a hosted docker repository: upload sessions,
// mounts, manifests and deletes of the registry protocol (tasks 13 and 14
// of the phase 4 plan, leg 4c).
func (s *Server) writeDocker(w *recorder, req *request, st *state, repo repos.Repo, t *target) error {
	return s.dockerWrite(w, req, st, repo, t)
}

// dockerCalls are the methods a hosted docker repository takes with writes
// on, by the kind of call, with the Allow header of a 405.
var dockerCalls = map[string]struct {
	methods map[string]bool
	allow   string
}{
	"upload-start": {map[string]bool{"POST": true}, "POST"},
	"upload":       {map[string]bool{"PATCH": true, "PUT": true, "DELETE": true}, "GET, PATCH, PUT, DELETE"},
	"manifest":     {map[string]bool{"PUT": true, "DELETE": true}, "GET, HEAD, PUT, DELETE"},
	"blob":         {map[string]bool{"DELETE": true}, "GET, HEAD, DELETE"},
	"tags":         {map[string]bool{}, "GET, HEAD"},
	"base":         {map[string]bool{}, "GET, HEAD"},
	"referrers":    {map[string]bool{}, "GET, HEAD"},
}

// dockerWrite is the dispatch of writeDocker: a path outside v2/ keeps the
// refusal of the port; below it the call of the registry protocol, under
// the write policy of the repository (section Refusals of the phase 4
// plan: read only is 405 with UNSUPPORTED for Docker).
func (s *Server) dockerWrite(w *recorder, req *request, st *state, repo repos.Repo, t *target) error {
	if t.path != "v2" && !strings.HasPrefix(t.path, "v2/") {
		return s.refuseWrite(w, req, nil)
	}
	rest := ""
	if t.path != "v2" {
		rest = t.path[len("v2/"):]
	}
	if t.directory && rest != "" {
		rest += "/"
	}
	call, err := docker.ParseWrite(rest)
	if call == nil && err == nil {
		call, err = docker.ParseReferrers(rest) // GET only, a 405 here
	}
	if err != nil {
		var pathErr *docker.PathError
		if errors.As(err, &pathErr) {
			dockerError(w, req, http.StatusBadRequest, pathErr.Code, pathErr.Message, nil)
			return nil
		}
		return err
	}
	if call == nil {
		dockerError(w, req, http.StatusNotFound, "UNSUPPORTED", fmt.Sprintf("/v2/%s is not a call of the registry protocol", rest), nil)
		return nil
	}
	kind := call.Kind
	if kind == "upload" && call.Reference == "" {
		kind = "upload-start"
	}
	method := req.r.Method
	if c := dockerCalls[kind]; !c.methods[method] {
		dockerError(w, req, http.StatusMethodNotAllowed, "UNSUPPORTED", fmt.Sprintf("%s is not a call of the registry protocol for this path", method),
			[][2]string{{"Allow", c.allow}})
		return nil
	}
	settings, err := s.writeSettingsOf(repo)
	if err != nil {
		return s.dockerFailed(w, req, err)
	}
	if err := policy.Write(settings.WritePolicy, false, false); err != nil {
		return s.dockerFailed(w, req, refusalOf(err))
	}
	switch kind {
	case "upload-start":
		return s.startUpload(w, req, st, repo, settings, call)
	case "upload":
		switch method {
		case http.MethodPatch:
			return s.patchUpload(w, req, repo, settings, call)
		case http.MethodPut:
			return s.finishUpload(w, req, repo, settings, call)
		}
		return s.cancelUpload(w, req, repo, call)
	case "manifest":
		if method == http.MethodPut {
			return s.putManifest(w, req, repo, settings, call)
		}
		return s.deleteManifest(w, req, repo, call)
	}
	return s.deleteBlob(w, req, repo, call)
}

// dockerFailed answers a write that ingest, erase or a check refused, in the
// error document of the registry protocol, with the codes of section
// Refusals of the phase 4 plan; the codes no public page states for these
// cases are provisional (W6). Any other error is one of the server.
func (s *Server) dockerFailed(w *recorder, req *request, err error) error {
	var refusal *writeRefusal
	if !errors.As(err, &refusal) {
		return err
	}
	status, code := refusal.status, "UNSUPPORTED"
	var extra [][2]string
	switch refusal.rule {
	case string(policy.RuleReadOnly):
		status, code = http.StatusMethodNotAllowed, "UNSUPPORTED"
		extra = [][2]string{{"Allow", allow}}
	case string(policy.RuleRedeploy):
		code = "DENIED"
	case ruleDigest:
		code = "DIGEST_INVALID"
	case ruleSize:
		code = "SIZE_INVALID"
	case ruleTooLarge:
		code = "SIZE_INVALID"
		w.Header().Set("Connection", "close") // the rest of the body is not read
	case ruleNoSpace:
		code = "BLOB_UPLOAD_INVALID"
	case ruleReadOnlyStore, ruleWriteStore, ruleEdit, ruleAdd:
		code = "DENIED"
	case ruleBusy:
		code = "UNAVAILABLE"
		extra = [][2]string{{"Retry-After", busyRetryAfter}}
	}
	dockerError(w, req, status, code, refusal.message, extra)
	return nil
}
