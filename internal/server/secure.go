package server

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/rest"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/version"
)

// Security on: the routes of the interface, of internal/rest and of the
// refusals, and the rule of every route (thin slice decisions T1, T2, T6,
// T7 and T8). With a nil Options.Security none of this is reached and the
// server answers as in step 2.

const (
	restBase = "/service/rest/v1"
	// basicChallenge asks a client for Basic credentials; a browser opens
	// its credential dialog for it.
	basicChallenge = `Basic realm="` + productName + `"`
	// sessionChallenge names the sign in of the interface, so that a
	// browser opens no dialog (N13 of the phase 3 plan).
	sessionChallenge = `Session realm="` + productName + `"`
	csrfRefusal      = "missing or wrong request token"
	// repositoryRefusal is the 403 of every rule that depends on a
	// repository: it names neither the format nor the permission, so a
	// missing repository and a forbidden one answer alike (review 6,
	// finding 7). The text is the message of DENIED in the Docker registry
	// protocol.
	repositoryRefusal = "requested access to the resource is denied"
)

// ruleKind is what a route asks of the caller.
type ruleKind int

const (
	ruleNone     ruleKind = iota
	rulePublic            // nothing; the page, its files, health, status, the session routes
	ruleSignedIn          // a subject; the handler shows what it may see
	ruleApp               // a subject with one of the permissions
	ruleView              // a subject with the view permission of the repository in the path
	ruleAdmin             // a subject with the administration permission of the repository in the path
	ruleAssetID           // a subject with the browse permission of the repository of the asset id
	ruleClient            // the client read path: the view permission of the repository in the path (T7, review 6)
)

type rule struct {
	kind        ruleKind
	permissions []string // ruleApp: any of them
	group       int      // ruleView, ruleAdmin, ruleAssetID: the match group of the repository or id
	action      string   // ruleView: browse or read
}

var (
	public   = rule{kind: rulePublic}
	signedIn = rule{kind: ruleSignedIn}
	client   = rule{kind: ruleClient}
)

func perm(permissions ...string) rule { return rule{kind: ruleApp, permissions: permissions} }

func view(group int, action string) rule { return rule{kind: ruleView, group: group, action: action} }

func admin(group int) rule { return rule{kind: ruleAdmin, group: group} }

func exact(path string) *regexp.Regexp { return regexp.MustCompile("^" + path + "$") }

// get is a GET route of internal/rest.
func get(path string, r rule, handle func(s *Server, w *recorder, req *request, match []string) error) route {
	return route{pattern: exact(path), methods: []string{"GET"}, rule: r, handle: handle}
}

// plain wraps a function of internal/rest that needs no path value.
func plain(build func(env *rest.Env) (rest.Answer, error)) func(*Server, *recorder, *request, []string) error {
	return func(s *Server, w *recorder, req *request, _ []string) error {
		return s.restAnswer(w, req, func(env *rest.Env, _ url.Values) (rest.Answer, error) { return build(env) })
	}
}

// named wraps a function of internal/rest that takes the first path value.
func named(build func(env *rest.Env, name string) (rest.Answer, error)) func(*Server, *recorder, *request, []string) error {
	return func(s *Server, w *recorder, req *request, match []string) error {
		return s.restAnswer(w, req, func(env *rest.Env, _ url.Values) (rest.Answer, error) { return build(env, match[1]) })
	}
}

// queried wraps a function of internal/rest that reads the query.
func queried(build func(env *rest.Env, q url.Values) (rest.Answer, error)) func(*Server, *recorder, *request, []string) error {
	return func(s *Server, w *recorder, req *request, _ []string) error {
		return s.restAnswer(w, req, build)
	}
}

const (
	settingsRead = "nexus:settings:read"
	segment      = `([^/]+)`
)

