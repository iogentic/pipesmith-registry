package server

import (
	"slices"

	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/maven"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/repos"
	"github.com/mirosso25/pipesmith-registry/internal/rest"
)

// asset is GET /api/asset/<repo>?path=: one asset with what its format adds,
// the Maven coordinates, the Docker reference or the npm package; the npm
// registry URL carries the origin of the request.
func (s *Server) asset(w *recorder, req *request, match []string) error {
	name := match[1]
	var param *string
	if value, ok := req.params().get("path"); ok {
		param = &value
	}
	path, apiErr := apiPath(param)
	if apiErr != nil {
		return jsonError(w, req, apiErr.status, apiErr.message)
	}
	st, err := s.rp.sync()
	if err != nil {
		return err
	}
	repo, ok := st.repositories.Get(name)
	if !ok {
		return jsonError(w, req, 404, "repository "+name+" not found")
	}
	if path == "" {
		return jsonError(w, req, 400, "the query parameter path is required")
	}
	var found *index.Asset
	for _, member := range st.repositories.Resolve(name) {
		if found, err = s.rp.index.Lookup(member.Name, path); err != nil {
			return err
		}
		if found != nil {
			break
		}
	}
	if found == nil {
		return jsonError(w, req, 404, path+" not found in "+name)
	}
	access := repos.AccessOf(repo)
	var coordinates, npmRecord any
	if slices.Contains(coordinateFormats[:], repo.Format) {
		if c := maven.CoordinatesOf(path); c != nil {
			coordinates = coordinatesObject(c)
		}
	}
	var dockerRecord any
	served := slices.Contains(fileAccess[:], access) || access == "npm"
	if access == "docker" {
		var port, open *int
		if p, ok := s.rp.cfg.DockerPorts[repo.Name]; ok {
			port = &p
		} else if repo.HTTPPort != nil {
			p := *repo.HTTPPort
			port = &p
		}
		if p, ok := s.rp.openPorts[repo.Name]; ok {
			open = &p
		}
		record, err := s.dockerAsset(st, found.Repo, path, port, open)
		if err != nil {
			return err
		}
		if record != nil {
			dockerRecord = record
			kind, _ := record.Get("kind")
			served = served || kind == "manifest"
		}
	}
	if access == "npm" {
		if record := npmAsset(path, npmBase(req, name)); record != nil {
			npmRecord = record
		}
	}
	var downloadURL, sha1 any
	if served {
		downloadURL = repositoryPrefix + quote(name, "") + "/" + quote(path, "/")
	}
	if found.SHA1 != nil {
		sha1 = *found.SHA1
	}
	contentType := maven.ContentTypeFor(path)
	if headerSafe(found.ContentType) {
		contentType = *found.ContentType
	}
	detail := pyjson.NewObject().
		Set("repo", name).
		Set("member", found.Repo).
		Set("path", path).
		Set("name", lastSegment(path)).
		Set("size", found.Size).
		Set("sha1", sha1).
		Set("content_type", contentType).
		Set("created_ms", found.CreatedMS).
		Set("store", found.Store).
		Set("access", access).
		Set("download_url", downloadURL).
		Set("maven", coordinates).
		Set("docker", dockerRecord).
		Set("npm", npmRecord)
	if s.sec != nil {
		// the id of the same file in the REST namespace, for GET /service/rest/v1/assets/{id}
		detail.Set("asset_id", rest.AssetID(name, path))
	}
	return writeJSON(w, req, 200, detail)
}
