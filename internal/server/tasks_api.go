package server

import (
	"io"
	"net/http"
	"strconv"

	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// The task types and task settings of leg 6-api (contract S39 and S40,
// ruling AP5) with security on, writes on or off. Each goes through
// taskRoute with the permission the contract names; every write stays
// behind freezeGate (ruling AP6).

const apiTasksBase = "/api/tasks"

// taskBodyRoute is a write of a task with its body, read up to the bound of
// the writes of the area security.
func taskBodyRoute(method, path, permission string, build func(s *Server, w rest.IdentityWrite, match []string, body []byte) (rest.Answer, error)) route {
	return taskRoute(method, path, permission, func(s *Server, req *request, match []string) (rest.Answer, error) {
		body, err := io.ReadAll(io.LimitReader(req.r.Body, maxIdentityBody+1))
		if err != nil {
			return rest.Answer{}, err
		}
		if len(body) > maxIdentityBody {
			return rest.Answer{Status: http.StatusRequestEntityTooLarge,
				Body: map[string]string{"message": "the body is larger than " + strconv.Itoa(maxIdentityBody) + " bytes"}}, nil
		}
		return build(s, rest.IdentityWrite{Store: s.identity, Subject: req.subject}, match, body)
	})
}

// taskSettingsRoutes are the requests of the task types and settings in the
// table with security on, writes on or off.
var taskSettingsRoutes = []route{
	taskRoute("GET", "/api/task-types", "nexus:tasks:read", func(*Server, *request, []string) (rest.Answer, error) {
		return rest.TaskTypes()
	}),
	taskRoute("GET", apiTasksBase+"/"+segment, "nexus:tasks:read", func(s *Server, req *request, m []string) (rest.Answer, error) {
		env, err := s.restEnv(req)
		if err != nil {
			return rest.Answer{}, err
		}
		return rest.TaskSettings(env, m[1])
	}),
	taskBodyRoute("POST", apiTasksBase, "nexus:tasks:create", func(s *Server, w rest.IdentityWrite, _ []string, body []byte) (rest.Answer, error) {
		return rest.CreateTask(w, s.tasks, body)
	}),
	taskBodyRoute("PUT", apiTasksBase+"/"+segment, "nexus:tasks:update", func(s *Server, w rest.IdentityWrite, m []string, body []byte) (rest.Answer, error) {
		return rest.SaveTask(w, s.tasks, m[1], body)
	}),
	taskBodyRoute("DELETE", apiTasksBase+"/"+segment, "nexus:tasks:delete", func(s *Server, w rest.IdentityWrite, m []string, _ []byte) (rest.Answer, error) {
		return rest.DeleteTask(w, s.tasks, m[1])
	}),
}