// secureRoutes are the routes that exist only with security on, matched
// before those of step 2. The permission of each read is the one the
// contract names for its screen.
var secureRoutes = []route{
	{exact("/api/session"), []string{"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"}, public, (*Server).session},
	get(restBase+"/repositories", signedIn, plain(rest.Repositories)),
	get(restBase+"/repositories/"+segment, view(1, "browse"), named(rest.Repository)),
	get(restBase+"/repositories/"+segment+"/"+segment+"/"+segment, admin(3), func(s *Server, w *recorder, req *request, m []string) error {
		return s.restAnswer(w, req, func(env *rest.Env, _ url.Values) (rest.Answer, error) {
			return rest.RepositoryDetail(env, m[1], m[2], m[3])
		})
	}),
	get(restBase+"/repositorySettings", signedIn, plain(rest.RepositorySettings)),
	get(restBase+"/search", perm("nexus:search:read"), queried(rest.Search)),
	get(restBase+"/assets/"+segment, rule{kind: ruleAssetID, group: 1}, named(rest.Asset)),
	get(restBase+"/blobstores", perm("nexus:blobstores:read"), plain(rest.BlobStores)),
	get(restBase+"/blobstores/file/"+segment, perm("nexus:blobstores:read"), named(rest.FileBlobStore)),
	get(restBase+"/blobstores/"+segment+"/quota-status", perm("nexus:blobstores:read"), named(rest.QuotaStatus)),
	get(restBase+"/security/privileges", perm("nexus:privileges:read"), plain(rest.Privileges)),
	get(restBase+"/security/privileges/"+segment, perm("nexus:privileges:read"), named(rest.Privilege)),
	get(restBase+"/security/roles", perm("nexus:roles:read"), queried(rest.Roles)),
	get(restBase+"/security/roles/"+segment, perm("nexus:roles:read"), named(rest.Role)),
	get(restBase+"/security/user-sources", perm("nexus:users:read", "nexus:roles:read"), plain(rest.UserSources)),
	get(restBase+"/security/users", perm("nexus:users:read"), queried(rest.Users)),
	get(restBase+"/security/anonymous", perm(settingsRead), plain(rest.Anonymous)),
	get(restBase+"/security/realms/available", perm(settingsRead), plain(rest.RealmsAvailable)),
	get(restBase+"/security/realms/active", perm(settingsRead), plain(rest.RealmsActive)),
	get(restBase+"/security/ldap", perm("nexus:ldap:read"), plain(rest.LDAPServers)),
	get(restBase+"/security/ldap/"+segment, perm("nexus:ldap:read"), named(rest.LDAPServer)),
	get(restBase+"/status/check", perm("nexus:atlas:read"), plain(rest.StatusChecks)),
	get(restBase+"/read-only", perm(settingsRead), plain(func(*rest.Env) (rest.Answer, error) { return rest.ReadOnly(), nil })),
	get("/api/areas", signedIn, plain(func(*rest.Env) (rest.Answer, error) { return rest.Areas(refused), nil })),
}

// A refusal is a request of the contract this server does not serve yet: a
// subject, the request token on a write, then 501 with the area (T8). Each
// is declared once as a template in the spelling of docs/ui/API.md: {name}
// is one path segment, {name+} one or more (listed as {name}), and (a|b)
// one request per alternative. The route patterns and the list of
// GET /api/areas both come from the templates, so they cannot differ.
type refusalDecl struct {
	template string
	area     string
	methods  []string
}

func refusal(template, area string, methods ...string) refusalDecl {
	return refusalDecl{template: template, area: area, methods: methods}
}

const usersBase = restBase + "/security/users"

