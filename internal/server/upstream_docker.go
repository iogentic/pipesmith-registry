package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/docker"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The registry read path through a Docker proxy, alone or as a member of a
// group (task 12 of the phase 5 plan, docker). Members are asked in order;
// a hosted member answers from its rows, a proxy member from its cache while
// that is fresh, else from its remote. A manifest by tag is fetched with
// the Accept values of the client, its SHA-256 is computed from the bytes,
// and it is stored under tag and digest as a pushed manifest is
// (putManifest). A manifest by digest and a blob must hash to the digest of
// the request (decision 5.10) and are never checked again. A blob is fetched
// under the name of the request and stored once under the shared blob path.
// HEAD of an uncached manifest answers with the digest and the content type
// of the remote and stores nothing. The tag list joins the tags the cache
// holds with the list of the remote, which the proxies keep for the
// metadata age. A failed proxy answers 502 alone and is skipped in a group,
// which answers 404 when no member has the path (decision 5.9).

// fetches reports a member that asks its remote: a proxy of a format that
// fetches, with upstream on.
func (s *Server) fetches(member repos.Repo) bool {
	return s.proxies != nil && member.Type == "proxy" && proxy.Fetches(member.Format)
}

// fetchingAny reports a member that asks its remote.
func (s *Server) fetchingAny(members []repos.Repo) bool {
	for _, m := range members {
		if s.fetches(m) {
			return true
		}
	}
	return false
}

