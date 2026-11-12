// Package server is the read path of the Python server: the host guard, the
// health and status routes, files of hosted, proxy and group repositories
// under /repository/<name>/<path> with the file answer of _send, the merged
// maven-metadata.xml of groups with its checksums, the npm read path, the
// folder listing of every directory URL, the Docker registry read path on
// the main port and on the Docker ports, the JSON API with the scan job,
// the page and the browse routes. Nothing here writes into a store; the
// index is written by Index.Drop for a row whose bytes file has vanished
// and by a scan started through POST /api/scan.
package server

import (
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- the ETag of the old product is the SHA-1 of the bytes, not a security use.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/rest"
	"github.com/iogentic/pipesmith-registry/internal/ui"
)

const (
	repositoryPrefix = "/repository/"
	browsePrefix     = "/service/rest/repository/browse/"
	allow            = "GET, HEAD"
	chunkBytes       = 256 * 1024
	textType         = "text/plain; charset=utf-8"
	octet            = "application/octet-stream"
	jsonType         = "application/json"
	dockerBlobPrefix = "v2/-/blobs/"
	forbidden        = "Forbidden: the Host header does not name this server"

	slogInfo = slog.LevelInfo
	slogWarn = slog.LevelWarn
)

var (
	localHostNames    = [...]string{"localhost", "127.0.0.1", "[::1]"}
	fileAccess        = [...]string{"maven", "files"} // access kinds the read path serves
	coordinateFormats = [...]string{"maven2", "unknown"}
	hostHeader        = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[^:\[\]]+)(?::[0-9]{1,5})?$`)
)

// Options configure a Server.
type Options struct {
	// AccessLog receives one line per request in the shape of uvicorn;
	// nil writes none.
	AccessLog io.Writer
	// Security turns security on: the interface at "/", the REST reads of
	// its screens, the refusals of the rest of its contract and the rule of
	// every route. Nil answers as in step 2.
	Security model.Security
	// HostNames are the names a Host header may carry besides localhost,
	// 127.0.0.1 and [::1]: lower case, an IPv6 address in brackets, no port.
	HostNames []string
}

// Server is the http.Handler of every listener of one server process.
type Server struct {
	rp          *readPath
	dockerPorts map[int]string // listening port -> Docker repository
	accessMu    sync.Mutex
	access      io.Writer
	marker      string // the gate puts it before a target without leading "/"
	escape      string // the gate puts it before a header value it escapes
	trusted     *trustedHosts
	port        int // the main port; POST /api/scan is guarded with it
	job         *scanJob
	sec         model.Security // nil: security off
	ui          *ui.Handler    // the interface, with security on
	catalog     *rest.Catalog  // the search, with security on
	hostNames   []string       // Options.HostNames
}

// New opens the index and attaches the stores of cfg. dockerPorts maps a
// listening port to the Docker repository it serves; a request that comes
// in on such a port is routed to that repository's registry path.
func New(cfg *config.Config, dockerPorts map[int]string, opts Options) (*Server, error) {
	rp, err := newReadPath(cfg, dockerPorts)
	if err != nil {
		return nil, err
	}
	ports := map[int]string{}
	for port, repo := range dockerPorts {
		ports[port] = repo
	}
	token := make([]byte, 16)
	rand.Read(token)
	takeOver := func() error {
		_, err := rp.sync()
		return err
	}
	s := &Server{rp: rp, dockerPorts: ports, access: opts.AccessLog, hostNames: slices.Clone(opts.HostNames), marker: "/" + hex.EncodeToString(token) + "/",
		escape: hex.EncodeToString(token) + "~", trusted: newTrustedHosts(forwardedAllowIPs()), port: cfg.Port,
		job: newScanJob(rp.index, takeOver)}
	if opts.Security != nil {
		if s.ui, err = ui.New(); err == nil {
			s.catalog, err = rest.OpenCatalog(cfg.IndexPath())
		}
		if err != nil {
			_ = rp.index.Close() // the error of the set up is the one returned
			return nil, err
		}
		s.sec = opts.Security
	}
	return s, nil
}

// Close stops a running scan and closes the index, as the lifespan of the
// Python application does. A scan that does not stop within
// scanStopTimeout keeps the index open until the process exits.
func (s *Server) Close() error {
	if !s.job.stop(scanStopTimeout) {
		logAt(slogWarn, serverLogger, "a scan did not stop within %s s; the index is left to the process exit",
			strconv.FormatFloat(scanStopTimeout.Seconds(), 'f', 0, 64))
		return nil
	}
	if s.catalog != nil {
		_ = s.catalog.Close() // read only connections; the index close below is the one that counts
	}
	return s.rp.index.Close()
}

func sortedPorts(m map[int]string) []int {
	ports := make([]int, 0, len(m))
	for port := range m {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data) // #nosec G401 -- the ETag of a generated answer, as the Python server writes it.
	return hex.EncodeToString(sum[:])
}

// request is one request as the Python application sees it.
type request struct {
	r          *http.Request
	raw        string // raw path, in its routed form on a Docker port
	path       string // the decoded path the router matches
	query      string // raw query string
	scheme     string // "http", or the X-Forwarded-Proto of a trusted client
	dockerRepo string // the request came in on this repository's Docker port
	head       bool
	parsed     queryValues
	parsedOK   bool
	// the caller with security on, resolved once by subjectOf
	subject    *model.Subject
	subjectErr error
	resolved   bool
}

func (req *request) params() queryValues {
	if !req.parsedOK {
		req.parsed, req.parsedOK = parseQuery(req.query), true
	}
	return req.parsed
}

// visibleASCII is the request target uvicorn's parser accepts.
func visibleASCII(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// localHost is _local_host: a Host header of localhost, 127.0.0.1 or
// [::1], with any port or none.
func localHost(value string) bool {
	m := hostHeader.FindStringSubmatch(latin1Strip(value))
	return m != nil && slices.Contains(localHostNames[:], asciiLower(m[1]))
}

// namedHost is a Host header of one of names, with any port or none.
func namedHost(value string, names []string) bool {
	m := hostHeader.FindStringSubmatch(latin1Strip(value))
	return m != nil && slices.Contains(names, asciiLower(m[1]))
}

func localPort(r *http.Request) int {
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if tcp, ok := addr.(*net.TCPAddr); ok {
			return tcp.Port
		}
	}
	return -1
}

// ServeHTTP answers one request: host guard first, then the routing of the
// Docker ports, then the routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength != 0 {
		// net/http reads a body the handler leaves before it sends the
		// answer, with no time limit. The answer goes first, as in uvicorn,
		// and the body then gets the keep alive time to arrive.
		rc := http.NewResponseController(w)
		rc.EnableFullDuplex()
		defer func() { rc.SetReadDeadline(time.Now().Add(keepAlive)) }()
	}
	if !r.ProtoAtLeast(1, 1) {
		// uvicorn answers HTTP/1.0 as HTTP/1.1 and closes the connection
		w.Header().Set("Connection", "close")
		if g, ok := r.Context().Value(gateKey{}).(*gateConn); ok {
			g.answerAs11()
		}
	}
	uri := r.RequestURI
	if rest, ok := strings.CutPrefix(uri, s.marker); ok {
		uri = rest
	}
	if !visibleASCII(uri) {
		s.invalidRequest(w)
		return
	}
	unescapeHeaders(r, s.escape)
	scheme, client := "http", r.RemoteAddr
	if host, port, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		scheme, client = s.trusted.forwarded(host, port, r.Header)
	}
	rawPath, query, _ := strings.Cut(uri, "?")
	rec := &recorder{ResponseWriter: w, s: s, r: r, path: accessPath(rawPath, query), client: client}
	req := &request{r: r, raw: rawPath, path: routePath(rawPath), query: query, scheme: scheme, head: r.Method == http.MethodHead}
	if !localHost(r.Host) && !namedHost(r.Host, s.hostNames) {
		writeText(rec, req, http.StatusForbidden, forbidden)
		return
	}
	if repo, ok := s.dockerPorts[localPort(r)]; ok {
		if req.path != "/v2" && !strings.HasPrefix(req.path, "/v2/") {
			dockerError(rec, req, http.StatusNotFound, "UNSUPPORTED", "this port answers the registry API under /v2/ only", nil)
			return
		}
		req.path = repositoryPrefix + repo + req.path
		req.raw = repositoryPrefix + quote(repo, "") + rawPath
		req.dockerRepo = repo
	}
	if err := s.route(rec, req); err != nil {
		s.fail(rec, req, err)
	}
}

// invalidRequest is uvicorn's answer to a request its parser refuses.
func (s *Server) invalidRequest(w http.ResponseWriter) {
	s.writeLog("WARNING:  Invalid HTTP request received.\n")
	h := w.Header()
	h.Set("Content-Type", textType)
	h.Set("Connection", "close")
	h.Set("Content-Length", "30")
	w.WriteHeader(http.StatusBadRequest)
	io.WriteString(w, "Invalid HTTP request received.")
}

// fail is the answer of Starlette to an exception of the application.
func (s *Server) fail(w *recorder, req *request, err error) {
	logAt(slog.LevelError, serverLogger, "Exception in ASGI application: %s: %s", PyClass(err), PyText(err))
	if w.wrote {
		return
	}
	writeBody(w, req, http.StatusInternalServerError, [][2]string{{"Content-Type", textType}}, []byte("Internal Server Error"))
}

type route struct {
	pattern *regexp.Regexp
	methods []string
	rule    rule // applied with security on only
	handle  func(s *Server, w *recorder, req *request, match []string) error
}

// The routes of the Python application, in their order. A pattern matches
// the decoded path as Starlette matches it: "$" also before a final
// newline, "." not across one. FastAPI adds no HEAD to a GET route.
// With security on each route has the rule of T6 and T7 of the thin slice
// plan; the client path needs the view permission of the repository it
// names, and the scan needs every right (T8).
var routes = []route{
	{regexp.MustCompile(`^/\n?$`), []string{"GET"}, public, (*Server).page},
	{regexp.MustCompile(`^/api/health\n?$`), []string{"GET"}, public, (*Server).health},
	{regexp.MustCompile(`^/api/status\n?$`), []string{"GET"}, public, (*Server).apiStatus},
	{regexp.MustCompile(`^/api/repositories\n?$`), []string{"GET"}, signedIn, (*Server).apiRepositories},
	{regexp.MustCompile(`^/api/browse/([^/]+)\n?$`), []string{"GET"}, view(1, "browse"), (*Server).apiBrowse},
	{regexp.MustCompile(`^/api/asset/([^/]+)\n?$`), []string{"GET"}, view(1, "browse"), (*Server).asset},
	{regexp.MustCompile(`^/api/scan\n?$`), []string{"POST"}, perm("nexus:*"), (*Server).apiScan},
	{regexp.MustCompile(`^/service/rest/v1/status\n?$`), []string{"GET"}, public, (*Server).status},
	{regexp.MustCompile(`^/service/rest/v1/status/writable\n?$`), []string{"GET"}, public, (*Server).writable},
	{regexp.MustCompile(`^/repository/(.*)\n?$`), []string{"GET", "HEAD"}, client, (*Server).repository},
	{regexp.MustCompile(`^/repository/(.*)\n?$`), []string{"PUT", "POST", "DELETE", "PATCH"}, client, (*Server).refuseWrite},
	{regexp.MustCompile(`^/service/rest/repository/browse/(.*)\n?$`), []string{"GET", "HEAD"}, client, (*Server).browse},
}

func (s *Server) route(w *recorder, req *request) error {
	table := s.table()
	if s.ui != nil && s.ui.Has(req.path) {
		s.ui.Serve(w, req.r, req.path)
		return nil
	}
	var partial *route
	for i := range table {
		rt := &table[i]
		match := rt.pattern.FindStringSubmatch(req.path)
		if match == nil {
			continue
		}
		if slices.Contains(rt.methods, req.r.Method) {
			if s.sec != nil {
				if allowed, err := s.allow(w, req, rt.rule, match); err != nil || !allowed {
					return err
				}
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
	if req.path != "/" {
		toggled := req.path + "/"
		if strings.HasSuffix(req.path, "/") {
			toggled = strings.TrimRight(req.path, "/")
		}
		for i := range table {
			if table[i].pattern.MatchString(toggled) {
				s.redirectSlashes(w, req, toggled)
				return nil
			}
		}
	}
	unknownRoute(w, req)
	return nil
}

// redirectSlashes is the 307 of Starlette's router to the path with or
// without its trailing slash.
func (s *Server) redirectSlashes(w *recorder, req *request, path string) {
	netloc := req.r.Host
	if !validPort(netloc) {
		if addr, ok := req.r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr); ok {
			netloc = addr.String()
			if defaultPorts[req.scheme] == addr.Port {
				netloc = strings.TrimSuffix(netloc, ":"+strconv.Itoa(addr.Port))
			}
		}
	}
	path = strings.NewReplacer("?", "%3F", "#", "%23", `\`, "%5C").Replace(path)
	url := req.scheme + "://" + netloc + path
	if req.query != "" {
		url += "?" + req.query
	}
	writeBody(w, req, http.StatusTemporaryRedirect, [][2]string{{"Location", quote(url, ":/%#?=@[]!$&'()*+,;")}}, nil)
}

// defaultPorts are the ports Starlette's URL leaves out of a netloc it
// builds from the server address.
var defaultPorts = map[string]int{"http": 80, "https": 443, "ws": 80, "wss": 443}

func validPort(host string) bool {
	// _HOST_RE of Starlette takes the bytes of a host name only; the host
	// guard lets white space of str.strip through
	for i := 0; i < len(host); i++ {
		if !hostByte(host[i]) && host[i] != '%' {
			return false
		}
	}
	port := ""
	if strings.HasPrefix(host, "[") {
		end := strings.Index(host, "]")
		if end < 0 {
			return false
		}
		rest := host[end+1:]
		if rest == "" {
			return true
		}
		if !strings.HasPrefix(rest, ":") {
			return false
		}
		port = rest[1:]
	} else {
		i := strings.LastIndex(host, ":")
		if i < 0 {
			return true
		}
		port = host[i+1:]
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 0 && n <= 65535
}

// unknownRoute is FastAPI's answer to a path no route serves.
func unknownRoute(w http.ResponseWriter, req *request) {
	writeBody(w, req, http.StatusNotFound, [][2]string{{"Content-Type", jsonType}}, []byte(`{"detail":"Not Found"}`))
}

// writeBody writes an answer whose body is known; HEAD gets the headers
// only, Content-Length included.
func writeBody(w http.ResponseWriter, req *request, status int, headers [][2]string, data []byte) {
	h := w.Header()
	for _, kv := range headers {
		h.Set(kv[0], kv[1])
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if !req.head && len(data) > 0 {
		w.Write(data)
	}
}

// writeText is _text: a line of text.
func writeText(w http.ResponseWriter, req *request, status int, message string) {
	writeBody(w, req, status, [][2]string{{"Content-Type", textType}, {"X-Content-Type-Options", "nosniff"}}, []byte(message+"\n"))
}

func writeJSON(w http.ResponseWriter, req *request, status int, value any) error {
	data, err := pyjson.ResponseBody(value)
	if err != nil {
		return err
	}
	writeBody(w, req, status, [][2]string{{"Content-Type", jsonType}}, data)
	return nil
}

func jsonError(w http.ResponseWriter, req *request, status int, message string) error {
	return writeJSON(w, req, status, pyjson.NewObject().Set("error", message))
}

// redirect is _redirect: 302 to the directory URL of a path.
func redirect(w http.ResponseWriter, req *request, prefix, repo, path string) {
	writeBody(w, req, http.StatusFound, [][2]string{{"Location", repoURL(prefix, repo, path)}}, nil)
}

func (s *Server) health(w *recorder, req *request, _ []string) error {
	return writeJSON(w, req, http.StatusOK, pyjson.NewObject().Set("status", "ok"))
}

func (s *Server) status(w *recorder, req *request, _ []string) error {
	writeBody(w, req, http.StatusOK, nil, nil)
	return nil
}

func (s *Server) writable(w *recorder, req *request, _ []string) error {
	writeBody(w, req, http.StatusServiceUnavailable, nil, nil)
	return nil
}

func filterAccess(rs *repos.Repositories, name string, kinds ...string) []repos.Repo {
	var out []repos.Repo
	for _, m := range rs.Resolve(name) {
		if slices.Contains(kinds, repos.AccessOf(m)) {
			out = append(out, m)
		}
	}
	return out
}

// repository is the route /repository/<name>/<path>.
func (s *Server) repository(w *recorder, req *request, _ []string) error {
	return s.handle(w, req, repositoryPrefix, true)
}

// browse is the route /service/rest/repository/browse/<name>/<path>: the
// directories of a repository, no files.
func (s *Server) browse(w *recorder, req *request, _ []string) error {
	return s.handle(w, req, browsePrefix, false)
}

// handle is ReadPath.handle: files says whether the route answers files,
// the npm read path and the registry API besides the directories.
func (s *Server) handle(w *recorder, req *request, prefix string, files bool) error {
	t, err := parseTarget(req.raw, prefix)
	if err != nil {
		writeText(w, req, http.StatusBadRequest, "Bad request path: "+err.Error())
		return nil
	}
	if t == nil {
		writeText(w, req, http.StatusNotFound, "Not found")
		return nil
	}
	st, err := s.rp.sync()
	if err != nil {
		return err
	}
	repo, ok := st.repositories.Get(t.repo)
	if req.dockerRepo != "" && (!ok || repos.AccessOf(repo) != "docker") {
		dockerError(w, req, http.StatusNotFound, "NAME_UNKNOWN",
			fmt.Sprintf("repository %s is not a Docker repository of this server", t.repo), nil)
		return nil
	}
	if !ok {
		writeText(w, req, http.StatusNotFound, fmt.Sprintf("Repository %s not found", t.repo))
		return nil
	}
	access := repos.AccessOf(repo)
	if access == "docker" {
		if files && (t.path == "v2" || strings.HasPrefix(t.path, "v2/")) {
			err := s.docker(w, req, st, repo, dockerMembers(st.repositories, repo.Name), t)
			var un *unavailable
			if errors.As(err, &un) {
				logAt(slogWarn, serverLogger, "%s", un.message)
				dockerError(w, req, http.StatusServiceUnavailable, "UNAVAILABLE", un.message, nil)
				return nil
			}
			return err
		}
		writeText(w, req, http.StatusNotFound, fmt.Sprintf("Repository %s has format docker; it answers the registry API under %s%s/v2/ and on its Docker port",
			repo.Name, repositoryPrefix, repo.Name))
		return nil
	}
	if !slices.Contains(fileAccess[:], access) && access != "npm" {
		writeText(w, req, http.StatusNotFound, fmt.Sprintf("Repository %s has format %s, which this server does not serve yet", repo.Name, repo.Format))
		return nil
	}
	kinds := fileAccess[:]
	if access == "npm" {
		kinds = []string{"npm"}
	}
	err = s.files(w, req, st, repo, filterAccess(st.repositories, repo.Name, kinds...), t, access, prefix, files)
	var un *unavailable
	if errors.As(err, &un) {
		logAt(slogWarn, serverLogger, "%s", un.message)
		writeText(w, req, http.StatusServiceUnavailable, un.message)
		return nil
	}
	return err
}

func (s *Server) files(w *recorder, req *request, st *state, repo repos.Repo, members []repos.Repo, t *target, access, prefix string,
	files bool) error {
	if t.bare {
		redirect(w, req, prefix, repo.Name, "")
		return nil
	}
	if access == "npm" && files {
		answered, err := s.npm(w, req, st, repo, members, t)
		if err != nil || answered {
			return err
		}
	}
	if t.directory {
		return s.listing(w, req, repo, members, t.path)
	}
	if files {
		b, err := s.rp.file(st, repo, members, t.path)
		if err != nil {
			return err
		}
		if b != nil {
			return send(w, req, b, nil)
		}
	}
	isDir, err := s.rp.anyIsDir(members, t.path)
	if err != nil {
		return err
	}
	if isDir {
		redirect(w, req, prefix, repo.Name, t.path)
		return nil
	}
	writeText(w, req, http.StatusNotFound, "Not found")
	return nil
}

// refuseWrite is ReadPath.refuse_write: 405 for every write; under v2/ of a
// Docker repository the registry error UNSUPPORTED, under "-" of an npm
// repository the npm 404, which holds for every method.
func (s *Server) refuseWrite(w *recorder, req *request, _ []string) error {
	isDocker := req.dockerRepo != ""
	if !isDocker {
		t, err := parseTarget(req.raw, repositoryPrefix)
		if err != nil {
			t = nil
		}
		if t != nil {
			st, err := s.rp.sync()
			if err != nil {
				return err
			}
			access := ""
			if repo, ok := st.repositories.Get(t.repo); ok {
				access = repos.AccessOf(repo)
			}
			switch {
			case access == "docker" && (t.path == "v2" || strings.HasPrefix(t.path, "v2/")):
				isDocker = true
			case access == "npm" && npmDash(t):
				npmNotFound(w, req)
				return nil
			}
		}
	}
	if isDocker {
		dockerError(w, req, http.StatusMethodNotAllowed, "UNSUPPORTED", "this registry is read only: push and delete are refused",
			[][2]string{{"Allow", allow}})
		return nil
	}
	writeBody(w, req, http.StatusMethodNotAllowed, [][2]string{{"Content-Type", textType}, {"Allow", allow}},
		[]byte("Method not allowed: this server is read only\n"))
	return nil
}