// refusalTable holds the other requests of docs/ui/API.md at the taken
// commit of the interface (1e6d884).
var refusalTable = []refusalDecl{
	refusal(restBase+"/assets/{id}", "content", "DELETE"),
	refusal(restBase+"/formats/{format}/upload-specs", "content", "GET"),
	refusal(restBase+"/components", "content", "POST"),
	refusal(restBase+"/repositories/{segment}/{type}", "repositories", "POST"),
	refusal(restBase+"/repositories/{segment}/{type}/{name}", "repositories", "PUT"),
	refusal(restBase+"/repositories/{name}", "repositories", "DELETE"),
	refusal(restBase+"/repositories/{name}/(rebuild-index|invalidate-cache)", "repositories", "POST"),
	refusal("/api/cleanup-policies", "cleanup-policies", "GET", "POST"),
	refusal("/api/cleanup-policies/preview", "cleanup-policies", "POST"),
	refusal("/api/cleanup-policies/{name}", "cleanup-policies", "GET", "PUT", "DELETE"),
	refusal(restBase+"/routing-rules", "routing-rules", "GET", "POST"),
	refusal(restBase+"/routing-rules/{name}", "routing-rules", "GET", "PUT", "DELETE"),
	refusal("/api/routing-rules/(test|preview)", "routing-rules", "POST"),
	refusal(restBase+"/blobstores/file", "blob-stores", "POST"),
	refusal(restBase+"/blobstores/file/{name}", "blob-stores", "PUT"),
	refusal(restBase+"/blobstores/{name}", "blob-stores", "DELETE"),
	refusal("/api/blob-stores/{name}/deleted", "blob-stores", "GET"),
	refusal(restBase+"/security/roles", "security", "POST"),
	refusal(restBase+"/security/roles/{id}", "security", "PUT", "DELETE"),
	refusal(usersBase, "security", "POST"),
	refusal(usersBase+"/{userId}", "security", "PUT", "DELETE"),
	refusal(usersBase+"/{userId}/change-password", "security", "PUT"),
	refusal(restBase+"/security/anonymous", "security", "PUT"),
	refusal(restBase+"/security/realms/active", "security", "PUT"),
	refusal(restBase+"/security/ldap", "ldap", "POST"),
	refusal(restBase+"/security/ldap/change-order", "ldap", "POST"),
	refusal(restBase+"/security/ldap/{name}", "ldap", "PUT", "DELETE"),
	refusal("/api/ldap/(clear-cache|verify-connection|verify-user-mapping|verify-login)", "ldap", "POST"),
	refusal("/api/tokens", "tokens", "GET", "POST"),
	refusal("/api/tokens/{id}", "tokens", "DELETE"),
	refusal("/api/account", "account", "PUT"),
	refusal("/api/account/password", "account", "POST"),
	refusal("/api/logging/loggers", "logging", "GET"),
	refusal("/api/logging/loggers/{name}", "logging", "PUT", "DELETE"),
	refusal("/api/logging/reset", "logging", "POST"),
	refusal("/api/logs", "logs", "GET"),
	refusal("/api/logs/{name+}", "logs", "GET"),
	refusal("/service/rest/beta/system/information", "system", "GET"),
	refusal(restBase+"/capabilities", "capabilities", "GET"),
	refusal(restBase+"/capabilities/{id}", "capabilities", "PUT", "DELETE"),
	refusal("/api/capabilities/support", "capabilities", "GET"),
	refusal(restBase+"/system/node", "system", "GET"),
	refusal(restBase+"/read-only/(freeze|release|force-release)", "system", "POST"),
	refusal(restBase+"/tasks", "tasks", "GET"),
	refusal(restBase+"/tasks/{id}", "tasks", "GET"),
	refusal(restBase+"/tasks/{id}/(run|stop)", "tasks", "POST"),
	refusal("/api/tasks", "tasks", "POST"),
	refusal("/api/tasks/{id}", "tasks", "GET", "PUT", "DELETE"),
	refusal("/api/task-types", "tasks", "GET"),
	refusal("/api/metrics/(requests|downloads)", "metrics", "GET"),
}

// templatePart finds the variable parts of a refusal template.
var templatePart = regexp.MustCompile(`\{[A-Za-z]+\+?\}|\([a-z-]+(\|[a-z-]+)+\)`)

// pattern is the route pattern of the template.
func (d refusalDecl) pattern() *regexp.Regexp {
	var b strings.Builder
	last := 0
	for _, m := range templatePart.FindAllStringIndex(d.template, -1) {
		b.WriteString(regexp.QuoteMeta(d.template[last:m[0]]))
		switch part := d.template[m[0]:m[1]]; {
		case strings.HasSuffix(part, "+}"):
			b.WriteString(`(.+)`)
		case part[0] == '{':
			b.WriteString(segment)
		default: // alternatives of plain words, as templatePart allows them
			b.WriteString(part)
		}
		last = m[1]
	}
	b.WriteString(regexp.QuoteMeta(d.template[last:]))
	return exact(b.String())
}

