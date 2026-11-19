package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/docker"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// The handlers of the routes of tokens in secureRoutes (task 12 of the
// phase 3 plan): npm login, whoami and logout below the path of an npm
// repository, the token route of the Docker flow below a Docker repository
// and at /v2/token of each Docker port. Their patterns are more specific
// than /repository/(.*) and match first; below a repository of another
// format a request goes on to the routes of step 2 with their rules, as if
// these routes did not exist. The table gives them the rule public: login and
// the token route check the credentials themselves, whoami asks for a
// subject and logout for a signed in one through allow, once the format is
// known.

// maxNpmLoginBody bounds the body of npm login (hardening line 4).
const maxNpmLoginBody = 64 << 10

// npmTokenPath is the part of the path of npm logout before the token.
const npmTokenPath = "/-/user/token/"

// accessOf is the access kind of a repository of the read path, "" for a
// name no repository has.
func (s *Server) accessOf(name string) (string, error) {
	st, err := s.rp.sync()
	if err != nil {
		return "", err
	}
	if repo, ok := st.repositories.Get(name); ok {
		return repos.AccessOf(repo), nil
	}
	return "", nil
}

// passOn answers a request with the routes of step 2, their rules included,
// as route does without the routes of this file.
func (s *Server) passOn(w *recorder, req *request) error {
	var partial *route
	for i := range routes {
		rt := &routes[i]
		match := rt.pattern.FindStringSubmatch(req.path)
		if match == nil {
			continue
		}
		if slices.Contains(rt.methods, req.r.Method) {
			if allowed, err := s.allow(w, req, rt.rule, match); err != nil || !allowed {
				return err
			}
			return rt.handle(s, w, req, match)
		}
		if partial == nil {
			partial = rt
		}
	}
	if partial != nil {
		writeBody(w, req, http.StatusMethodNotAllowed, [][2]string{{"Content-Type", jsonType},
			{"Allow", strings.Join(partial.methods, ", ")}}, []byte(`{"detail":"Method Not Allowed"}`))
		return nil
	}
	unknownRoute(w, req)
	return nil
}

// ownAccess reports a repository of the access kind of a route; for any
// other the request has gone on with passOn.
func (s *Server) ownAccess(w *recorder, req *request, name, access string) (bool, error) {
	got, err := s.accessOf(name)
	if err != nil {
		return false, err
	}
	if got == access {
		return true, nil
	}
	return false, s.passOn(w, req)
}

var npmHeaders = [][2]string{{"Content-Type", jsonType}, {"X-Content-Type-Options", "nosniff"}, {"Cache-Control", "no-store"}}

func npmAnswer(w *recorder, req *request, status int, value any, extra ...[2]string) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	writeBody(w, req, status, append(slices.Clone(npmHeaders), extra...), data)
	return nil
}

type npmStatus struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// npmRefusal is the answer of a refusal of Security, in the legacy status of
// the npm registry; ok is false for an error Security does not give as
// *model.Error.
func npmRefusal(w *recorder, req *request, err error, wrongPair string) (bool, error) {
	var e *model.Error
	if !errors.As(err, &e) {
		return false, nil
	}
	message, extra := e.Message, [][2]string{}
	switch e.Status {
	case http.StatusUnauthorized:
		if errors.Is(err, model.ErrUnauthorized) && wrongPair != "" {
			message = wrongPair
		}
		extra = append(extra, [2]string{"WWW-Authenticate", basicChallenge})
	case http.StatusTooManyRequests:
		extra = append(extra, [2]string{"Retry-After", strconv.Itoa(max(e.RetryAfter, 1))})
	}
	return true, npmAnswer(w, req, e.Status, npmStatus{Error: message}, extra...)
}

// npmLogin is the legacy login of the npm client: PUT
// /-/user/org.couchdb.user:<name> with a JSON body that holds the name and
// the password. A good pair gets 201 with an npm token, the one answer that
// shows it.
func (s *Server) npmLogin(w *recorder, req *request, match []string) error {
	if own, err := s.ownAccess(w, req, match[1], "npm"); !own || err != nil {
		return err
	}
	var body struct {
		Name     *string `json:"name"`
		Password *string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.r.Body, maxNpmLoginBody)).Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return npmAnswer(w, req, http.StatusRequestEntityTooLarge, npmStatus{Error: "the body is larger than 64 KiB"})
		}
		return npmAnswer(w, req, http.StatusBadRequest, npmStatus{Error: "the body must be a JSON object with name and password"})
	}
	if body.Name == nil || body.Password == nil {
		return npmAnswer(w, req, http.StatusBadRequest, npmStatus{Error: "the body must be a JSON object with name and password"})
	}
	if *body.Name != match[2] {
		return npmAnswer(w, req, http.StatusBadRequest, npmStatus{Error: "the name of the body is not the name of the path"})
	}
	tok, err := s.sec.NpmLogin(req.r, *body.Name, *body.Password)
	if err != nil {
		if answered, aerr := npmRefusal(w, req, err, "wrong user name or password"); answered {
			return aerr
		}
		return err
	}
	return npmAnswer(w, req, http.StatusCreated, struct {
		OK    bool   `json:"ok"`
		ID    string `json:"id"`
		Token string `json:"token"`
	}{true, "org.couchdb.user:" + *body.Name, tok.Text})
}

// npmWhoami names the user of the credentials; the anonymous user while
// anonymous access is on.
func (s *Server) npmWhoami(w *recorder, req *request, match []string) error {
	if own, err := s.ownAccess(w, req, match[1], "npm"); !own || err != nil {
		return err
	}
	subject, err := s.subjectOf(req)
	if err != nil {
		if s.credentialError(w, req, err, false) {
			return nil
		}
		return err
	}
	name := ""
	switch {
	case subject != nil:
		name = subject.User.ID
	case s.sec.AnonymousEnabled():
		name = s.sec.Snapshot().Anonymous.UserID
	default:
		s.unauthorized(w, req, false)
		return nil
	}
	return npmAnswer(w, req, http.StatusOK, struct {
		Username string `json:"username"`
	}{name})
}