// escapedPath is a path with each segment escaped, as the remote is asked.
func escapedPath(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// digestsOf are the digests a path named by a digest must have, by
// algorithm in hex.
func digestsOf(digest string) map[string]string {
	algorithm, value := docker.DigestParts(digest)
	return map[string]string{algorithm: value}
}

// clientAddress is the address of the client of a request, stored as
// created-by-ip of what its fetch stores (decision 5.20).
func clientAddress(w http.ResponseWriter) string {
	if rec, ok := w.(*recorder); ok {
		return peerAddress(rec)
	}
	return ""
}

// fetchMember asks the proxies for a path of a member that fetches: its
// cached copy while it is fresh or permanent, else the remote at remote,
// with the digests the bytes must have and the headers of the client; what
// arrives goes through store. left is true when the client left; the fetch
// goes on without it.
func (s *Server) fetchMember(req *request, member repos.Repo, path, remote string, digests map[string]string, header http.Header,
	store func(proxy.Fetched) error) (outcome proxy.Outcome, left bool) {
	look := s.lookFor(member.Name, path)
	cached, err := look()
	if err != nil {
		logAt(slogWarn, serverLogger, "proxy %s: %q: the index is not read: %v", member.Name, path, err)
		return proxy.Failed, false
	}
	q := proxy.Query{Repo: member, Path: path, Remote: remote, Header: header, Digests: digests, Cached: cached,
		Limit: member.WriteSettings().MaxBodyBytes, Look: look, Store: store}
	outcome, err = s.proxies.Get(req.r.Context(), q)
	return outcome, err != nil
}

// accepted are the Accept values of the client, which a manifest is asked
// for with.
func accepted(req *request) http.Header {
	if values := req.r.Header.Values("Accept"); len(values) > 0 {
		return http.Header{"Accept": values}
	}
	return nil
}

// dockerThrough is the registry call of a request whose members include a
// proxy that fetches.
func (s *Server) dockerThrough(w http.ResponseWriter, req *request, st *state, repo repos.Repo, members []repos.Repo, call *docker.V2Path) error {
	switch call.Kind {
	case "blob":
		return s.dockerBlobThrough(w, req, st, repo, members, call)
	case "tags":
		return s.dockerTagsThrough(w, req, repo, members, call)
	}
	return s.dockerManifestThrough(w, req, st, repo, members, call)
}

// dockerFailedRemote is the 502 of a Docker proxy whose remote failed and
// whose cache has no copy.
func dockerFailedRemote(w http.ResponseWriter, req *request, repo repos.Repo) {
	dockerError(w, req, http.StatusBadGateway, "UNAVAILABLE", fmt.Sprintf("the remote of repository %s failed", repo.Name), nil)
}

func (s *Server) dockerBlobThrough(w http.ResponseWriter, req *request, st *state, repo repos.Repo, members []repos.Repo,
	call *docker.V2Path) error {
	path := dockerBlobPrefix + call.Reference
	failed := false
	for _, member := range members {
		if s.fetches(member) {
			outcome, left := s.fetchMember(req, member, path, escapedPath("v2/"+call.Name+"/blobs/"+call.Reference),
				digestsOf(call.Reference), nil, s.storeTyped(member, path, octet, req.subject, clientAddress(w)))
			if left {
				return nil
			}
			if outcome == proxy.Failed {
				failed = true
			}
			if outcome == proxy.Failed || outcome == proxy.Missing {
				continue
			}
		}
		answered, err := s.blobOf(w, req, st, member, path, call)
		if err != nil || answered {
			return err
		}
	}
	if failed && repo.Type == "proxy" {
		dockerFailedRemote(w, req, repo)
		return nil
	}
	dockerError(w, req, http.StatusNotFound, "BLOB_UNKNOWN", fmt.Sprintf("blob %s is not known", call.Reference), nil)
	return nil
}

func (s *Server) dockerManifestThrough(w http.ResponseWriter, req *request, st *state, repo repos.Repo, members []repos.Repo,
	call *docker.V2Path) error {
	path := "v2/" + call.Name + "/manifests/" + call.Reference
	var digests map[string]string
	if docker.IsDigest(call.Reference) {
		digests = digestsOf(call.Reference)
	}
	failed := false
	for _, member := range members {
		if s.fetches(member) {
			cached, err := s.lookFor(member.Name, path)()
			if err != nil {
				return err
			}
			if req.head && cached == nil {
				peek, err := s.proxies.Head(req.r.Context(), proxy.Query{Repo: member, Path: path, Remote: escapedPath(path), Header: accepted(req),
					Limit: member.WriteSettings().MaxBodyBytes})
				switch {
				case err != nil:
					return nil // the client left
				case peek.Found:
					answerPeek(w, peek)
					return nil
				case peek.Failed:
					failed = true
				}
				continue
			}
			outcome, left := s.fetchMember(req, member, path, escapedPath(path), digests, accepted(req),
				s.storeManifest(member, call.Name, call.Reference, req.subject, clientAddress(w)))
			if left {
				return nil
			}
			if outcome == proxy.Failed {
				failed = true
			}
			if outcome == proxy.Failed || outcome == proxy.Missing {
				continue
			}
		}
		answered, err := s.manifestOf(w, req, st, member, path, call)
		if err != nil || answered {
			return err
		}
	}
	if failed && repo.Type == "proxy" {
		dockerFailedRemote(w, req, repo)
		return nil
	}
	dockerError(w, req, http.StatusNotFound, "MANIFEST_UNKNOWN", fmt.Sprintf("manifest %s of %s is not known", call.Reference, call.Name), nil)
	return nil
}

// answerPeek is the answer to HEAD of a manifest the cache lacks: status,
// length, content type and digest of the remote, no body, nothing stored.
func answerPeek(w http.ResponseWriter, peek proxy.Peek) {
	h := w.Header()
	contentType := peek.Header.Get("Content-Type")
	if !headerSafe(&contentType) {
		contentType = jsonType
	}
	h.Set("Content-Type", contentType)
	if digest := peek.Header.Get(docker.DigestHeader); docker.IsDigest(digest) {
		h.Set(docker.DigestHeader, digest)
		h.Set("ETag", `"`+digest+`"`)
	}
	if length := peek.Header.Get("Content-Length"); length != "" && strings.Trim(length, "0123456789") == "" {
		h.Set("Content-Length", length)
	}
	h.Set(docker.APIVersionHeader, docker.APIVersion)
	w.WriteHeader(http.StatusOK)
}

// dockerTagsThrough is the tag list of a repository with a member that asks
// its remote: n is checked first, as dockerTags does, so an n that does not
// read asks no remote.
func (s *Server) dockerTagsThrough(w http.ResponseWriter, req *request, repo repos.Repo, members []repos.Repo, call *docker.V2Path) error {
	limit, apiErr := intParam(req.params(), "n", -1, 0, nil)
	if apiErr != nil {
		dockerError(w, req, http.StatusBadRequest, "PAGINATION_NUMBER_INVALID", apiErr.message, nil)
		return nil
	}
	var names []string
	known := false
	for _, member := range members {
		children, err := s.rp.allChildren(member.Name, "v2/"+call.Name+"/manifests")
		if err != nil {
			return err
		}
		for _, c := range children {
			known = true
			if !c.IsDir {
				names = append(names, c.Name)
			}
		}
		if s.fetches(member) {
			tags, ok, err := s.proxies.Tags(req.r.Context(), member, call.Name)
			if err != nil {
				return nil // the client left
			}
			if ok {
				known = true
				names = append(names, tags...)
			}
		}
	}
	if !known {
		dockerError(w, req, http.StatusNotFound, "NAME_UNKNOWN", fmt.Sprintf("repository name %s is not known", call.Name), nil)
		return nil
	}
	return s.answerTags(w, req, repo, call, names, limit)
}

// storeManifest is the store of a fetched manifest: the bytes at most
// docker.MaxManifestBytes, which must have the digest the request names or
// the one the remote sends in its digest header, stored through ingest under
// its digest and, for a tag, under the tag as a hard link of the digest
// blob, as putManifest stores a pushed manifest. A path that holds these
// bytes already is not written again.
func (s *Server) storeManifest(member repos.Repo, name, reference string, subject *model.Subject, address string) func(proxy.Fetched) error {
	named := func(err error) error {
		var refusal *writeRefusal
		if errors.As(err, &refusal) {
			return fmt.Errorf("rule %s: %w", refusal.rule, err)
		}
		return err
	}
	return func(f proxy.Fetched) error {
		data, err := io.ReadAll(io.LimitReader(f.Body, docker.MaxManifestBytes+1))
		if err != nil {
			return err
		}
		if len(data) > docker.MaxManifestBytes {
			return named(writeRefused(http.StatusRequestEntityTooLarge, ruleTooLarge, "the manifest of the remote passes %d bytes",
				docker.MaxManifestBytes))
		}
		if f.Size >= 0 && int64(len(data)) != f.Size {
			return named(writeRefused(http.StatusBadRequest, ruleSize, "the manifest has %d bytes, the remote declares %d", len(data), f.Size))
		}
		digest := docker.DigestOf(data)
		for _, want := range []string{reference, f.Header.Get(docker.DigestHeader)} {
			if !docker.IsDigest(want) {
				continue
			}
			algorithm, _ := docker.DigestParts(want)
			actual, err := docker.DigestOfAlgorithm(data, algorithm)
			if err != nil {
				return err
			}
			if actual != want {
				return named(writeRefused(http.StatusBadRequest, ruleDigest, "the manifest has the digest %s, the remote names %s", actual, want))
			}
		}
		if docker.IsDigest(reference) {
			digest = reference
		}
		// checked as a pushed manifest is: the type stored is the one of the manifest
		mediaType, _, bad := docker.Manifest(data, f.ContentType)
		if bad != nil {
			return named(writeRefused(http.StatusBadRequest, "content", "the remote sent no manifest this server takes: %s", bad.Message))
		}
		digestPath := "v2/" + name + "/manifests/" + digest
		digestRow, err := s.rp.index.Lookup(member.Name, digestPath)
		if err != nil {
			return err
		}
		var digests stores.Digests
		if !sameBytes(digestRow, data) {
			algorithm, value := docker.DigestParts(digest)
			result, err := s.ingest(ingestRequest{repo: member, path: digestPath, body: bytes.NewReader(data), size: int64(len(data)),
				digests: map[string]string{algorithm: value}, contentType: mediaType, exempt: true, fetch: true, subject: subject, address: address})
			if err != nil {
				return named(err)
			}
			digestRow, digests = &result.asset, result.digests
		}
		if docker.IsDigest(reference) {
			return nil
		}
		tagPath := "v2/" + name + "/manifests/" + reference
		tagRow, err := s.rp.index.Lookup(member.Name, tagPath)
		if err != nil || sameBytes(tagRow, data) {
			return err
		}
		if digestRow.Store == s.writes.writer.Name() {
			_, err = s.linkCopy(member, tagPath, digestRow, mediaType, digests, false, false, subject, address)
		} else {
			_, err = s.ingest(ingestRequest{repo: member, path: tagPath, body: bytes.NewReader(data), size: int64(len(data)),
				contentType: mediaType, fetch: true, subject: subject, address: address})
		}
		return named(err)
	}
}