// paths are the paths of the template in the spelling of the contract, one
// per alternative.
func (d refusalDecl) paths() []string {
	paths := []string{""}
	last := 0
	for _, m := range templatePart.FindAllStringIndex(d.template, -1) {
		literal, part := d.template[last:m[0]], d.template[m[0]:m[1]]
		alternatives := []string{strings.TrimSuffix(strings.TrimSuffix(part, "}"), "+") + "}"}
		if part[0] == '(' {
			alternatives = strings.Split(part[1:len(part)-1], "|")
		}
		var next []string
		for _, p := range paths {
			for _, a := range alternatives {
				next = append(next, p+literal+a)
			}
		}
		paths, last = next, m[1]
	}
	for i := range paths {
		paths[i] += d.template[last:]
	}
	return paths
}

func (d refusalDecl) route() route {
	area := d.area
	return route{pattern: d.pattern(), methods: d.methods, rule: signedIn,
		handle: func(s *Server, w *recorder, req *request, _ []string) error {
			return s.writeAnswer(w, req, rest.NotImplemented(area, req.r.Method+" "+req.path))
		}}
}

func routesOf(table []refusalDecl) []route {
	out := make([]route, 0, len(table))
	for _, d := range table {
		out = append(out, d.route())
	}
	return out
}

// refusedOf is the list of GET /api/areas: one entry per method and path.
func refusedOf(table []refusalDecl) []rest.Refused {
	var out []rest.Refused
	for _, d := range table {
		for _, method := range d.methods {
			for _, path := range d.paths() {
				out = append(out, rest.Refused{Method: method, Path: path, Area: d.area})
			}
		}
	}
	return out
}

var (
	refusalRoutes = routesOf(refusalTable)
	refused       = refusedOf(refusalTable)
)

// securedRoutes is the table with security on: the new routes, the
// refusals, then the routes of step 2 with their rules.
var securedRoutes = slices.Concat(secureRoutes, refusalRoutes, routes)

func (s *Server) table() []route {
	if s.sec == nil {
		return routes
	}
	return securedRoutes
}

// subjectOf resolves the caller once per request.
func (s *Server) subjectOf(req *request) (*model.Subject, error) {
	if !req.resolved {
		req.subject, req.subjectErr = s.sec.Subject(req.r)
		req.resolved = true
	}
	return req.subject, req.subjectErr
}

// fromBrowser is a request a browser sends from a page or with a cookie:
// a Basic challenge would open its credential dialog.
func fromBrowser(r *http.Request) bool {
	if r.Header.Get("Cookie") != "" {
		return true
	}
	mode := r.Header.Get("Sec-Fetch-Mode")
	return mode != "" && mode != "navigate"
}

func writeMethod(method string) bool {
	return method == "POST" || method == "PUT" || method == "DELETE" || method == "PATCH"
}

var restHeaders = [][2]string{{"Content-Type", jsonType}, {"X-Content-Type-Options", "nosniff"}, {"Cache-Control", "no-store"}}

func (s *Server) writeAnswer(w *recorder, req *request, a rest.Answer) error {
	data, err := a.JSON()
	if err != nil {
		return err
	}
	writeBody(w, req, a.Status, restHeaders, data)
	return nil
}

func (s *Server) message(w *recorder, req *request, status int, text string, extra ...[2]string) {
	data, _ := rest.Answer{Body: map[string]string{"message": text}}.JSON() // a map of strings always encodes
	writeBody(w, req, status, append(slices.Clone(restHeaders), extra...), data)
}

// credentialError answers a failed resolution of the caller: 401 with the
// challenge of the route, 429 with Retry-After and no body, another status
// of Security with its message. ok is false for an error Security does not
// give as *model.Error.
func (s *Server) credentialError(w *recorder, req *request, err error, clientPath bool) (ok bool) {
	var e *model.Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Status {
	case http.StatusUnauthorized:
		s.unauthorized(w, req, clientPath)
	case http.StatusTooManyRequests:
		retry := strconv.Itoa(max(e.RetryAfter, 1))
		if req.dockerRepo != "" || dockerTarget(req) {
			dockerError(w, req, http.StatusTooManyRequests, "TOOMANYREQUESTS", e.Message, [][2]string{{"Retry-After", retry}})
		} else {
			writeBody(w, req, http.StatusTooManyRequests, [][2]string{{"Retry-After", retry}}, nil)
		}
	default:
		s.message(w, req, e.Status, e.Message)
	}
	return true
}

