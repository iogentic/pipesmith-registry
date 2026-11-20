package server

import (
	"sync"
	"sync/atomic"

	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/npm"
	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// Groups with proxy members (task 13 of the phase 5 plan). A file comes from
// the first member that has it, in member order, across hosted and proxy
// members and a group inside a group: a hosted member by its rows, a proxy
// member by its cache while that is fresh, else by its remote. A member
// whose remote said 404 or failed, or that is not ready, is skipped, and the
// group answers 404 when no member has the path (decision 5.9). A merged
// document (maven-metadata.xml, an npm package document) is the merge over
// every member, each proxy member first asked through its freshness rules,
// at most groupFetches at once. The routing rules of the members were
// applied before (routedMembers). The Docker read path walks its members in
// upstream_docker.go.

// groupFetches is the most proxy members a merged document asks at once.
const groupFetches = 4

// eachAtOnce runs ask for every member that fetches, at most groupFetches at
// once, and waits for all of them. It reports whether ask said that the
// client left.
func (s *Server) eachAtOnce(members []repos.Repo, ask func(member repos.Repo) (left bool)) bool {
	slots := make(chan struct{}, groupFetches)
	var wg sync.WaitGroup
	var left atomic.Bool
	for _, member := range members {
		if !s.fetches(member) {
			continue
		}
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() {
				<-slots
				wg.Done()
			}()
			if ask(member) {
				left.Store(true)
			}
		}()
	}
	wg.Wait()
	return left.Load()
}

// groupFile answers a file of a raw or maven2 group whose members include a
// proxy that fetches.
func (s *Server) groupFile(w *recorder, req *request, st *state, repo repos.Repo, members []repos.Repo, t *target, prefix string) error {
	base, _ := maven.SplitChecksum(t.path)
	if repo.Format == "maven2" && maven.IsMetadataPath(base) {
		if s.eachAtOnce(members, func(member repos.Repo) bool {
			b, err := s.rp.file(st, member, []repos.Repo{member}, base)
			if err != nil {
				logAt(slogWarn, serverLogger, "proxy %s: %q: the cache is not read: %v", member.Name, base, err)
				return false
			}
			b, _, left, err := s.proxyCopy(w, req, st, member, base, b)
			if b != nil {
				b.close()
			}
			if err != nil {
				logAt(slogWarn, serverLogger, "proxy %s: %q: the cache is not read: %v", member.Name, base, err)
			}
			return left
		}) {
			return nil // the client left; the fetches go on without it
		}
		b, err := s.rp.file(st, repo, members, t.path)
		if err != nil || b == nil {
			return s.notInGroup(w, req, repo, members, t, prefix, err)
		}
		return send(w, req, b, nil)
	}
	for _, member := range members {
		b, err := s.rp.file(st, member, []repos.Repo{member}, t.path)
		if err != nil {
			return err
		}
		if s.fetches(member) {
			var left bool
			if b, _, left, err = s.proxyCopy(w, req, st, member, t.path, b); err != nil || left {
				return err // left: the client left; the fetch goes on without it
			}
		}
		if b != nil {
			return send(w, req, b, nil)
		}
	}
	return s.notInGroup(w, req, repo, members, t, prefix, nil)
}

// notInGroup is the answer of a group for a path no member has: the
// redirect to the folder when a member holds one at the path, else 404.
func (s *Server) notInGroup(w *recorder, req *request, repo repos.Repo, members []repos.Repo, t *target, prefix string, err error) error {
	if err != nil {
		return err
	}
	isDir, err := s.rp.anyIsDir(members, t.path)
	if err != nil {
		return err
	}
	if isDir {
		redirect(w, req, prefix, repo.Name, t.path)
		return nil
	}
	writeText(w, req, 404, "Not found")
	return nil
}

// groupNpm takes a request below an npm group whose members include a proxy
// that fetches, before the read path of stored documents: a package
// document of every such member is asked for through its freshness rules,
// then that read path merges them (decision 5.19 for the versions); a
// tarball comes from the first member that has it, a proxy member asking
// its remote with the digests of its own document. done is true when it
// answered.
func (s *Server) groupNpm(w *recorder, req *request, st *state, members []repos.Repo, t *target) (bool, error) {
	if t.directory || npmDash(t) {
		return false, nil
	}
	if call, ok := npm.ParsePath(t.path); ok {
		return s.eachAtOnce(members, func(member repos.Repo) bool {
			_, err := s.npmDocument(w, req, member, call.Name)
			return err != nil
		}), nil
	}
	name, _, ok := npmTarball(t.path)
	if !ok {
		return false, nil
	}
	for _, member := range members {
		if s.fetches(member) {
			outcome, left, err := s.npmTarballFetch(w, req, st, member, name, t.path)
			if err != nil || left {
				return true, err
			}
			if outcome == proxy.Failed || outcome == proxy.Missing {
				continue
			}
		}
		b, err := s.rp.file(st, member, []repos.Repo{member}, t.path)
		if err != nil {
			return true, err
		}
		if b != nil {
			return true, send(w, req, b, nil)
		}
	}
	return false, nil // the read path of the port answers 404
}
