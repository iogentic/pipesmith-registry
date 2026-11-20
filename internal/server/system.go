package server

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"

	adminrules "github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/rest"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The routes of leg 6-system with security on: the system information
// (SY7), the capabilities (SY4 to SY6, SY8), the routing rules with
// their test and preview (SY1 to SY3), the tasks (SB1, SB3) and the
// changes of the read only mode (SB2). Each route asks the
// permission the contract names; a change goes through the identity store
// (Options.Identity) as the writes of the area security do.

// systemRoutes are the routes of leg 6-system in the table with security
// on, writes on or off.
var systemRoutes = []route{
	get("/service/rest/beta/system/information", perm("nexus:atlas:read"), (*Server).systemInformation),
	get(restBase+"/system/node", perm(settingsRead), (*Server).nodeAnswer),
	get(restBase+"/capabilities", perm("nexus:capabilities:read"), plain(rest.Capabilities)),
	identityRoute("PUT", restBase+"/capabilities/"+segment, "nexus:capabilities:update",
		func(env *rest.Env, w rest.IdentityWrite, m []string, _ url.Values, body []byte) (rest.Answer, error) {
			return rest.SaveCapability(env, w, m[1], body)
		}),
	identityRoute("DELETE", restBase+"/capabilities/"+segment, "nexus:capabilities:delete",
		func(env *rest.Env, w rest.IdentityWrite, m []string, _ url.Values, _ []byte) (rest.Answer, error) {
			return rest.DeleteCapability(env, w, m[1])
		}),
	get("/api/capabilities/support", perm("nexus:capabilities:read"), plain(func(*rest.Env) (rest.Answer, error) {
		return rest.CapabilitySupport()
	})),
	routingRoute("GET", routingRulesBase, settingsRead, true, func(s *Server, _ *rest.Env, store adminrules.RoutingStore, _ []string, _ []byte) (rest.Answer, error) {
		return rest.RoutingRules(store)
	}),
	routingRoute("POST", routingRulesBase, settingsUpdate, true, func(s *Server, _ *rest.Env, store adminrules.RoutingStore, _ []string, body []byte) (rest.Answer, error) {
		return rest.CreateRoutingRule(store, body)
	}),
	routingRoute("GET", routingRulesBase+"/"+segment, settingsRead, true, func(s *Server, _ *rest.Env, store adminrules.RoutingStore, m []string, _ []byte) (rest.Answer, error) {
		return rest.RoutingRule(store, m[1])
	}),
	routingRoute("PUT", routingRulesBase+"/"+segment, settingsUpdate, true, func(s *Server, _ *rest.Env, store adminrules.RoutingStore, m []string, body []byte) (rest.Answer, error) {
		return rest.SaveRoutingRule(store, m[1], body)
	}),
	routingRoute("DELETE", routingRulesBase+"/"+segment, settingsUpdate, true, func(s *Server, _ *rest.Env, store adminrules.RoutingStore, m []string, _ []byte) (rest.Answer, error) {
		return rest.DeleteRoutingRule(store, m[1])
	}),
	routingRoute("POST", "/api/routing-rules/test", settingsRead, false, func(s *Server, _ *rest.Env, _ adminrules.RoutingStore, _ []string, body []byte) (rest.Answer, error) {
		return rest.TestRoutingRule(body)
	}),
	routingRoute("POST", "/api/routing-rules/preview", settingsRead, false, func(s *Server, env *rest.Env, _ adminrules.RoutingStore, _ []string, body []byte) (rest.Answer, error) {
		return rest.PreviewRouting(env, body, func(repository, path string) (string, bool, error) {
			rule, err := s.routes.Link(repository)
			if err != nil {
				return "", false, &adminrules.Refusal{Code: adminrules.CodeUnavailable, Message: routingRefusal}
			}
			allowed, err := s.routes.Allowed(repository, path)
			if err != nil {
				return "", false, &adminrules.Refusal{Code: adminrules.CodeUnavailable, Message: routingRefusal}
			}
			return rule, allowed, nil
		})
	}),
	freezeRoute("freeze", (*adminrules.ReadOnlyMode).Freeze),
	freezeRoute("release", (*adminrules.ReadOnlyMode).Release),
	freezeRoute("force-release", (*adminrules.ReadOnlyMode).ForceRelease),
	taskRoute("GET", tasksBase, "nexus:tasks:read", func(s *Server, _ []string) (rest.Answer, error) {
		return rest.Tasks(s.tasks, s.runs)
	}),
	taskRoute("GET", tasksBase+"/"+segment, "nexus:tasks:read", func(s *Server, m []string) (rest.Answer, error) {
		return rest.Task(s.tasks, s.runs, m[1])
	}),
	taskRoute("POST", tasksBase+"/"+segment+"/run", "nexus:tasks:start", func(s *Server, m []string) (rest.Answer, error) {
		return rest.RunTask(s.identity, s.tasks, m[1])
	}),
	taskRoute("POST", tasksBase+"/"+segment+"/stop", "nexus:tasks:stop", func(s *Server, m []string) (rest.Answer, error) {
		return rest.StopTask(s.identity, s.tasks, m[1])
	}),
}

