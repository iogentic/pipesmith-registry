package server

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/iogentic/pipesmith-registry/internal/docker"
	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/npm"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// The REST writes of the components and assets area (task 15 of the phase
// 4 plan, leg 4d): the component upload (rest_upload.go), the asset delete,
// the component delete and the two upload field lists, in the table while
// writes are on (writingRoutes). Every file goes through ingest and every
// delete through erase, so the blob layout, the policies and the limits are
// those of a client write. With writes off these requests answer with the
// refusals of writeRefusals, and the component delete, which the contract of
// the interface does not name, with the 404 of 0.1.1.
//
// The answers follow the public documentation of the old product, pages
// Components API and Assets API (read 2026-11-18): 204 for an upload and a
// delete. Every refusal status is provisional (W6), with the shapes of the
// read routes: {"message"} and, for the fields of an upload, the list of
// {"id", "message"} the interface contract reads.

var contentWriteRoutes = []route{
	get(restBase+"/formats/upload-specs", signedIn, plain(func(*rest.Env) (rest.Answer, error) { return rest.UploadSpecs(), nil })),
	get(restBase+"/formats/"+segment+"/upload-specs", signedIn, named(func(_ *rest.Env, format string) (rest.Answer, error) {
		return rest.UploadSpec(format), nil
	})),
	{exact(restBase + "/components"), []string{"POST"}, rule{kind: ruleContent, action: "add"}, (*Server).uploadComponent},
	{exact(restBase + "/assets/" + segment), []string{"DELETE"}, rule{kind: ruleContent, group: 1, action: "delete"}, (*Server).deleteAsset},
	{exact(restBase + "/components/" + segment), []string{"DELETE"}, rule{kind: ruleContent, group: 1, action: "delete"},
		(*Server).deleteComponent},
}

// allowContent is the rule of a REST write: the view permission of the
// repository the request names, with the action of the rule. An asset id or
// a component id names it in the path, an upload in the query parameter
// repository. A request whose repository the rule cannot read goes on to
// the handler, which refuses it with 422.
func (s *Server) allowContent(w *recorder, req *request, subject *model.Subject, r rule, match []string) (bool, error) {
	var name string
	if r.group > 0 {
		repo, ok := rest.IDRepository(match[r.group])
		if !ok {
			return true, nil
		}
		name = repo
	} else {
		repo, ok := uploadRepository(req)
		if !ok {
			return true, nil
		}
		name = repo
	}
	format, err := s.formatOf(name)
	if err != nil {
		return false, err
	}
	if s.sec.Permits(subject, rest.ViewPermission(format, name, r.action)) {
		return true, nil
	}
	s.message(w, req, http.StatusForbidden, repositoryRefusal)
	return false, nil
}

// uploadRepository is the one value of the query parameter repository.
func uploadRepository(req *request) (string, bool) {
	q, err := url.ParseQuery(req.query)
	if err != nil {
		return "", false
	}
	names := q["repository"]
	if len(names) != 1 || names[0] == "" {
		return "", false
	}
	return names[0], true
}

// hostedTarget is the hosted repository a REST write names; false means
// the answer is written: 404 for a repository that does not exist, 400 for
// a group or a proxy, by its definition or its stored record, which take no
// writes (P10), and for a repository its record says is offline.
func (s *Server) hostedTarget(w *recorder, req *request, name, missing string) (repos.Repo, *state, bool, error) {
	st, err := s.rp.sync()
	if err != nil {
		return repos.Repo{}, nil, false, err
	}
	repo, found := st.repositories.Get(name)
	if !found {
		s.message(w, req, http.StatusNotFound, missing)
		return repos.Repo{}, nil, false, nil
	}
	if !repo.TakesWrites() {
		kind := repo.Type
		if kind == "hosted" && repo.Record != nil {
			kind = repo.Record.Type
		}
		text := "repository " + repo.Name + " is a " + kind + " repository; writes go to hosted repositories only"
		if kind == "hosted" {
			text = "repository " + repo.Name + " is offline; it takes no writes"
		}
		s.message(w, req, http.StatusBadRequest, text)
		return repos.Repo{}, nil, false, nil
	}
	return repo, st, true, nil
}

// restWriteFailed answers a delete that erase or a rule refused, with the
// status of section Refusals of the phase 4 plan and its text. Any other
// error is one of the server.
func (s *Server) restWriteFailed(w *recorder, req *request, err error) error {
	var refusal *writeRefusal
	if errors.As(err, &refusal) {
		retryAfter(w, refusal)
		s.message(w, req, refusal.status, refusal.message)
		return nil
	}
	var rule *npm.Refusal
	if errors.As(err, &rule) {
		s.message(w, req, http.StatusBadRequest, rule.Message)
		return nil
	}
	return err
}

