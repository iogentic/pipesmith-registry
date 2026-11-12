package server

import (
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"

	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/docker"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/pystr"
	"github.com/mirosso25/pipesmith-registry/internal/repos"
	"github.com/mirosso25/pipesmith-registry/internal/stores"
)

var registryHeaders = [][2]string{{docker.APIVersionHeader, docker.APIVersion}}

// dockerJSON is _docker_json: a 200 JSON answer of the registry.
func dockerJSON(w http.ResponseWriter, req *request, data []byte, extra [][2]string) {
	headers := [][2]string{{"Content-Type", jsonType}, {docker.APIVersionHeader, docker.APIVersion}}
	writeBody(w, req, http.StatusOK, append(headers, extra...), data)
}

// dockerError is _docker_error: the error document of the registry spec
// with one error.
func dockerError(w http.ResponseWriter, req *request, status int, code, message string, extra [][2]string) {
	headers := [][2]string{{"Content-Type", jsonType}, {docker.APIVersionHeader, docker.APIVersion},
		{"X-Content-Type-Options", "nosniff"}}
	writeBody(w, req, status, append(headers, extra...), docker.ErrorBody(code, message))
}

func dockerMembers(r *repos.Repositories, name string) []repos.Repo {
	var out []repos.Repo
	for _, m := range r.Resolve(name) {
		if repos.AccessOf(m) == "docker" {
			out = append(out, m)
		}
	}
	return out
}

// docker is one call of the registry read path; t.path is "v2" or starts
// with "v2/".
func (s *Server) docker(w http.ResponseWriter, req *request, st *state, repo repos.Repo, members []repos.Repo, t *target) error {
	rest := ""
	if t.path != "v2" {
		rest = t.path[len("v2/"):]
	}
	if t.directory && rest != "" {
		rest += "/"
	}
	call, err := docker.ParseV2(rest)
	if err != nil {
		var pathErr *docker.PathError
		if errors.As(err, &pathErr) {
			dockerError(w, req, http.StatusBadRequest, pathErr.Code, pathErr.Message, nil)
			return nil
		}
		return err
	}
	if call == nil {
		dockerError(w, req, http.StatusNotFound, "UNSUPPORTED", fmt.Sprintf("/v2/%s is not a call of the registry read path", rest), nil)
		return nil
	}
	switch call.Kind {
	case "base":
		dockerJSON(w, req, []byte("{}"), nil)
		return nil
	case "blob":
		return s.dockerBlob(w, req, st, members, call)
	}
	known, err := s.rp.anyIsDir(members, "v2/"+call.Name+"/manifests")
	if err != nil {
		return err
	}
	if !known {
		dockerError(w, req, http.StatusNotFound, "NAME_UNKNOWN", fmt.Sprintf("repository name %s is not known", call.Name), nil)
		return nil
	}
	if call.Kind == "tags" {
		return s.dockerTags(w, req, repo, members, call)
	}
	return s.dockerManifest(w, req, st, members, call)
}

// dockerManifest is _docker_manifest: the stored bytes; by digest only when
// they hash to the digest, except a signed schema 1 manifest. A member whose
// bytes do not answer the reference is passed over.
func (s *Server) dockerManifest(w http.ResponseWriter, req *request, st *state, members []repos.Repo, call *docker.V2Path) error {
	path := "v2/" + call.Name + "/manifests/" + call.Reference
	for _, member := range members {
		asset, f, size, err := s.rp.open(st, member.Name, path)
		if err != nil {
			return err
		}
		if asset == nil {
			continue
		}
		var data []byte
		if size <= docker.MaxManifestBytes {
			if data, err = readUpTo(f, docker.MaxManifestBytes+1); err != nil {
				return err
			}
		} else {
			f.Close()
		}
		if len(data) == 0 || len(data) > docker.MaxManifestBytes {
			logAt(slogWarn, serverLogger, "%s/%s in store %s has %d bytes, not served as a manifest", asset.Repo, path, asset.Store, size)
			continue
		}
		contentType := stores.ManifestMediaType(data)
		if headerSafe(asset.ContentType) {
			contentType = *asset.ContentType
		} else if contentType == "" {
			contentType = jsonType
		}
		digest := docker.DigestOf(data)
		if docker.IsDigest(call.Reference) {
			algorithm, _, _ := strings.Cut(call.Reference, ":")
			actual := digest
			if algorithm != "sha256" {
				if actual, err = docker.DigestOfAlgorithm(data, algorithm); err != nil {
					return err
				}
			}
			if actual != call.Reference && !docker.IsSchema1Signed(data, &contentType) {
				logAt(slogWarn, serverLogger, "%s/%s in store %s hashes to %s; not served under %s", asset.Repo, path, asset.Store,
					actual, call.Reference)
				continue
			}
			digest = call.Reference
		}
		created := asset.CreatedMS
		b := &body{size: int64(len(data)), etag: `"` + digest + `"`, modifiedMS: &created, contentType: contentType, data: data}
		return send(w, req, b, append([][2]string{{docker.DigestHeader, digest}}, registryHeaders...))
	}
	dockerError(w, req, http.StatusNotFound, "MANIFEST_UNKNOWN", fmt.Sprintf("manifest %s of %s is not known", call.Reference, call.Name), nil)
	return nil
}

