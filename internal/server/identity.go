package server

import (
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// The writes of the area security (leg 6-identity): roles, users, the
// password of a user, anonymous access, the active realms and the LDAP
// connections. Each route
// asks the permission the contract names for its action; the rule of the
// route checks the request token of a session (allow). The writes change
// the state through Options.Identity and work whenever security is on,
// writes on or off (ruling I2). internal/rest builds the answers.

// maxIdentityBody bounds the body of an identity write.
const maxIdentityBody = 64 << 10

// identityBuild builds the answer of one identity write from the match of
// its path, its query and its body.
type identityBuild func(env *rest.Env, w rest.IdentityWrite, match []string, q url.Values, body []byte) (rest.Answer, error)

func identityRoute(method, path, permission string, build identityBuild) route {
	return entry(exact(path), []string{method}, perm(permission), func(s *Server, w *recorder, req *request, match []string) error {
		return s.identityWrite(w, req, match, build)
	})
}

// ldapCheckRoute is a check of an LDAP form: POST, with the permission to
// create or to save a connection (contract S27). It writes nothing.
func ldapCheckRoute(path string, check func(env *rest.Env, w rest.IdentityWrite, body []byte) (rest.Answer, error)) route {
	return entry(exact(path), []string{"POST"}, perm("nexus:ldap:create", "nexus:ldap:update"),
		func(s *Server, w *recorder, req *request, match []string) error {
			return s.identityWrite(w, req, match, func(env *rest.Env, iw rest.IdentityWrite, _ []string, _ url.Values, body []byte) (rest.Answer, error) {
				return check(env, iw, body)
			})
		})
}

const securityBase = restBase + "/security"

const ldapBase = securityBase + "/ldap"

// identityRoutes are the writes of the area security in the table with
// security on, writes on or off: the nine of users, roles, anonymous access
// and realms, those of the LDAP connections and the three checks of an LDAP
// form.
var identityRoutes = []route{
	identityRoute("POST", securityBase+"/roles", "nexus:roles:create",
		func(env *rest.Env, w rest.IdentityWrite, _ []string, _ url.Values, body []byte) (rest.Answer, error) {
			return rest.CreateRole(env, w, body)
		}),
	identityRoute("PUT", securityBase+"/roles/"+segment, "nexus:roles:update",
		func(env *rest.Env, w rest.IdentityWrite, m []string, _ url.Values, body []byte) (rest.Answer, error) {
			return rest.SaveRole(env, w, m[1], body)
		}),
	identityRoute("DELETE", securityBase+"/roles/"+segment, "nexus:roles:delete",
		func(env *rest.Env, w rest.IdentityWrite, m []string, _ url.Values, _ []byte) (rest.Answer, error) {
			return rest.DeleteRole(env, w, m[1])
		}),
	identityRoute("POST", usersBase, "nexus:users:create",
		func(env *rest.Env, w rest.IdentityWrite, _ []string, _ url.Values, body []byte) (rest.Answer, error) {
			return rest.CreateUser(env, w, body)
		}),
	identityRoute("PUT", usersBase+"/"+segment, "nexus:users:update",
		func(env *rest.Env, w rest.IdentityWrite, m []string, _ url.Values, body []byte) (rest.Answer, error) {
			return rest.SaveUser(env, w, m[1], body)
		}),
	identityRoute("DELETE", usersBase+"/"+segment, "nexus:users:delete",
		func(env *rest.Env, w rest.IdentityWrite, m []string, q url.Values, _ []byte) (rest.Answer, error) {
			return rest.DeleteUser(env, w, m[1], q)
		}),
	identityRoute("PUT", usersBase+"/"+segment+"/change-password", "nexus:userschangepw:create",
		func(env *rest.Env, w rest.IdentityWrite, m []string, _ url.Values, body []byte) (rest.Answer, error) {
			return rest.ChangePassword(env, w, m[1], body)
		}),
	identityRoute("PUT", securityBase+"/anonymous", "nexus:settings:update",
		func(env *rest.Env, w rest.IdentityWrite, _ []string, _ url.Values, body []byte) (rest.Answer, error) {
			return rest.SaveAnonymous(env, w, body)
		}),
	identityRoute("PUT", securityBase+"/realms/active", "nexus:settings:update",
		func(env *rest.Env, w rest.IdentityWrite, _ []string, _ url.Values, body []byte) (rest.Answer, error) {
			return rest.SaveRealms(env, w, body)
		}),
	identityRoute("POST", ldapBase, "nexus:ldap:create",
		func(env *rest.Env, w rest.IdentityWrite, _ []string, _ url.Values, body []byte) (rest.Answer, error) {
			return rest.CreateLDAP(env, w, body)
		}),
	identityRoute("POST", ldapBase+"/change-order", "nexus:ldap:update",
		func(env *rest.Env, w rest.IdentityWrite, _ []string, _ url.Values, body []byte) (rest.Answer, error) {
			return rest.OrderLDAP(env, w, body)
		}),
	identityRoute("PUT", ldapBase+"/"+segment, "nexus:ldap:update",
		func(env *rest.Env, w rest.IdentityWrite, m []string, _ url.Values, body []byte) (rest.Answer, error) {
			return rest.SaveLDAP(env, w, m[1], body)
		}),
	identityRoute("DELETE", ldapBase+"/"+segment, "nexus:ldap:delete",
		func(env *rest.Env, w rest.IdentityWrite, m []string, _ url.Values, _ []byte) (rest.Answer, error) {
			return rest.DeleteLDAP(env, w, m[1])
		}),
	identityRoute("POST", "/api/ldap/clear-cache", "nexus:ldap:update",
		func(env *rest.Env, w rest.IdentityWrite, _ []string, _ url.Values, _ []byte) (rest.Answer, error) {
			return rest.ClearLDAPCache(env, w)
		}),
	ldapCheckRoute("/api/ldap/verify-connection", rest.VerifyLDAPConnection),
	ldapCheckRoute("/api/ldap/verify-user-mapping", rest.VerifyLDAPUserMapping),
	ldapCheckRoute("/api/ldap/verify-login", rest.VerifyLDAPLogin),
}

// identityRefusal is the 503 of an identity write of a server without an
// identity store.
const identityRefusal = "the identity state of this server cannot be changed"

// identityWrite reads the body of an identity write, at most
// maxIdentityBody bytes (413 past it), and answers with what build makes
// of it. A 204, and a 201 without a body, have none.
func (s *Server) identityWrite(w *recorder, req *request, match []string, build identityBuild) error {
	if s.identity == nil {
		s.message(w, req, http.StatusServiceUnavailable, identityRefusal)
		return nil
	}
	q, err := url.ParseQuery(req.query)
	if err != nil {
		s.message(w, req, http.StatusBadRequest, "the query string cannot be read")
		return nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.r.Body, maxIdentityBody))
	if tooLarge(err) {
		s.message(w, req, http.StatusRequestEntityTooLarge, "the body is larger than "+strconv.Itoa(maxIdentityBody)+" bytes")
		return nil
	}
	if err != nil {
		return err
	}
	env, err := s.restEnv(req)
	if err != nil {
		return err
	}
	a, err := build(env, rest.IdentityWrite{Store: s.identity, Subject: req.subject}, match, q, body)
	if err != nil {
		return err
	}
	if a.Status == http.StatusNoContent || a.Status == http.StatusCreated && a.Body == nil {
		writeBody(w, req, a.Status, [][2]string{{"Cache-Control", "no-store"}}, nil)
		return nil
	}
	return s.writeAnswer(w, req, a)
}
