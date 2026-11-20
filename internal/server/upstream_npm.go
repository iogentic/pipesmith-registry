package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/npm"
	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// The read path of an npm proxy (task 12 of the phase 5 plan, npm). The
// routing rule decides first. With upstream on a package document is asked
// for at the remote under its escaped name and stored through ingest as the
// document of the package; the read path of stored documents answers it,
// with tarball addresses of this server (npm.go). A tarball is asked for
// under the path of the request and must have the integrity or the SHA-1
// that the cached document gives its version (decision 5.10). The calls
// below "-" never reach the remote (decision 5.18).

// errUnverifiable is a tarball whose document gives a digest that cannot be
// read: it is not fetched.
var errUnverifiable = errors.New("the document of the package gives no digest that can be read")

// proxyNpm takes a request below an npm proxy before the read path of
// stored documents. done is true when it answered; else that read path
// answers from what the cache holds now.
func (s *Server) proxyNpm(w *recorder, req *request, st *state, repo repos.Repo, t *target) (done bool, err error) {
	allowed, err := s.routes.Allowed(repo.Name, routedPath(t))
	if err != nil {
		npmRefuse(w, req, http.StatusServiceUnavailable, routingRefusal, "")
		return true, nil
	}
	if !allowed {
		npmRefuse(w, req, http.StatusForbidden, fmt.Sprintf("a routing rule of repository %s blocks this path", repo.Name), "")
		return true, nil
	}
	if s.proxies == nil || t.directory || npmDash(t) {
		return false, nil
	}
	if call, ok := npm.ParsePath(t.path); ok {
		outcome, err := s.npmDocument(w, req, repo, call.Name)
		if err != nil {
			return true, nil // the client left; the fetch goes on without it
		}
		if outcome == proxy.Failed {
			npmFailedRemote(w, req, repo)
			return true, nil
		}
		return false, nil
	}
	if name, _, ok := npmTarball(t.path); ok {
		return s.npmTarballOf(w, req, st, repo, name, t.path)
	}
	return false, nil
}

// npmFailedRemote is the 502 of an npm proxy whose remote failed and whose
// cache has no copy (decision 5.9).
func npmFailedRemote(w http.ResponseWriter, req *request, repo repos.Repo) {
	npmRefuse(w, req, http.StatusBadGateway, fmt.Sprintf("the remote of repository %s failed", repo.Name), "")
}

// npmRemoteName is the escaped path of the document of a package at the
// remote: a scoped name is one segment, its slash encoded, as the npm
// command line asks for it.
func npmRemoteName(name string) string {
	if scope, rest, scoped := strings.Cut(name, "/"); scoped {
		return url.PathEscape(scope) + "%2F" + url.PathEscape(rest)
	}
	return url.PathEscape(name)
}

// npmDocument asks the proxies for the package document of name: the cached
// copy while the metadata age lasts, else the remote. The error is the one of
// the client that left.
func (s *Server) npmDocument(w *recorder, req *request, repo repos.Repo, name string) (proxy.Outcome, error) {
	look := s.lookFor(repo.Name, name)
	cached, err := look()
	if err != nil {
		return proxy.Failed, nil
	}
	q := proxy.Query{Repo: repo, Path: name, Remote: npmRemoteName(name), Cached: cached, Limit: repo.WriteSettings().MaxBodyBytes,
		Look: look, Store: s.storeTyped(repo, name, jsonType, req.subject, peerAddress(w))}
	return s.proxies.Get(req.r.Context(), q)
}

// npmTarballOf fetches a tarball of an npm proxy that its document names
// (npmTarballFetch) and answers 502 when the remote failed and the cache
// has no copy; done is false when the read path of the port answers from
// the cache, or with 404.
func (s *Server) npmTarballOf(w *recorder, req *request, st *state, repo repos.Repo, name, path string) (bool, error) {
	outcome, left, err := s.npmTarballFetch(w, req, st, repo, name, path)
	switch {
	case err != nil:
		return false, err
	case left:
		return true, nil // the client left; the fetch goes on without it
	case outcome == proxy.Failed:
		npmFailedRemote(w, req, repo)
		return true, nil
	}
	return false, nil
}

