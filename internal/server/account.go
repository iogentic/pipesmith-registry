package server

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// The own account of the interface (leg 6-api, contract S41, ruling AP7):
// the names and mail and the password of the subject itself. Both writes
// pass freezeGate and the request token rule (AP6, A22).

const accountBase = "/api/account"

// passwordChanger is the change of the own password of the security
// service; auth.Service fulfils it.
type passwordChanger interface {
	ChangeOwnPassword(r *http.Request, subject *model.Subject, current, hash string) error
}

// accountRefusal is the message of a 503 of the password change.
// #nosec G101 -- the message of a 503 of the password change, no credential.
const accountRefusal = "the passwords of this server cannot be changed"

// accountRoute is a request of the own account. An error of the security
// service with a status, the 429 of the sign in limits among them, answers
// as a failed sign in does.
func accountRoute(method, path string, build func(s *Server, req *request, w rest.IdentityWrite, body []byte) (rest.Answer, error)) route {
	return entry(exact(path), []string{method}, signedIn, func(s *Server, w *recorder, req *request, match []string) error {
		err := s.identityWrite(w, req, match, func(_ *rest.Env, iw rest.IdentityWrite, _ []string, _ url.Values, body []byte) (rest.Answer, error) {
			return build(s, req, iw, body)
		})
		var e *model.Error
		if errors.As(err, &e) && s.credentialError(w, req, err, false) {
			return nil
		}
		return err
	})
}

// accountRoutes are the requests of the own account in the table with
// security on, writes on or off.
var accountRoutes = []route{
	accountRoute("PUT", accountBase, func(_ *Server, _ *request, w rest.IdentityWrite, body []byte) (rest.Answer, error) {
		return rest.SaveAccount(w, body)
	}),
	accountRoute("POST", accountBase+"/password", func(s *Server, req *request, w rest.IdentityWrite, body []byte) (rest.Answer, error) {
		changer, _ := s.sec.(passwordChanger)
		if changer == nil {
			return rest.Answer{Status: http.StatusServiceUnavailable, Body: map[string]string{"message": accountRefusal}}, nil
		}
		return rest.ChangeOwnPassword(w, func(current, hash string) error {
			return changer.ChangeOwnPassword(req.r, w.Subject, current, hash)
		}, body)
	}),
}
