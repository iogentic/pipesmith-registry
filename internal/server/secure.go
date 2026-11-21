package server

import (
	"cmp"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	adminrules "github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/rest"
	rtable "github.com/iogentic/pipesmith-registry/internal/route"
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
	// crossSiteRefusal is the 403 of a write that a page of another site
	// sends (crossSite).
	crossSiteRefusal = "a write that a page of another origin sends is refused"
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
	ruleContent           // a REST write: the view permission of the repository of the id or of the query, with the action (rest_write.go)
	ruleCreate            // a create of a repository: the administration permission of the format of the segment in the path, any repository, add (stores.go)
	ruleListing           // a REST read of one repository: the browse permission of the repository of the id or of the query (allowContent)
)

type rule struct {
	kind        ruleKind
	permissions []string // ruleApp: any of them
	group       int      // ruleView, ruleAdmin, ruleAssetID: the match group of the repository or id
	action      string   // ruleView: browse or read; ruleAdmin: empty for read, or edit
	audit       string   // the action of the audit line of the route, also when it refuses; empty for the default
}

var (
	// browseQueried is the browse permission of the repository the query
	// parameter repository names: the listings of one repository.
	browseQueried = rule{kind: ruleListing, action: "browse"}
	public        = rule{kind: rulePublic}
	signedIn      = rule{kind: ruleSignedIn}
	client        = rule{kind: ruleClient}
)

// Empty is true for the zero rule, which the route table refuses (route.New).
func (r rule) Empty() bool { return r.kind == ruleNone }

func perm(permissions ...string) rule { return rule{kind: ruleApp, permissions: permissions} }

// audited is r with the action of the audit line of its route.
func audited(r rule, action string) rule {
	r.audit = action
	return r
}

func view(group int, action string) rule { return rule{kind: ruleView, group: group, action: action} }

func admin(group int) rule { return rule{kind: ruleAdmin, group: group} }

func exact(path string) *regexp.Regexp { return regexp.MustCompile("^" + path + "$") }

// entry is a route of the table, in the order pattern, methods, rule,
// handler.
func entry(pattern *regexp.Regexp, methods []string, r rule, handle handler) route {
	return route{Pattern: pattern, Methods: methods, Rule: r, Handle: handle}
}

