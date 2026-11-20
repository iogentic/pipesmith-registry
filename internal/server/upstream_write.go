package server

import (
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// The write to a group (decision 5.16 of the phase 5 plan, task 13). A group
// may name one writable member, group.groupWriteMember, a hosted member of
// its format (GroupSettings of internal/repos). A write to the group runs the
// write handler of phase 4 with that member as target; without the attribute
// the refusal of phase 4 stays. The permission checked is that of the
// group, with the action the path asks in the member: add for a new path,
// edit for a path the member holds, delete for a delete. Every address an
// answer names carries the group (dockerBase), so the next request of a
// client stays on the group, where it holds its permission.

// writableMember is the member a write to a group goes to, false when the
// group names none, or one that takes no write.
func (s *Server) writableMember(st *state, group repos.Repo) (repos.Repo, bool) {
	if group.Type != "group" {
		return repos.Repo{}, false
	}
	settings, err := st.repositories.GroupSettings(group.Name)
	if err != nil || settings.WritableMember == "" {
		return repos.Repo{}, false
	}
	member, found := st.repositories.Get(settings.WritableMember)
	return member, found && member.TakesWrites()
}

// writeTarget is the name of the repository a write below
// /repository/<name>/ lands in: the writable member of a group that names
// one, else name itself. The action of the permission follows the path in
// it.
func (s *Server) writeTarget(name string) (string, error) {
	st, err := s.rp.sync()
	if err != nil {
		return "", err
	}
	if group, found := st.repositories.Get(name); found {
		if member, writable := s.writableMember(st, group); writable {
			return member.Name, nil
		}
	}
	return name, nil
}

// throughGroup marks a request to a group that names a writable member: the
// member takes the request, the group stays the repository its answers name
// and whose permission it asks. It returns the member, or repo when the
// request does not go through such a group.
func (s *Server) throughGroup(st *state, req *request, repo repos.Repo) (repos.Repo, bool) {
	member, writable := s.writableMember(st, repo)
	if !writable {
		return repo, false
	}
	group := repo
	req.via = &group
	return member, true
}

// addressed is the repository a client addressed: the group a write came
// through, else the repository written. Its permission is asked, and an
// address an answer names carries it.
func addressed(req *request, repo repos.Repo) repos.Repo {
	if req.via != nil {
		return *req.via
	}
	return repo
}