// deleteAsset is DELETE /v1/assets/{id}: the delete mark on the blob of the
// path the id names, through erase. 422 for an id this server cannot have
// made, 404 when no store holds the path.
func (s *Server) deleteAsset(w *recorder, req *request, match []string) error {
	repoName, path, valid := rest.DecodeAssetID(match[1])
	if !valid {
		s.message(w, req, http.StatusUnprocessableEntity, "the asset id is malformed")
		return nil
	}
	repo, _, ok, err := s.hostedTarget(w, req, repoName, "asset not found")
	if err != nil || !ok {
		return err
	}
	removed, err := s.erase(repo, path)
	if err != nil {
		return s.restWriteFailed(w, req, err)
	}
	if removed == nil {
		s.message(w, req, http.StatusNotFound, "asset not found")
		return nil
	}
	noContent(w)
	return nil
}

// deleteComponent is DELETE /v1/components/{id}: the delete mark on every
// asset of the component, through erase, after a check that none lies in a
// read only store. A maven2 component gets the metadata of its artifact
// written again; an npm version leaves the package document, which goes
// with its last version; a Docker manifest by digest takes the tags of the
// same bytes with it, as the delete of the registry protocol does.
func (s *Server) deleteComponent(w *recorder, req *request, match []string) error {
	repoName, _, valid := rest.DecodeComponentID(match[1])
	if !valid {
		s.message(w, req, http.StatusUnprocessableEntity, "the component id is malformed")
		return nil
	}
	repo, st, ok, err := s.hostedTarget(w, req, repoName, "component not found")
	if err != nil || !ok {
		return err
	}
	env, err := s.restEnv(req)
	if err != nil {
		return err
	}
	c, err := rest.FindComponent(env, match[1])
	if err != nil {
		return err
	}
	if c == nil {
		s.message(w, req, http.StatusNotFound, "component not found")
		return nil
	}
	subject, err := s.subjectOf(req)
	if err != nil {
		return err
	}
	switch repo.Format {
	case "npm":
		err = s.deleteNpmVersion(st, repo, c, subject, peerAddress(w))
	case "docker":
		err = s.deleteDockerComponent(repo, c)
	default:
		err = s.erasePaths(repo, c.Paths)
	}
	if err != nil {
		return s.restWriteFailed(w, req, err)
	}
	if repo.Format == "maven2" {
		s.rebuildAfterDelete(repo, c, subject, peerAddress(w))
	}
	noContent(w)
	return nil
}

// rebuildAfterDelete writes the metadata of the artifact of a deleted
// maven2 component again (W15 of the phase 4 plan), so that it no longer
// lists the version; metadata no version supports goes. The delete is done
// when it runs, so a failure is logged with the command that repeats it.
func (s *Server) rebuildAfterDelete(repo repos.Repo, c *rest.Component, subject *model.Subject, address string) {
	_, err := s.RebuildMavenMetadata(repo.Name, maven.Scope{GroupID: c.Group, ArtifactID: c.Name}, subject, address)
	if err != nil {
		logAt(slogWarn, serverLogger, "the metadata of %s:%s in %s is not written again after the delete of %s: %v; run maven rebuild-metadata %s",
			c.Group, c.Name, repo.Name, c.Version, err, repo.Name)
	}
}

// deleteNpmVersion deletes one version of a package: its tarball, and its
// entry, tags and time in the package document; the document goes when no
// version stays.
func (s *Server) deleteNpmVersion(st *state, repo repos.Repo, c *rest.Component, subject *model.Subject, address string) error {
	name := c.Name
	if c.HasGroup {
		name = "@" + c.Group + "/" + c.Name
	}
	unlock := s.npmLock(repo, name)
	defer unlock()
	stored, err := s.storedDocument(st, repo, name)
	if err != nil {
		return err
	}
	paths := c.Paths
	var changed *pyjson.Object
	if stored != nil {
		if versions, ok := field(stored, "versions").(*pyjson.Object); ok && versions.Has(c.Version) {
			if changed, err = npm.RemoveVersions(stored, []string{c.Version}, nowISO()); err != nil {
				return err
			}
			if left, ok := field(changed, "versions").(*pyjson.Object); !ok || left.Len() == 0 {
				paths, changed = append(paths, name), nil
			}
		}
	}
	if err := s.erasePaths(repo, paths); err != nil {
		return err
	}
	if changed == nil {
		return nil
	}
	_, err = s.writeDocument(repo, name, changed, npm.Rev(stored), subject, address)
	return err
}

// deleteDockerComponent deletes the manifest of a component: by tag that
// tag, by digest the digest and every tag of the name with the same bytes.
func (s *Server) deleteDockerComponent(repo repos.Repo, c *rest.Component) error {
	paths := c.Paths
	if docker.IsDigest(c.Version) {
		row, err := s.rp.index.Lookup(repo.Name, "v2/"+c.Name+"/manifests/"+c.Version)
		if err != nil {
			return err
		}
		if row != nil {
			tags, err := s.tagsOfBytes(repo, c.Name, row)
			if err != nil {
				return err
			}
			paths = append(tags, paths...)
		}
	}
	return s.erasePaths(repo, paths)
}
