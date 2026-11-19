package server

import (
	"bytes"
	"crypto/sha1" // #nosec G505 -- compares with the SHA-1 the index holds of a stored manifest, not a security use.
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/docker"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/policy"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The manifests and the deletes of the registry protocol (task 14 of the
// phase 4 plan; OCI distribution spec v1.1.1, sections Pushing Manifests
// and Content Management, read 2026-11-18).

func sha1HexOf(data []byte) string {
	sum := sha1.Sum(data) // #nosec G401 -- see the import.
	return hex.EncodeToString(sum[:])
}

// sameBytes reports a row that holds data: its size and SHA-1.
func sameBytes(row *index.Asset, data []byte) bool {
	return row != nil && row.Size == int64(len(data)) && row.SHA1 != nil && strings.EqualFold(*row.SHA1, sha1HexOf(data))
}

// putManifest is PUT <name>/manifests/<reference> (task 14): the manifest
// is checked, the blobs and child manifests it names must be in the
// repository, and it is stored as the client sent it under its digest and,
// for a tag, under the tag as a hard link of the digest blob. A path that
// holds these bytes already is not written again.
func (s *Server) putManifest(w *recorder, req *request, repo repos.Repo, settings repos.WriteSettings, call *docker.V2Path) error {
	tooLarge := func() error {
		return s.dockerManifestTooLarge(w, req)
	}
	if req.r.ContentLength > docker.MaxManifestBytes {
		return tooLarge()
	}
	body := newTimedBody(w, req)
	data, err := io.ReadAll(io.LimitReader(body, docker.MaxManifestBytes+1))
	if err != nil {
		if status, cut := body.cut(); cut {
			logAt(slogWarn, serverLogger, "PUT of a manifest of %s/%s: the body did not arrive in full, nothing is written: %v", repo.Name, call.Name, body.err)
			w.Header().Set("Connection", "close")
			dockerError(w, req, status, "MANIFEST_INVALID", "the body did not arrive in full; nothing is written", nil)
			return nil
		}
		return err
	}
	if len(data) > docker.MaxManifestBytes {
		return tooLarge()
	}
	mediaType, refs, bad := docker.Manifest(data, req.r.Header.Get("Content-Type"))
	if bad != nil {
		dockerError(w, req, http.StatusBadRequest, bad.Code, bad.Message, nil)
		return nil
	}
	digest := docker.DigestOf(data)
	if docker.IsDigest(call.Reference) {
		algorithm, _ := docker.DigestParts(call.Reference)
		actual, err := docker.DigestOfAlgorithm(data, algorithm)
		if err != nil {
			return err
		}
		if actual != call.Reference {
			dockerError(w, req, http.StatusBadRequest, "DIGEST_INVALID", fmt.Sprintf("the manifest has the digest %s, the path names %s", actual, call.Reference), nil)
			return nil
		}
		digest = call.Reference
	}
	for _, ref := range refs {
		path := dockerBlobPrefix + ref.Digest
		if ref.Manifest {
			path = "v2/" + call.Name + "/manifests/" + ref.Digest
		}
		row, err := s.rp.index.Lookup(repo.Name, path)
		if err != nil {
			return err
		}
		if row == nil {
			what := "blob"
			if ref.Manifest {
				what = "manifest"
			}
			dockerError(w, req, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN",
				fmt.Sprintf("the manifest names the %s %s, which repository %s does not hold", what, ref.Digest, repo.Name), nil)
			return nil
		}
	}
	tagPath := ""
	var tagRow *index.Asset
	if !docker.IsDigest(call.Reference) {
		tagPath = "v2/" + call.Name + "/manifests/" + call.Reference
		if tagRow, err = s.rp.index.Lookup(repo.Name, tagPath); err != nil {
			return err
		}
		exempt := settings.LatestRedeploy && policy.LatestTag(tagPath)
		if !sameBytes(tagRow, data) {
			// A refused tag leaves no digest blob behind.
			if err := policy.Write(settings.WritePolicy, tagRow != nil, exempt); err != nil {
				return s.dockerFailed(w, req, refusalOf(err))
			}
		}
	}
	subject, err := s.subjectOf(req)
	if err != nil {
		return err
	}
	address := peerAddress(w)
	digestPath := "v2/" + call.Name + "/manifests/" + digest
	digestRow, err := s.rp.index.Lookup(repo.Name, digestPath)
	if err != nil {
		return err
	}
	var digests stores.Digests
	if !sameBytes(digestRow, data) {
		algorithm, value := docker.DigestParts(digest)
		result, err := s.ingest(ingestRequest{repo: repo, path: digestPath, body: bytes.NewReader(data), size: int64(len(data)),
			digests: map[string]string{algorithm: value}, contentType: mediaType, exempt: true, subject: subject, address: address})
		if err != nil {
			return s.dockerFailed(w, req, err)
		}
		digestRow, digests = &result.asset, result.digests
	}
	if tagPath != "" && !sameBytes(tagRow, data) {
		// a tag that another push made meanwhile asks edit (finding S3 of review 4)
		addOnly := !s.mayEdit(subject, repo)
		if digestRow.Store == s.writes.writer.Name() {
			_, err = s.linkCopy(repo, tagPath, digestRow, mediaType, digests, addOnly, subject, address)
		} else {
			_, err = s.ingest(ingestRequest{repo: repo, path: tagPath, body: bytes.NewReader(data), size: int64(len(data)),
				contentType: mediaType, addOnly: addOnly, subject: subject, address: address})
		}
		if err != nil {
			return s.dockerFailed(w, req, err)
		}
	}
	writeBody(w, req, http.StatusCreated, [][2]string{{"Location", dockerBase(req, repo) + "/" + call.Name + "/manifests/" + digest},
		{docker.DigestHeader, digest}, {docker.APIVersionHeader, docker.APIVersion}}, nil)
	return nil
}

// dockerManifestTooLarge is the 413 of a manifest above the limit the read
// path serves; the rest of the body is not read.
func (s *Server) dockerManifestTooLarge(w *recorder, req *request) error {
	w.Header().Set("Connection", "close")
	dockerError(w, req, http.StatusRequestEntityTooLarge, "MANIFEST_INVALID",
		fmt.Sprintf("a manifest holds at most %d bytes", docker.MaxManifestBytes), nil)
	return nil
}

// deleteManifest is DELETE <name>/manifests/<reference> (task 14): by tag
// the delete mark on that tag; by digest on the digest blob and on every
// tag of the name that holds the same bytes, since a tag of a deleted
// manifest answers 404 too. Every path is checked before one is marked.
func (s *Server) deleteManifest(w *recorder, req *request, repo repos.Repo, call *docker.V2Path) error {
	path := "v2/" + call.Name + "/manifests/" + call.Reference
	row, err := s.rp.index.Lookup(repo.Name, path)
	if err != nil {
		return err
	}
	if row == nil {
		dockerError(w, req, http.StatusNotFound, "MANIFEST_UNKNOWN", fmt.Sprintf("manifest %s of %s is not known", call.Reference, call.Name), nil)
		return nil
	}
	paths := []string{path}
	if docker.IsDigest(call.Reference) {
		tags, err := s.tagsOfBytes(repo, call.Name, row)
		if err != nil {
			return err
		}
		paths = append(tags, path)
	}
	if err := s.erasePaths(repo, paths); err != nil {
		return s.dockerFailed(w, req, err)
	}
	writeBody(w, req, http.StatusAccepted, registryHeaders, nil)
	return nil
}

// tagsOfBytes are the tag paths of a name whose rows hold the bytes of row:
// size and SHA-1 of the index.
func (s *Server) tagsOfBytes(repo repos.Repo, name string, row *index.Asset) ([]string, error) {
	directory := "v2/" + name + "/manifests"
	children, err := s.rp.allChildren(repo.Name, directory)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range children {
		if c.IsDir || docker.IsDigest(c.Name) {
			continue
		}
		tag, err := s.rp.index.Lookup(repo.Name, directory+"/"+c.Name)
		if err != nil {
			return nil, err
		}
		if tag != nil && tag.Size == row.Size && tag.SHA1 != nil && row.SHA1 != nil && strings.EqualFold(*tag.SHA1, *row.SHA1) {
			out = append(out, tag.Path)
		}
	}
	return out, nil
}

// erasePaths erases every path, after checking that none lies in a read
// only store.
func (s *Server) erasePaths(repo repos.Repo, paths []string) error {
	held, err := s.heldRows(repo.Name, paths)
	if err != nil {
		return err
	}
	if held != nil {
		return readOnlyRefusal(repo.Name, held)
	}
	for _, path := range paths {
		if _, err := s.erase(repo, path); err != nil {
			return err
		}
	}
	return nil
}

// deleteBlob is DELETE <name>/blobs/<digest>: the delete mark on the blob
// of the repository, which every name of it shares.
func (s *Server) deleteBlob(w *recorder, req *request, repo repos.Repo, call *docker.V2Path) error {
	removed, err := s.erase(repo, dockerBlobPrefix+call.Reference)
	if err != nil {
		return s.dockerFailed(w, req, err)
	}
	if removed == nil {
		dockerError(w, req, http.StatusNotFound, "BLOB_UNKNOWN", fmt.Sprintf("blob %s is not known", call.Reference), nil)
		return nil
	}
	writeBody(w, req, http.StatusAccepted, registryHeaders, nil)
	return nil
}