const (
	tasksBase        = restBase + "/tasks"
	tasksUnavailable = "the tasks of this server are not available"
	routingRulesBase = restBase + "/routing-rules"
	settingsUpdate   = "nexus:settings:update"
)

// routingBuild builds the answer of one request of the routing rules from
// the store, the match of its path and its body.
type routingBuild func(s *Server, env *rest.Env, store adminrules.RoutingStore, match []string, body []byte) (rest.Answer, error)

// routingRoute is a request of the routing rules with the permission the
// contract names. Its body is read up to one byte past the bound of ruling
// SY2, so that admin answers 400 for a larger one. A request that needs
// the rules answers 503 on a server whose identity store holds none; the
// test of a rule and the preview need none.
func routingRoute(method, path, permission string, needsStore bool, build routingBuild) route {
	return entry(exact(path), []string{method}, perm(permission), func(s *Server, w *recorder, req *request, match []string) error {
		store, _ := s.identity.(adminrules.RoutingStore)
		if store == nil && needsStore {
			s.message(w, req, http.StatusServiceUnavailable, "the routing rules of this server cannot be read or changed")
			return nil
		}
		body, err := io.ReadAll(io.LimitReader(req.r.Body, adminrules.MaxRuleBody+1))
		if err != nil {
			return err
		}
		env, err := s.restEnv(req)
		if err != nil {
			return err
		}
		a, err := build(s, env, store, match, body)
		if err != nil {
			return err
		}
		if a.Status == http.StatusNoContent {
			writeBody(w, req, a.Status, [][2]string{{"Cache-Control", "no-store"}}, nil)
			return nil
		}
		return s.writeAnswer(w, req, a)
	})
}

// systemInformation answers with the facts of this server; the stores are
// the stored ones and every attached store, the write store among them.
func (s *Server) systemInformation(w *recorder, req *request, _ []string) error {
	return s.restAnswer(w, req, func(env *rest.Env, _ url.Values) (rest.Answer, error) {
		st, err := s.rp.sync()
		if err != nil {
			return rest.Answer{}, err
		}
		env.Stores = nil
		for _, name := range slices.Sorted(maps.Keys(st.stores)) {
			store := st.stores[name]
			env.Stores = append(env.Stores, rest.Store{Name: name, Kind: store.Kind(), Root: store.Root(), Available: !stores.LooksUnmounted(store)})
		}
		return rest.SystemInformation(env, rest.SystemFacts{Started: s.started, Now: time.Now()})
	})
}

// startTasks starts the scheduler of the stored tasks, once the server is
// built and before it answers.
func (s *Server) startTasks() error {
	if s.tasks == nil {
		return nil
	}
	s.tasks.SetHost(s) // leg 6-kinds: the host of the built kinds
	if err := s.tasks.Start(); err != nil {
		return fmt.Errorf("the task scheduler did not start: %w", err)
	}
	return nil
}

// taskRoute is a request of the tasks with the permission the contract
// names. A server without a registry, or without the identity store whose
// hook a run and a stop end in, answers 503.
func taskRoute(method, path, permission string, build func(s *Server, match []string) (rest.Answer, error)) route {
	return entry(exact(path), []string{method}, perm(permission), func(s *Server, w *recorder, req *request, match []string) error {
		if s.tasks == nil || s.identity == nil {
			s.message(w, req, http.StatusServiceUnavailable, tasksUnavailable)
			return nil
		}
		a, err := build(s, match)
		if err != nil {
			return err
		}
		if a.Status == http.StatusNoContent {
			writeBody(w, req, a.Status, [][2]string{{"Cache-Control", "no-store"}}, nil)
			return nil
		}
		return s.writeAnswer(w, req, a)
	})
}

// freezeRoute is POST /v1/read-only/<name>, a change of the read only mode
// with nexus:settings:update. A server without the identity store whose
// hook the change ends in answers 503.
func freezeRoute(name string, change func(*adminrules.ReadOnlyMode) error) route {
	return entry(exact(restBase+"/read-only/"+name), []string{"POST"}, perm(settingsUpdate), func(s *Server, w *recorder, req *request, _ []string) error {
		if s.identity == nil || s.readOnly == nil {
			s.message(w, req, http.StatusServiceUnavailable, identityRefusal)
			return nil
		}
		a, err := rest.ChangeReadOnly(s.identity, func() error { return change(s.readOnly) })
		if err != nil {
			return err
		}
		if a.Status == http.StatusNoContent {
			writeBody(w, req, a.Status, [][2]string{{"Cache-Control", "no-store"}}, nil)
			return nil
		}
		return s.writeAnswer(w, req, a)
	})
}
