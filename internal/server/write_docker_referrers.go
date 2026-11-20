package server

import (
	"net/http"
	"slices"

	"github.com/iogentic/pipesmith-registry/internal/docker"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The referrers route of the OCI distribution spec v1.1.1 (section Listing
// Referrers, read 2026-11-20; W11 of the phase 4 plan). The list is read
// from the stored manifests of the name at each request; nothing else keeps
// it.

// referrersCall is GET <name>/referrers/<digest>, which the read path hands
// over while writes are on; nil for another path and with writes off, so
// that a read only server answers as before.
func (s *Server) referrersCall(rest string) (*docker.V2Path, error) {
	if !s.writes.clients {
		return nil, nil
	}
	return docker.ParseReferrers(rest)
}

// dockerReferrers answers an image index of the manifests of the name in
// the members whose subject is the digest, in digest order, each once. A
// digest nothing refers to and a name that is not there answer an empty
// index, never 404. artifactType keeps the referrers of that type and is
// named in OCI-Filters-Applied.
func (s *Server) dockerReferrers(w http.ResponseWriter, req *request, st *state, members []repos.Repo, call *docker.V2Path) error {
	filter, _ := req.params().get("artifactType")
	directory := "v2/" + call.Name + "/manifests"
	found := map[string]*pyjson.Object{}
	for _, member := range members {
		children, err := s.rp.allChildren(member.Name, directory)
		if err != nil {
			return err
		}
		for _, c := range children {
			if c.IsDir || !docker.IsDigest(c.Name) || found[c.Name] != nil {
				continue
			}
			descriptor, err := s.referrerOf(st, member, directory+"/"+c.Name, c.Name, call.Reference)
			if err != nil {
				return err
			}
			if descriptor == nil {
				continue
			}
			if filter != "" {
				if kind, _ := descriptor.Get("artifactType"); kind != filter {
					continue
				}
			}
			found[c.Name] = descriptor
		}
	}
	digests := make([]string, 0, len(found))
	for digest := range found {
		digests = append(digests, digest)
	}
	slices.Sort(digests)
	manifests := make([]any, len(digests))
	for i, digest := range digests {
		manifests[i] = found[digest]
	}
	data, err := pyjson.Dumps(pyjson.NewObject().Set("schemaVersion", int64(2)).Set("mediaType", docker.OCIIndex).Set("manifests", manifests))
	if err != nil {
		return err
	}
	headers := [][2]string{{"Content-Type", docker.OCIIndex}, {docker.APIVersionHeader, docker.APIVersion}}
	if filter != "" {
		headers = append(headers, [2]string{docker.FiltersHeader, "artifactType"})
	}
	writeBody(w, req, http.StatusOK, headers, data)
	return nil
}

// referrerOf is the descriptor of the manifest at path when its subject is
// subject, nil otherwise and for bytes that do not hash to the digest of
// the path.
func (s *Server) referrerOf(st *state, member repos.Repo, path, digest, subject string) (*pyjson.Object, error) {
	asset, f, size, err := s.rp.open(st, member.Name, path)
	if err != nil || asset == nil {
		return nil, err
	}
	if size > docker.MaxManifestBytes {
		f.Close()
		return nil, nil
	}
	data, err := readUpTo(f, docker.MaxManifestBytes+1)
	if err != nil {
		return nil, err
	}
	algorithm, _ := docker.DigestParts(digest)
	if actual, err := docker.DigestOfAlgorithm(data, algorithm); err != nil || actual != digest {
		return nil, err
	}
	mediaType := stores.ManifestMediaType(data)
	if headerSafe(asset.ContentType) {
		mediaType = *asset.ContentType
	}
	target, descriptor := docker.Referrer(data, mediaType, digest)
	if target != subject {
		return nil, nil
	}
	return descriptor, nil
}