// npmTarballFetch asks the proxies for a tarball of a member that fetches,
// with the digests its cached document gives the version (decision 5.10).
// A document the cache lacks, or that does not name the tarball, is asked
// for first. A tarball no document names is never asked for: Missing, or
// Cache when the cache holds it. Failed is a remote that failed with no
// copy cached, or a document whose digest cannot be read. left is true
// when the client left.
func (s *Server) npmTarballFetch(w *recorder, req *request, st *state, member repos.Repo, name, path string) (proxy.Outcome, bool, error) {
	look := s.lookFor(member.Name, path)
	cached, err := look()
	if err != nil {
		return proxy.Failed, false, err
	}
	keep := proxy.Missing
	if cached != nil {
		keep = proxy.Cache
	}
	digests, named, err := s.npmTarballDigests(st, member, name, path)
	if err == nil && !named {
		outcome, err := s.npmDocument(w, req, member, name)
		if err != nil {
			return proxy.Failed, true, nil
		}
		if outcome == proxy.Failed && cached == nil {
			return proxy.Failed, false, nil
		}
		if st, err = s.rp.sync(); err != nil {
			return proxy.Failed, false, err
		}
		digests, named, err = s.npmTarballDigests(st, member, name, path)
	}
	if errors.Is(err, errUnverifiable) {
		logAt(slogWarn, serverLogger, "proxy %s: %q: %v; the tarball is not fetched", member.Name, path, err)
		if cached == nil {
			return proxy.Failed, false, nil
		}
		return proxy.Cache, false, nil
	}
	if err != nil || !named {
		return keep, false, err
	}
	q := proxy.Query{Repo: member, Path: path, Cached: cached, Digests: digests, Limit: member.WriteSettings().MaxBodyBytes, Look: look,
		Store: s.storeFor(member, path, req.subject, peerAddress(w))}
	outcome, err := s.proxies.Get(req.r.Context(), q)
	return outcome, err != nil, nil
}

// npmTarballDigests are the digests the cached document of a package gives
// the version whose tarball lies at path: dist.integrity and dist.shasum,
// by algorithm in hex. named is false when the cache holds no document that
// names the tarball.
func (s *Server) npmTarballDigests(st *state, repo repos.Repo, name, path string) (map[string]string, bool, error) {
	documents, _, err := s.rp.npmDocuments(st, []repos.Repo{repo}, name)
	if err != nil || len(documents) == 0 {
		return nil, false, err
	}
	document := documents[0]
	document.Set("name", name)
	versions, _ := field(document, "versions").(*pyjson.Object)
	for _, vp := range npm.TarballPaths(document) {
		if vp.Path != path || versions == nil {
			continue
		}
		entry, _ := field(versions, vp.Version).(*pyjson.Object)
		if entry == nil {
			continue
		}
		dist, _ := field(entry, "dist").(*pyjson.Object)
		digests := map[string]string{}
		if dist != nil {
			if integrity, ok := field(dist, "integrity").(string); ok && integrity != "" {
				found, err := npm.Integrity(integrity)
				if err != nil {
					return nil, true, fmt.Errorf("%w: %v", errUnverifiable, err)
				}
				digests = found
			}
			if shasum := npmShasum(entry); shasum != "" {
				if other, ok := digests["sha1"]; ok && other != shasum {
					return nil, true, fmt.Errorf("%w: dist.integrity and dist.shasum name two SHA-1 digests", errUnverifiable)
				}
				digests["sha1"] = shasum
			}
		}
		return digests, true, nil
	}
	return nil, false, nil
}

// reaching are the members that reach their remote now: proxies of a format
// that fetches, with upstream on, in the state ready (decision 5.19). Their
// versions stay in an npm document without a stored tarball, which arrives
// when a client asks for it.
func (s *Server) reaching(members []repos.Repo) map[string]bool {
	if s.proxies == nil {
		return nil
	}
	var out map[string]bool
	for _, m := range members {
		if m.Type != "proxy" || !proxy.Fetches(m.Format) || s.proxies.State(m).Status != proxy.Ready {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[m.Name] = true
	}
	return out
}