// get is a GET route of internal/rest.
func get(path string, r rule, handle func(s *Server, w *recorder, req *request, match []string) error) route {
	return route{Pattern: exact(path), Methods: []string{"GET"}, Rule: r, Handle: handle}
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
// before those of step 2: the base routes, then the proposed paths of leg
// 6-api. The permission of each read is the one the contract names for its
// screen.
var secureRoutes = slices.Concat(baseRoutes, tokenRoutes, accountRoutes, taskSettingsRoutes, policyRoutes)

// baseRoutes are the routes of security on before leg 6-api.
var baseRoutes = []route{
	entry(exact("/api/session"), []string{"GET", "HEAD", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"}, public, (*Server).session),
	get(restBase+"/repositories", signedIn, plain(rest.Repositories)),
	get(restBase+"/repositories/"+segment, view(1, "browse"), named(rest.Repository)),
	get(restBase+"/repositories/"+segment+"/"+segment+"/"+segment, admin(3), func(s *Server, w *recorder, req *request, m []string) error {
		return s.restAnswer(w, req, func(env *rest.Env, _ url.Values) (rest.Answer, error) {
			return rest.RepositoryDetail(env, m[1], m[2], m[3])
		})
	}),
	get(restBase+"/repositorySettings", signedIn, plain(rest.RepositorySettings)),
	get(restBase+"/search", perm("nexus:search:read"), queried(rest.Search)),
	get(restBase+"/search/assets", perm("nexus:search:read"), queried(rest.SearchAssets)),
	get(restBase+"/search/assets/download", perm("nexus:search:read"), queried(rest.SearchAssetsDownload)),
	get(restBase+"/assets/"+segment, rule{kind: ruleAssetID, group: 1}, named(rest.Asset)),
	get(restBase+"/assets", browseQueried, queried(rest.Assets)),
	get(restBase+"/components", browseQueried, queried(rest.Components)),
	get(restBase+"/components/"+segment, rule{kind: ruleListing, group: 1, action: "browse"}, named(rest.ComponentByID)),
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
	entry(exact("/api/updates"), []string{"GET"}, perm("nexus:atlas:read"), (*Server).apiUpdates),
	entry(exact("/api/updates/check"), []string{"POST"}, perm("nexus:atlas:read"), (*Server).apiUpdatesCheck),
	get(restBase+"/read-only", perm(settingsRead), func(s *Server, w *recorder, req *request, _ []string) error {
		return s.restAnswer(w, req, func(*rest.Env, url.Values) (rest.Answer, error) {
			return rest.ReadOnly(s.readOnly.State(), !s.writes.clients), nil
		})
	}),
	get("/api/areas", signedIn, func(s *Server, w *recorder, req *request, _ []string) error {
		return s.restAnswer(w, req, func(*rest.Env, url.Values) (rest.Answer, error) {
			return rest.Areas(s.readOnly.State(), !s.writes.clients, s.refusedList()), nil
		})
	}),
	// the routes of tokens (tokens.go), more specific than /repository/(.*)
	entry(npmLoginPattern, []string{"PUT"}, public, (*Server).npmLogin),
	entry(exact(repositoryPrefix+segment+`/-/whoami`), []string{"GET"}, public, (*Server).npmWhoami),
	entry(npmLogoutPattern, []string{"DELETE"}, public, (*Server).npmLogout),
	entry(exact(repositoryPrefix+segment+`/v2/token`), []string{"GET"}, public, (*Server).dockerToken),
}

// The sign in and sign out of npm, which the read only mode lets through.
var (
	npmLoginPattern  = exact(repositoryPrefix + segment + `/-/user/org\.couchdb\.user:(.+)`)
	npmLogoutPattern = exact(repositoryPrefix + segment + npmTokenPath + `(.+)`)
)

// unfrozenWrite is a write the read only mode lets through: its path, and
// the methods it takes there, every write method when methods is nil.
type unfrozenWrite struct {
	*regexp.Regexp
	methods []string
}

// takes reports whether the write passes the read only mode.
func (u unfrozenWrite) takes(method, path string) bool {
	return (u.methods == nil || slices.Contains(u.methods, method)) && u.MatchString(path)
}

// unfrozen are the writes the read only mode lets through (ruling SB2): the
// session, the sign in and sign out of npm, the end of a freeze, the stop
// of a task (ruling SB8), and the tests and previews, which change nothing.
// Each is bound to its method, so a write of another method on the same
// path, such as the save of a policy named preview, waits (review F2).
var unfrozen = []unfrozenWrite{
	{exact("/api/session"), nil}, {npmLoginPattern, []string{"PUT"}}, {npmLogoutPattern, []string{"DELETE"}},
	{exact(restBase + "/read-only/(release|force-release)"), []string{"POST"}},
	{exact(restBase + "/tasks/" + segment + "/stop"), []string{"POST"}},
	{exact("/api/routing-rules/(test|preview)"), []string{"POST"}}, {exact("/api/cleanup-policies/preview"), []string{"POST"}},
	{exact("/api/ldap/verify-(connection|user-mapping|login)"), []string{"POST"}},
	{exact("/api/updates/check"), []string{"POST"}},
}

// taskRunPattern is the run of a task, which the read only mode refuses
// with 409 (ruling SB2).
var taskRunPattern = exact(restBase + "/tasks/" + segment + "/run")

const frozenRun = "the server is frozen, so no task starts; release the freeze first"

var freezePattern = exact(restBase + "/read-only/freeze")

// noEnd is the end of a request that holds no part of the read only mode.
func noEnd() {}

// freezeGate holds the read only mode shared for a write until end is
// called (ruling SB9), and answers a write while the mode is frozen: 409
// for the run of a task, 503 with the reason for every other change, in
// the shape of its path. The writes of unfrozen and the freeze itself pass
// without a hold. refused true means the answer is written.
func (s *Server) freezeGate(w *recorder, req *request) (end func(), refused bool) {
	if s.readOnly == nil || !writeMethod(req.r.Method) {
		return noEnd, false
	}
	for _, p := range unfrozen {
		if p.takes(req.r.Method, req.path) {
			return noEnd, false
		}
	}
	var state adminrules.ReadOnlyState
	if freezePattern.MatchString(req.path) {
		if state = s.readOnly.State(); !state.Frozen {
			return noEnd, false
		}
	} else {
		var ok bool
		if end, state, ok = s.readOnly.Begin(req.r.Context()); ok {
			return end, false
		}
	}
	switch {
	case taskRunPattern.MatchString(req.path):
		s.message(w, req, http.StatusConflict, frozenRun+". "+state.Reason)
	case req.dockerRepo != "" || dockerTarget(req):
		dockerError(w, req, http.StatusServiceUnavailable, "UNAVAILABLE", state.Reason, nil)
	case strings.HasPrefix(req.path, repositoryPrefix):
		writeText(w, req, http.StatusServiceUnavailable, state.Reason)
	default:
		s.message(w, req, http.StatusServiceUnavailable, state.Reason)
	}
	return nil, true
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

// writeRefusals are the requests that the REST writes serve while writes are
// on (rest_write.go). With writes off the three of docs/ui/API.md answer the
// 501 of 0.1.1, in the place 0.1.1 lists them. The field list of every
// format, which docs/ui/API.md does not name, answered 404 in 0.1.1; it
// answers like them since 2026-11-19 (decision of the planning session).
var writeRefusals = []refusalDecl{
	refusal(restBase+"/assets/{id}", "content", "DELETE"),
	refusal(restBase+"/formats/{format}/upload-specs", "content", "GET"),
	refusal(restBase+"/components", "content", "POST"),
	refusal(restBase+"/formats/upload-specs", "content", "GET"),
}

// keptMisses are the requests that keep the 404 of 0.1.1 with writes off
// although another method takes their path: the component delete, which
// docs/ui/API.md does not name, on the path of the read of a component.
var keptMisses = []struct {
	method  string
	pattern *regexp.Regexp
}{{"DELETE", exact(restBase + "/components/" + segment)}}

// keptMiss tells whether the request is one of keptMisses.
func keptMiss(req *request) bool {
	for _, m := range keptMisses {
		if req.r.Method == m.method && m.pattern.MatchString(req.path) {
			return true
		}
	}
	return false
}

// refusalTable holds the other requests of docs/ui/API.md at the taken
// commit of the interface (1e6d884) that this server does not serve: none
// since leg 6-api-b served the cleanup policies.
var refusalTable = []refusalDecl{}

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
	return route{Pattern: d.pattern(), Methods: d.methods, Rule: signedIn,
		Handle: func(s *Server, w *recorder, req *request, _ []string) error {
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
	out := []rest.Refused{}
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
	// refused is the list of GET /api/areas while writes are on; with
	// writes off the requests of writeRefusals come first, the three of
	// 0.1.1 in its order
	refused          = refusedOf(refusalTable)
	refusedWritesOff = refusedOf(slices.Concat(writeRefusals, refusalTable))
)

// securedRoutes is the table with security on: the new routes, the
// refusals, then the routes of step 2 with their rules. writingRoutes is the
// table while writes are on, with the REST writes in place of their
// refusals.
var (
	securedRoutes = slices.Concat(secureRoutes, identityRoutes, systemRoutes, statusAreaRoutes, figureRoutes, logRoutes, proxyRoutes, storeRoutes, routesOf(writeRefusals), refusalRoutes, routes)
	writingRoutes = slices.Concat(secureRoutes, identityRoutes, systemRoutes, statusAreaRoutes, figureRoutes, logRoutes, proxyRoutes, storeRoutes, contentWriteRoutes, refusalRoutes, routes)
)

// The three tables of the server, built once (internal/route refuses an
// entry without rule): security off, security on, security on with writes.
var (
	plainTable   = rtable.MustNew(routes)
	securedTable = rtable.MustNew(securedRoutes)
	writingTable = rtable.MustNew(writingRoutes)
)

func (s *Server) table() *routeTable {
	switch {
	case s.sec == nil:
		return plainTable
	case s.writes.clients:
		return writingTable
	}
	return securedTable
}

// refusedList is the list of GET /api/areas.
func (s *Server) refusedList() []rest.Refused {
	if s.writes.clients {
		return refused
	}
	return refusedWritesOff
}

// subjectOf resolves the caller once per request.
func (s *Server) subjectOf(req *request) (*model.Subject, error) {
	if !req.resolved {
		req.subject, req.subjectErr = s.sec.Subject(req.r)
		noteSubject(req.r, req.subject)
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
	if a.Location != "" {
		writeBody(w, req, a.Status, [][2]string{{"Location", a.Location}}, nil)
		return nil
	}
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
		s.unauthorizedWith(w, req, clientPath, e.Message)
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
// the session, so no credential dialog opens; a Docker route gets the
// challenge of N12 (dockerChallenge); other scripts and clients get Basic.
// The client read path answers in the shape of its protocol.
func (s *Server) unauthorized(w *recorder, req *request, clientPath bool) {
	s.unauthorizedWith(w, req, clientPath, model.ErrUnauthorized.Message)
}

// unauthorizedWith is unauthorized with the message of a refusal of
// Security, as the answer to a token in the shape of the old product.
func (s *Server) unauthorizedWith(w *recorder, req *request, clientPath bool, message string) {
	docker := clientPath && (req.dockerRepo != "" || dockerTarget(req))
	challenge := basicChallenge
	switch {
	case strings.HasPrefix(req.path, "/api/") || fromBrowser(req.r):
		challenge = sessionChallenge
	case docker:
		challenge = s.dockerChallenge(req)
	}
	header := [2]string{"WWW-Authenticate", challenge}
	switch {
	case docker:
		dockerError(w, req, http.StatusUnauthorized, "UNAUTHORIZED", message, [][2]string{header})
	case clientPath:
		text := "Authentication required\n"
		if message != model.ErrUnauthorized.Message {
			text = message + "\n"
		}
		writeBody(w, req, http.StatusUnauthorized, [][2]string{{"Content-Type", textType}, {"X-Content-Type-Options", "nosniff"}, header},
			[]byte(text))
	default:
		s.message(w, req, http.StatusUnauthorized, message, header)
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

// crossSite reports a request that a page of another site sends (assessment
// A3 of review 4): its Origin names another origin than the one of the
// request, scheme and host as this server sees them, or its Sec-Fetch-Site
// says cross-site. A browser sends the Basic credentials it holds for this
// server with such a request too. Maven, npm, Docker and curl send neither
// header.
func crossSite(req *request) bool {
	if strings.EqualFold(strings.TrimSpace(req.r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return true
	}
	for _, origin := range req.r.Header.Values("Origin") {
		if !sameOrigin(origin, req.scheme, req.r.Host) {
			return true
		}
	}
	return false
}

// sameOrigin compares an Origin header with the scheme and the Host header
// of a request; the default port of the scheme counts as no port, as a
// browser leaves it out of an origin.
func sameOrigin(origin, scheme, host string) bool {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil || !strings.EqualFold(u.Scheme, scheme) {
		return false
	}
	return hostPort(u.Host, scheme) == hostPort(host, scheme)
}

// hostPort is a host and its port in lower case, "" for the default port of
// the scheme.
func hostPort(host, scheme string) string {
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		name, port = host, ""
	}
	if port == strconv.Itoa(defaultPorts[strings.ToLower(scheme)]) {
		port = ""
	}
	return strings.ToLower(strings.Trim(name, "[]")) + " " + port
}

// writeOfAnotherSite refuses a write of the write path that a page of
// another site sends, with 403 and before its body is read: a client write
// below /repository/ and on the Docker ports, and a REST write. false means
// the answer is written.
func (s *Server) writeOfAnotherSite(w *recorder, req *request, r rule) bool {
	writePath := r.kind == ruleContent || r.kind == ruleClient && strings.HasPrefix(req.path, repositoryPrefix)
	if s.writes.writer == nil || !writeMethod(req.r.Method) || !writePath || !crossSite(req) {
		return false
	}
	switch {
	case r.kind == ruleContent:
		s.message(w, req, http.StatusForbidden, crossSiteRefusal)
	case req.dockerRepo != "" || dockerTarget(req):
		dockerError(w, req, http.StatusForbidden, "DENIED", crossSiteRefusal, nil)
	default:
		writeText(w, req, http.StatusForbidden, crossSiteRefusal)
	}
	return true
}

// allow applies the rule of a route; false means the answer is written.
func (s *Server) allow(w *recorder, req *request, r rule, match []string) (bool, error) {
	if r.kind == rulePublic {
		return true, nil
	}
	if s.writeOfAnotherSite(w, req, r) {
		return false, nil
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
	case ruleContent, ruleListing:
		return s.allowContent(w, req, subject, r, match)
	case ruleApp:
		needed = r.permissions
	case ruleCreate:
		needed = []string{rest.AdminPermission(cmp.Or(adminrules.FormatOfSegment(match[r.group]), match[r.group]), "*", "add")}
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
			if action == "" {
				action = "read"
			}
			needed = []string{rest.AdminPermission(format, name, action)}
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
	if r.kind == ruleApp || r.kind == ruleCreate {
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
// route. While writes are on a write below /repository/ asks the action of
// writeAction instead (seam Permission of a write), and a nil subject is
// asked for credentials whatever the anonymous user holds: no write without
// credentials (decision E4), and the Docker token of the anonymous subject
// is none. Else a nil subject holds the rights of the anonymous user while
// anonymous access is on and is asked for credentials when they do not
// suffice. The Docker base /v2/ of a Docker port needs sign in only, and a
// nil subject is asked for credentials there unless the anonymous user may
// pull from the repository (row 4-15 of review 4), so docker login checks
// its password where the repository forces Basic. A path
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
		if subject == nil && !s.sec.DockerAccess(req.dockerRepo).AnonymousPull {
			s.unauthorized(w, req, true)
			return false, nil
		}
		return true, nil
	}
	if s.writes.clients && prefix == repositoryPrefix {
		if action, err = s.writeAction(req.r.Method, t); err != nil {
			return false, err
		}
		if writeMethod(req.r.Method) && subject == nil {
			s.unauthorized(w, req, true)
			return false, nil
		}
		// a write of a browser session needs its request token here too
		if writeMethod(req.r.Method) && !s.sec.CSRF(req.r, subject) {
			writeText(w, req, http.StatusForbidden, csrfRefusal)
			return false, nil
		}
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

// restEnv is what internal/rest builds the answer of one request from. The
// url and downloadUrl fields take the scheme of the request (origin), as
// every URL the server builds does (review R3).
func (s *Server) restEnv(req *request) (*rest.Env, error) {
	st, err := s.rp.sync()
	if err != nil {
		return nil, err
	}
	list := make([]rest.Store, 0, len(st.configs))
	attached := make([]string, 0, len(st.configs))
	for _, sc := range st.configs {
		store := st.stores[sc.Name]
		list = append(list, rest.Store{Name: sc.Name, Kind: sc.Kind, Root: sc.Root, Available: store != nil && !stores.LooksUnmounted(store),
			Readable: store != nil && rest.ReadableStore(store.Root(), sc.Kind == stores.KindBlobStore)})
		attached = append(attached, sc.Name)
	}
	subject := req.subject
	env := &rest.Env{
		Context:      req.r.Context(),
		Snapshot:     s.sec.Snapshot(),
		Repositories: st.repositories,
		Index:        s.rp.index,
		Catalog:      s.catalog,
		Stores:       list,
		Permits:      func(p string) bool { return s.sec.Permits(subject, p) },
		Base:         baseURL(req),
		StoresRoot:   filepath.Join(s.rp.cfg.DataDir, adminrules.StoresDir),
		Attached:     attached,
	}
	if s.writes.file != nil {
		env.Details = s.writes.file // leg F1b: the side data of the writes
		env.Components = s.writes.file
		env.CatalogBehind = func() (string, error) {
			stats, err := s.rp.index.StoreStats()
			if err != nil {
				return "", err
			}
			return s.writes.file.Behind(stats)
		}
	}
	return env, nil
}

// Limits of the REST reads (task 12 of the phase 1c plan): the query string
// of a request, and the time the answer may take to compute and send. The
// search ends after its timeout of at most two minutes; the write deadline
// gives the answer one more minute to reach a slow client and then frees
// the connection.
const (
	maxRESTQuery   = 8192
	restWriteLimit = 3 * time.Minute
)

func (s *Server) restAnswer(w *recorder, req *request, build func(env *rest.Env, q url.Values) (rest.Answer, error)) error {
	if len(req.query) > maxRESTQuery {
		s.message(w, req, http.StatusBadRequest, "the query string is longer than "+strconv.Itoa(maxRESTQuery)+" bytes")
		return nil
	}
	rc := http.NewResponseController(w.ResponseWriter)
	if rc.SetWriteDeadline(time.Now().Add(restWriteLimit)) == nil {
		defer func() { _ = rc.SetWriteDeadline(time.Time{}) }() // the next request of the connection starts without one
	}
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