// maskToken is an access path with what follows npmTokenPath replaced by
// "-", so no access line names the token of npm logout, whatever the answer
// and the method (review S6). The access path is decoded before it is
// quoted again, so a percent escaped byte of the raw path shows the marker
// as well.
func maskToken(path string) string {
	if i := strings.Index(path, npmTokenPath); i >= 0 {
		return path[:i+len(npmTokenPath)] + "-"
	}
	return path
}

// npmLogout revokes the token of the path, which the signed in subject
// holds (npm logout). The access line names the path without the token
// (maskToken).
func (s *Server) npmLogout(w *recorder, req *request, match []string) error {
	if own, err := s.ownAccess(w, req, match[1], "npm"); !own || err != nil {
		return err
	}
	if allowed, err := s.allow(w, req, signedIn, match); err != nil || !allowed {
		return err
	}
	if err := s.sec.RevokeToken(req.subject, match[2]); err != nil {
		if answered, aerr := npmRefusal(w, req, err, ""); answered {
			return aerr
		}
		return err
	}
	return npmAnswer(w, req, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{true})
}

// dockerToken is the token route of the Docker flow: GET with Basic
// credentials, or none for an anonymous pull, gives a bearer token in the
// shape of the token authentication of the registry. The scope and the
// service of the query decide nothing.
func (s *Server) dockerToken(w *recorder, req *request, match []string) error {
	if own, err := s.ownAccess(w, req, match[1], "docker"); !own || err != nil {
		return err
	}
	tok, err := s.sec.DockerToken(req.r, match[1])
	if err != nil {
		var e *model.Error
		if !errors.As(err, &e) {
			return err
		}
		switch e.Status {
		case http.StatusUnauthorized:
			dockerError(w, req, e.Status, "UNAUTHORIZED", e.Message, [][2]string{{"WWW-Authenticate", basicChallenge}})
		case http.StatusTooManyRequests:
			dockerError(w, req, e.Status, "TOOMANYREQUESTS", e.Message, [][2]string{{"Retry-After", strconv.Itoa(max(e.RetryAfter, 1))}})
		default:
			dockerError(w, req, e.Status, "DENIED", e.Message, nil)
		}
		return nil
	}
	// #nosec G117 -- the one answer that shows a new token, in both fields the token authentication of the registry names.
	data, err := json.Marshal(struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		IssuedAt    string `json:"issued_at"`
	}{tok.Text, tok.Text, tok.ExpiresIn, tok.IssuedAt.UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	writeBody(w, req, http.StatusOK, [][2]string{{"Content-Type", jsonType}, {"Cache-Control", "no-store"},
		{docker.APIVersionHeader, docker.APIVersion}}, data)
	return nil
}

// dockerChallenge is the challenge of a 401 on a Docker route (N12): Bearer
// with the token route of the same host when the repository lets a token
// in, else Basic. The realm names only the host of the request, which
// passed the host guard, and its scheme (hardening line 12); a request of
// the registry API names its scope.
func (s *Server) dockerChallenge(req *request) string {
	repo, route := req.dockerRepo, "/v2/token"
	if repo == "" {
		t, err := parseTarget(req.raw, repositoryPrefix)
		if err != nil || t == nil {
			return basicChallenge
		}
		repo, route = t.repo, repositoryPrefix+quote(t.repo, "")+"/v2/token"
	}
	host := latin1Strip(req.r.Host)
	if !s.sec.DockerAccess(repo).Bearer || !localHost(host) && !namedHost(host, s.hostNames) {
		return basicChallenge
	}
	challenge := `Bearer realm="` + req.scheme + "://" + host + route + `",service="` + productName + `"`
	if scope := dockerScope(req, repo); scope != "" {
		challenge += `,scope="` + scope + `"`
	}
	return challenge
}

// dockerScope is the scope of the token authentication for a request of a
// name of the registry API, "" for the base and anything else. The actions
// follow the method, as the registry of the distribution project names them
// (registry/handlers/app.go, appendAccessRecords, read 2026-11-19): pull for
// GET and HEAD, pull and push for POST, PUT and PATCH, delete for DELETE; a
// mount names pull of its source repository as a second scope.
func dockerScope(req *request, repo string) string {
	t, err := parseTarget(req.raw, repositoryPrefix)
	if err != nil || t == nil || t.repo != repo || !strings.HasPrefix(t.path, "v2/") {
		return ""
	}
	name := dockerName(t.path[len("v2/"):])
	if name == "" {
		return ""
	}
	actions := "pull"
	switch req.r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		actions = "pull,push"
	case http.MethodDelete:
		actions = "delete"
	}
	scope := "repository:" + name + ":" + actions
	if req.r.Method == http.MethodPost {
		if q, err := url.ParseQuery(req.query); err == nil && q.Get("mount") != "" {
			if from := q.Get("from"); from != "" && dockerName(from+"/blobs/uploads/") == from {
				scope += " repository:" + from + ":pull"
			}
		}
	}
	return scope
}

// dockerName is the name of a path of the registry API below /v2/, the
// upload routes included; "" for none and for a name a quoted value cannot
// hold.
func dockerName(path string) string {
	call, err := docker.ParseWrite(path)
	if err != nil || call == nil || strings.ContainsAny(call.Name, `"\`) {
		return ""
	}
	return call.Name
}