var dockerPath = regexp.MustCompile(`^/repository/[^/]+/v2(/|$)`)

// dockerTarget is a request of the registry protocol on the main port.
func dockerTarget(req *request) bool { return dockerPath.MatchString(req.path) }

// unauthorized is the 401 of a request without a subject. The interface
// routes below /api/ and every request of a browser get the challenge of
// the session, so no credential dialog opens; scripts and clients get
// Basic. The client read path answers in the shape of its protocol.
func (s *Server) unauthorized(w *recorder, req *request, clientPath bool) {
	challenge := basicChallenge
	if strings.HasPrefix(req.path, "/api/") || fromBrowser(req.r) {
		challenge = sessionChallenge
	}
	header := [2]string{"WWW-Authenticate", challenge}
	switch {
	case clientPath && (req.dockerRepo != "" || dockerTarget(req)):
		dockerError(w, req, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required", [][2]string{header})
	case clientPath:
		writeBody(w, req, http.StatusUnauthorized, [][2]string{{"Content-Type", textType}, {"X-Content-Type-Options", "nosniff"}, header},
			[]byte("Authentication required\n"))
	default:
		s.message(w, req, http.StatusUnauthorized, model.ErrUnauthorized.Message, header)
	}
}

// formatOf is the format of a repository for its permission, the format
// only a wildcard grant implies for a name that does not exist.
func (s *Server) formatOf(name string) (string, error) {
	st, err := s.rp.sync()
	if err != nil {
		return "", err
	}
	if r, found := st.repositories.Get(name); found {
		return r.Format, nil
	}
	return rest.UnknownFormat, nil
}

// allow applies the rule of a route; false means the answer is written.
func (s *Server) allow(w *recorder, req *request, r rule, match []string) (bool, error) {
	if r.kind == rulePublic {
		return true, nil
	}
	subject, err := s.subjectOf(req)
	if err != nil && r.kind == ruleClient && s.staleCookie(req, err) {
		req.subject, req.subjectErr = nil, nil
		subject, err = nil, nil
	}
	if err != nil {
		if s.credentialError(w, req, err, r.kind == ruleClient) {
			return false, nil
		}
		return false, err
	}
	if r.kind == ruleClient {
		return s.allowClient(w, req, subject)
	}
	if subject == nil {
		s.unauthorized(w, req, false)
		return false, nil
	}
	if writeMethod(req.r.Method) && !s.sec.CSRF(req.r, subject) {
		s.message(w, req, http.StatusForbidden, csrfRefusal)
		return false, nil
	}
	var needed []string
	switch r.kind {
	case ruleSignedIn:
		return true, nil
	case ruleApp:
		needed = r.permissions
	case ruleView, ruleAdmin, ruleAssetID:
		name, action := match[r.group], r.action
		if r.kind == ruleAssetID {
			repo, _, valid := rest.DecodeAssetID(name)
			if !valid {
				return true, nil // the handler answers 422 for an id it cannot have made
			}
			name, action = repo, "browse"
		}
		format, err := s.formatOf(name)
		if err != nil {
			return false, err
		}
		if r.kind == ruleAdmin {
			needed = []string{rest.AdminPermission(format, name, "read")}
		} else {
			needed = []string{rest.ViewPermission(format, name, action)}
		}
	default:
		return false, errors.New("a route without a rule")
	}
	for _, p := range needed {
		if s.sec.Permits(subject, p) {
			return true, nil
		}
	}
	if r.kind == ruleApp {
		s.message(w, req, http.StatusForbidden, "this needs the permission "+strings.Join(needed, " or "))
	} else {
		s.message(w, req, http.StatusForbidden, repositoryRefusal)
	}
	return false, nil
}

// staleCookie is a refusal that only a cookie of no live session can have
// caused: anonymous access is on and no Authorization header came with it.
// On the client path it counts as no credentials, so a browser is not shut
// out of the open path until the interface clears its cookie (review 6,
// finding 9).
func (s *Server) staleCookie(req *request, err error) bool {
	var e *model.Error
	return errors.As(err, &e) && e.Status == http.StatusUnauthorized && s.sec.AnonymousEnabled() &&
		req.r.Header.Get("Authorization") == ""
}

// allowClient is the rule of the client read path (T7 with the decision on
// review 6, finding 3): the view permission of the repository in the path,
// read below /repository/ and on the Docker ports, browse below the browse
// route. A nil subject holds the rights of the anonymous user while
// anonymous access is on and is asked for credentials when they do not
// suffice. The Docker base /v2/ of a Docker port needs sign in only. A path
// that names no repository goes on to the handler, whose answer names none.
func (s *Server) allowClient(w *recorder, req *request, subject *model.Subject) (bool, error) {
	if subject == nil && !s.sec.AnonymousEnabled() {
		s.unauthorized(w, req, true)
		return false, nil
	}
	prefix, action := repositoryPrefix, "read"
	if strings.HasPrefix(req.path, browsePrefix) {
		prefix, action = browsePrefix, "browse"
	}
	t, err := parseTarget(req.raw, prefix)
	if err != nil || t == nil {
		return true, nil
	}
	if req.dockerRepo != "" && t.path == "v2" {
		return true, nil
	}
	format, err := s.formatOf(t.repo)
	if err != nil {
		return false, err
	}
	if s.sec.Permits(subject, rest.ViewPermission(format, t.repo, action)) {
		return true, nil
	}
	if subject == nil {
		s.unauthorized(w, req, true)
		return false, nil
	}
	if req.dockerRepo != "" || dockerTarget(req) {
		dockerError(w, req, http.StatusForbidden, "DENIED", repositoryRefusal, nil)
	} else {
		writeText(w, req, http.StatusForbidden, repositoryRefusal)
	}
	return false, nil
}

// session hands /api/session to the identity code.
func (s *Server) session(w *recorder, req *request, _ []string) error {
	s.sec.SessionHandler().ServeHTTP(w, req.r)
	return nil
}

// restEnv is what internal/rest builds the answer of one request from.
func (s *Server) restEnv(req *request) (*rest.Env, error) {
	st, err := s.rp.sync()
	if err != nil {
		return nil, err
	}
	list := make([]rest.Store, 0, len(st.configs))
	for _, sc := range st.configs {
		store := st.stores[sc.Name]
		list = append(list, rest.Store{Name: sc.Name, Kind: sc.Kind, Root: sc.Root, Available: store != nil && !stores.LooksUnmounted(store)})
	}
	scheme := "http"
	if req.r.TLS != nil {
		scheme = "https"
	}
	subject := req.subject
	return &rest.Env{
		Context:      req.r.Context(),
		Snapshot:     s.sec.Snapshot(),
		Repositories: st.repositories,
		Index:        s.rp.index,
		Catalog:      s.catalog,
		Stores:       list,
		Permits:      func(p string) bool { return s.sec.Permits(subject, p) },
		Base:         scheme + "://" + req.r.Host,
	}, nil
}

func (s *Server) restAnswer(w *recorder, req *request, build func(env *rest.Env, q url.Values) (rest.Answer, error)) error {
	q, err := url.ParseQuery(req.query)
	if err != nil {
		s.message(w, req, http.StatusBadRequest, "the query string cannot be read")
		return nil
	}
	env, err := s.restEnv(req)
	if err != nil {
		return err
	}
	a, err := build(env, q)
	if err != nil {
		return err
	}
	return s.writeAnswer(w, req, a)
}

// mayBrowse filters the lists of step 2 with security on.
func (s *Server) mayBrowse(req *request, r repos.Repo) bool {
	return s.sec.Permits(req.subject, rest.ViewPermission(r.Format, r.Name, "browse"))
}

// shortStatus is /api/status without a subject: name and version only.
func shortStatus(w *recorder, req *request) error {
	return writeJSON(w, req, http.StatusOK, pyjson.NewObject().Set("name", productName).Set("version", version.Version))
}
