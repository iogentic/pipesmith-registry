package server

import (
	"fmt"
	"net/http"

	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// The operation that invalidates the cache of a proxy or a group (decision
// 5.17 of the phase 5 plan, task 13): POST
// /service/rest/v1/repositories/{name}/invalidate-cache. The public source of
// the old product (tag release-3.87.2-01, read 2026-11-19) gives its
// answers: 204 once the cache is invalidated, 400 for a repository that is
// neither proxy nor group, 404 for an unknown one, and the edit action of
// the administration permission of the repository, asked before the type.
// The route asks that permission (ruleAdmin), so an unknown repository
// answers like a forbidden one to a caller without it (review 6, finding 7).

// proxyRoutes are the REST routes of phase 5, matched before the refusals.
var proxyRoutes = []route{
	entry(exact(restBase+"/repositories/"+segment+"/invalidate-cache"), []string{"POST"}, rule{kind: ruleAdmin, group: 1, action: "edit"},
		(*Server).invalidateCache),
}

// invalidateCache invalidates the cache of a proxy, or of every proxy member
// of a group, groups inside it included (Invalidate of internal/proxy). With
// upstream off no proxy keeps a cache state, and the operation answers 503
// with the switch named.
func (s *Server) invalidateCache(w *recorder, req *request, match []string) error {
	st, err := s.rp.sync()
	if err != nil {
		return err
	}
	repo, found := st.repositories.Get(match[1])
	switch {
	case !found:
		s.message(w, req, http.StatusNotFound, fmt.Sprintf("repository %s not found", match[1]))
		return nil
	case repo.Type != "proxy" && repo.Type != "group":
		s.message(w, req, http.StatusBadRequest, fmt.Sprintf("repository %s is a %s repository; the cache of a proxy or a group is invalidated", repo.Name, repo.Type))
		return nil
	case s.proxies == nil:
		s.message(w, req, http.StatusServiceUnavailable, fmt.Sprintf("requests to remotes are off (the switch upstream); repository %s keeps no cache state to invalidate", repo.Name))
		return nil
	}
	var proxies []repos.Repo
	for _, member := range st.repositories.Resolve(repo.Name) {
		if member.Type == "proxy" {
			proxies = append(proxies, member)
		}
	}
	for _, p := range proxies {
		if err := s.proxies.Invalidate(p.Name); err != nil {
			logAt(slogWarn, serverLogger, "the cache of proxy %s is not invalidated: %v", p.Name, err)
			s.message(w, req, http.StatusServiceUnavailable, fmt.Sprintf("the cache of repository %s is not invalidated: the state of the proxies cannot be written", p.Name))
			return nil
		}
	}
	noContent(w)
	return nil
}