// dockerBlob is _docker_blob: the bytes of v2/-/blobs/<digest>, shared by
// every name and not hashed here.
func (s *Server) dockerBlob(w http.ResponseWriter, req *request, st *state, members []repos.Repo, call *docker.V2Path) error {
	path := dockerBlobPrefix + call.Reference
	for _, member := range members {
		asset, f, size, err := s.rp.open(st, member.Name, path)
		if err != nil {
			return err
		}
		if asset != nil {
			b := fileBody(asset, f, size)
			b.contentType = octet
			if headerSafe(asset.ContentType) {
				b.contentType = *asset.ContentType
			}
			return send(w, req, b, append([][2]string{{docker.DigestHeader, call.Reference}}, registryHeaders...))
		}
	}
	dockerError(w, req, http.StatusNotFound, "BLOB_UNKNOWN", fmt.Sprintf("blob %s is not known", call.Reference), nil)
	return nil
}

// dockerTags is _docker_tags: the tags in code point order; n limits the
// count, last starts after the named tag.
func (s *Server) dockerTags(w http.ResponseWriter, req *request, repo repos.Repo, members []repos.Repo, call *docker.V2Path) error {
	limit, apiErr := intParam(req.params(), "n", -1, 0, nil)
	if apiErr != nil {
		dockerError(w, req, http.StatusBadRequest, "PAGINATION_NUMBER_INVALID", apiErr.message, nil)
		return nil
	}
	var names []string
	for _, member := range members {
		children, err := s.rp.allChildren(member.Name, "v2/"+call.Name+"/manifests")
		if err != nil {
			return err
		}
		for _, c := range children {
			if !c.IsDir {
				names = append(names, c.Name)
			}
		}
	}
	tags := docker.TagNames(names)
	if last, _ := req.params().get("last"); last != "" {
		kept := []string{}
		for _, tag := range tags {
			if pystr.Compare(tag, last) > 0 {
				kept = append(kept, tag)
			}
		}
		tags = kept
	}
	var extra [][2]string
	if limit.Sign() >= 0 {
		n := len(tags)
		if limit.Cmp(big.NewInt(int64(n))) < 0 {
			n = int(limit.Int64())
		}
		if limit.Sign() > 0 && n < len(tags) {
			base := repositoryPrefix + quote(repo.Name, "") + "/v2"
			if req.dockerRepo != "" {
				base = "/v2"
			}
			extra = append(extra, [2]string{"Link", fmt.Sprintf(`<%s/%s/tags/list?n=%d&last=%s>; rel="next"`, base, call.Name, n,
				quote(tags[n-1], ""))})
		}
		tags = tags[:n]
	}
	list := make([]any, len(tags))
	for i, tag := range tags {
		list[i] = tag
	}
	data, err := pyjson.Dumps(pyjson.NewObject().Set("name", call.Name).Set("tags", list))
	if err != nil {
		return err
	}
	dockerJSON(w, req, data, extra)
	return nil
}

// dockerAsset is _docker_asset: image, reference and pull command of a
// stored Docker path, nil for a path outside the registry layout. port is
// the port of the configuration, openPort the one this process listens on;
// only the open port carries a pull command.
func (s *Server) dockerAsset(st *state, member, path string, port, openPort *int) (*pyjson.Object, error) {
	object := func(kind string, image, tag, digest any, pull any) *pyjson.Object {
		var p any
		if port != nil {
			p = *port
		}
		return pyjson.NewObject().Set("kind", kind).Set("image", image).Set("tag", tag).Set("digest", digest).
			Set("port", p).Set("pull", pull)
	}
	if strings.HasPrefix(path, dockerBlobPrefix) {
		digest := path[len(dockerBlobPrefix):]
		if !docker.IsDigest(digest) {
			return nil, nil
		}
		return object("blob", nil, nil, digest, nil), nil
	}
	if !strings.HasPrefix(path, "v2/") {
		return nil, nil
	}
	call, err := docker.ParseV2(path[len("v2/"):])
	if err != nil || call == nil || call.Kind != "manifest" && call.Kind != "blob" {
		return nil, nil
	}
	if call.Kind == "blob" {
		return object("blob", call.Name, nil, call.Reference, nil), nil
	}
	var tag, digest any
	reference := ""
	if docker.IsDigest(call.Reference) {
		digest, reference = call.Reference, "@"+call.Reference
	} else {
		tag, reference = call.Reference, ":"+call.Reference
		found, err := s.manifestDigest(st, member, path)
		if err != nil {
			return nil, err
		}
		if found != "" {
			digest = found
		}
	}
	var pull any
	if openPort != nil && reference != "" {
		pull = fmt.Sprintf("docker pull %s:%d/%s%s", s.pullHost(), *openPort, call.Name, reference)
	}
	return object("manifest", call.Name, tag, digest, pull), nil
}

// manifestDigest is the SHA256 of the stored bytes, "" when unreadable.
func (s *Server) manifestDigest(st *state, repo, path string) (string, error) {
	asset, f, size, err := s.rp.open(st, repo, path)
	var un *unavailable
	if errors.As(err, &un) {
		logAt(slogWarn, serverLogger, "%s", un.message)
		return "", nil
	}
	if err != nil || asset == nil {
		return "", err
	}
	var data []byte
	if size <= docker.MaxManifestBytes {
		if data, err = readUpTo(f, docker.MaxManifestBytes+1); err != nil {
			return "", err
		}
	} else {
		f.Close()
	}
	if len(data) == 0 || len(data) > docker.MaxManifestBytes {
		return "", nil
	}
	return docker.DigestOf(data), nil
}

// pullHost is the host of a pull command: the first host name when the
// server listens on an address that is not local, else the bound address.
func (s *Server) pullHost() string {
	if len(s.hostNames) > 0 && !slices.Contains(config.LocalBinds(), s.rp.cfg.Bind) {
		return s.hostNames[0]
	}
	return pullHost(s.rp.cfg.Bind)
}

// pullHost is the host of a pull command for the bound address.
func pullHost(bind string) string {
	switch bind {
	case "::1":
		return "[::1]"
	case "localhost":
		return "127.0.0.1"
	}
	return bind
}
