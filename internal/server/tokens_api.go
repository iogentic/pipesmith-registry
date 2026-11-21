package server

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// The tokens and the own account of the interface (leg 6-api, contract
// S29 and S41). The tokens ask nexus:apikey:*, the account a session or
// other credentials of a user; every write passes freezeGate and the
// request token rule like every other change (AP6, A22). A server whose
// security service issues no tokens answers 503.

const (
	// #nosec G101 -- the path of the token routes, no credential.
	tokensBase = "/api/tokens"
	// #nosec G101 -- the permission string the token routes ask, no credential.
	apikeyPermission = "nexus:apikey:*"
	// #nosec G101 -- the message of a 503 of the token routes, no credential.
	tokensUnavailable = "the tokens of this server cannot be read or changed"
)

// tokenBuild builds the answer of one request of the tokens or the own
// account.
type tokenBuild func(s *Server, req *request, svc rest.TokenService, env *rest.Env, w rest.IdentityWrite, match []string,
	q url.Values, body []byte) (rest.Answer, error)

// tokenAPIRoute is a request of the tokens or of the own account. An error of
// the security service with a status, the 429 of the sign in limits among
// them, answers as a failed sign in does.
func tokenAPIRoute(method, path string, r rule, build tokenBuild) route {
	return entry(exact(path), []string{method}, r, func(s *Server, w *recorder, req *request, match []string) error {
		svc, _ := s.sec.(rest.TokenService)
		if svc == nil {
			s.message(w, req, http.StatusServiceUnavailable, tokensUnavailable)
			return nil
		}
		err := s.identityWrite(w, req, match, func(env *rest.Env, iw rest.IdentityWrite, m []string, q url.Values, body []byte) (rest.Answer, error) {
			return build(s, req, svc, env, iw, m, q, body)
		})
		var e *model.Error
		if errors.As(err, &e) && s.credentialError(w, req, err, false) {
			return nil
		}
		return err
	})
}

// tokenRoutes are the requests of the tokens and of the own account in the
// table with security on, writes on or off.
var tokenRoutes = []route{
	tokenAPIRoute("GET", tokensBase, perm(apikeyPermission),
		func(_ *Server, _ *request, svc rest.TokenService, env *rest.Env, w rest.IdentityWrite, _ []string, q url.Values, _ []byte) (rest.Answer, error) {
			return rest.ListTokens(env, svc, w.Subject, q.Get("userId"))
		}),
	tokenAPIRoute("POST", tokensBase, perm(apikeyPermission),
		func(_ *Server, _ *request, svc rest.TokenService, _ *rest.Env, w rest.IdentityWrite, _ []string, _ url.Values, body []byte) (rest.Answer, error) {
			return rest.CreateToken(svc, w.Subject, body)
		}),
	tokenAPIRoute("DELETE", tokensBase+"/"+segment, perm(apikeyPermission),
		func(_ *Server, _ *request, svc rest.TokenService, env *rest.Env, w rest.IdentityWrite, m []string, _ url.Values, _ []byte) (rest.Answer, error) {
			return rest.DeleteToken(env, svc, w.Subject, m[1])
		}),
}
