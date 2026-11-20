package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/proxy"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/routing"
)

// The read path of a proxy repository of the formats raw and maven2 (leg 5b
// of phase 5). A routing rule decides first. With upstream off (decision 5.1
// of the phase 5 plan) a proxy answers as the read path of the port: the
// cache, and 404 for a miss. With upstream on internal/proxy decides whether
// to ask the remote; what it fetches goes through ingest into the write
// store, and the answer comes from the stored file, as for a hosted file
// (decision 5.7).

// openUpstream is the proxies of a server, nil while upstream is off.
func openUpstream(cfg *config.Config, opts Options) *proxy.Proxies {
	if !opts.Upstream {
		return nil
	}
	c := opts.upstreamConfig
	c.DataDir, c.State = cfg.DataDir, opts.ProxyState
	return proxy.New(c)
}

// routingSource is where a running server reads the routing rules: the
// identity store of a server with security (internal/cli), whose
// generation rises with every write of the rules (ruling SY3 of leg
// 6-system).
type routingSource interface {
	Generation() (int64, error)
	Routing() (model.Routing, error)
}

// routingSourceOf is the identity store of opts as a routing source, nil
// without security or for a store that is none.
func routingSourceOf(opts Options) routingSource {
	if opts.Security == nil {
		return nil
	}
	src, _ := opts.Identity.(routingSource)
	return src
}

// routingNow is the routing of one generation: the compiled rules and the
// rule each repository names.
type routingNow struct {
	generation int64
	rules      *routing.Rules // nil allows every path
	links      map[string]string
}

// liveRouting is the routing of a running server. With a source it reads
// the generation at each call of now and compiles the rules again when it
// changed, so a saved rule decides the next request. While the generation
// or the rules cannot be read, now gives errRoutingUnreadable and a request
// that a rule could decide is refused (ruling SY9); the failure is logged
// when it starts and when it ends (SY10). Without a source the rules of
// the start stay. It is safe for use by several goroutines; a nil
// *liveRouting allows every path.
type liveRouting struct {
	source  routingSource
	mu      sync.Mutex
	cur     atomic.Pointer[routingNow]
	failing atomic.Bool
}

// errRoutingUnreadable is the refusal of a request a routing rule could
// decide while the rules cannot be read (SY9); it names no path.
var errRoutingUnreadable = errors.New("the routing rules cannot be read; a request that a routing rule decides is refused until they can")

// newLiveRouting starts with the rules of start, nil for none. With a
// source the first call of now reads them from it.
func newLiveRouting(start *model.Routing, source routingSource) *liveRouting {
	l := &liveRouting{source: source}
	now := &routingNow{generation: -1, links: linksOf(start)}
	if start != nil {
		now.rules = routing.Compile(*start)
	}
	l.cur.Store(now)
	return l
}

// now is the routing of the current generation, or the routing it had
// and errRoutingUnreadable while the generation or the rules of that
// generation cannot be read.
func (l *liveRouting) now() (*routingNow, error) {
	if l == nil {
		return nil, nil
	}
	cur := l.cur.Load()
	if l.source == nil {
		return cur, nil
	}
	gen, err := l.source.Generation()
	if err != nil {
		return cur, l.failed()
	}
	if gen == cur.generation {
		l.recovered()
		return cur, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur = l.cur.Load(); cur.generation == gen {
		l.recovered()
		return cur, nil
	}
	r, err := l.source.Routing()
	if err != nil {
		return cur, l.failed()
	}
	next := &routingNow{generation: gen, rules: routing.Compile(r), links: linksOf(&r)}
	known := cur.rules.Problems()
	for _, problem := range next.rules.Problems() {
		if !slices.Contains(known, problem) {
			logAt(slogWarn, serverLogger, "%s", problem)
		}
	}
	l.cur.Store(next)
	l.recovered()
	return next, nil
}

// failed logs the start of a read failure once and gives the refusal. The
// line names no path, so the error of the read stays out of it (SY10).
func (l *liveRouting) failed() error {
	if l.failing.CompareAndSwap(false, true) {
		logAt(slogWarn, serverLogger, "the routing rules cannot be read; proxy requests answer 503 until they can")
	}
	return errRoutingUnreadable
}

// recovered logs the end of a read failure once.
func (l *liveRouting) recovered() {
	if l.failing.CompareAndSwap(true, false) {
		logAt(slogInfo, serverLogger, "the routing rules can be read again")
	}
}

// Allowed is routing.Rules.Allowed of the current generation, and
// errRoutingUnreadable while it cannot be read (SY9).
func (l *liveRouting) Allowed(repository, path string) (bool, error) {
	n, err := l.now()
	if err != nil {
		return false, err
	}
	if n != nil {
		return n.rules.Allowed(repository, path), nil
	}
	return true, nil
}

// Problems are those of the current generation, or of the last one read.
func (l *liveRouting) Problems() []string {
	if n, _ := l.now(); n != nil {
		return n.rules.Problems()
	}
	return nil
}

// Link is the rule the repository names in the current generation, empty
// for none; errRoutingUnreadable while it cannot be read.
func (l *liveRouting) Link(repository string) (string, error) {
	n, err := l.now()
	if err != nil || n == nil {
		return "", err
	}
	return n.links[repository], nil
}

// links is the routing of the links that the lists of repositories show:
// the last one read, also while the rules cannot be read, because no
// routing rule decides a read of the REST API (SY9).
func (l *liveRouting) links() *routingNow {
	n, _ := l.now()
	return n
}

// routingRefusal is the text of the 503 of SY9.
var routingRefusal = errRoutingUnreadable.Error()

// linksOf is the routing rule each repository names, nil for none.
func linksOf(r *model.Routing) map[string]string {
	if r == nil || len(r.Links) == 0 {
		return nil
	}
	return r.Links
}

// logUpstream is what the start log says of the proxies: one line that
// names upstream while it is on, after the line of writes (ruling P5-6 of
// the phase 5 session), each problem of the routing rules (ruling P5-4), and
// with upstream on each proxy whose settings are refused, with repository
// and key. With upstream off and no routing rule the log keeps the lines of
// the port.
func (s *Server) logUpstream() {
	if s.proxies != nil {
		logAt(slogInfo, serverLogger, "upstream on: proxies ask their remotes for what they do not hold and store it in store %s at %s",
			s.writes.writer.Name(), s.writes.writer.Root())
	}
	for _, problem := range s.routes.Problems() {
		logAt(slogWarn, serverLogger, "%s", problem)
	}
	if s.proxies == nil {
		return
	}
	st, err := s.rp.sync()
	if err != nil {
		logAt(slogWarn, serverLogger, "the proxies are not checked: %v", err)
		return
	}
	for _, repo := range st.repositories.All() {
		if repo.Type == "proxy" && proxy.Fetches(repo.Format) {
			_ = s.proxies.CheckSettings(repo) // a refusal is logged by the proxies
		}
	}
}

// routedPath is the path a routing rule is matched against: the request
// path below the repository with its leading slash, the trailing slash of a
// folder kept, the query left out (ruling P5-9).
func routedPath(t *target) string {
	if t.directory && t.path != "" {
		return "/" + t.path + "/"
	}
	return "/" + t.path
}

// routedMembers are the members a read may use under the routing rules
// (decision 5.15, ruling P5-4): a proxy member whose rule blocks the path is
// left out, its cache included. blocked is true when the repository of the
// request is itself such a proxy, which answers 403.
//
// err is errRoutingUnreadable when a rule of the repository or of a proxy
// member could decide and the rules cannot be read (SY9).
func (s *Server) routedMembers(repo repos.Repo, members []repos.Repo, path string) (kept []repos.Repo, blocked bool, err error) {
	if repo.Type == "proxy" {
		allowed, err := s.routes.Allowed(repo.Name, path)
		if err != nil || !allowed {
			return nil, err == nil, err
		}
	}
	for _, m := range members {
		if m.Type != "proxy" {
			kept = append(kept, m)
			continue
		}
		allowed, err := s.routes.Allowed(m.Name, path)
		if err != nil {
			return nil, false, err
		}
		if allowed {
			kept = append(kept, m)
		}
	}
	return kept, false, nil
}

// proxyFile answers a request below a proxy whose format fetches
// (proxy.Fetches). A path the routing rule of the proxy blocks answers 403
// before the cache is looked at (decision 5.15, ruling P5-4). A folder is the
// listing of the cache and asks nothing. A file is the cached copy, or what
// the proxy answers for it; the answer of a fetched file is the one of the
// stored file.
func (s *Server) proxyFile(w *recorder, req *request, st *state, repo repos.Repo, members []repos.Repo, t *target, prefix string) error {
	allowed, err := s.routes.Allowed(repo.Name, routedPath(t))
	if err != nil {
		writeText(w, req, http.StatusServiceUnavailable, routingRefusal)
		return nil
	}
	if !allowed {
		writeText(w, req, http.StatusForbidden, fmt.Sprintf("Forbidden: a routing rule of repository %s blocks this path", repo.Name))
		return nil
	}
	if t.directory {
		return s.listing(w, req, repo, members, t.path)
	}
	b, err := s.rp.file(st, repo, members, t.path)
	if err != nil {
		return err
	}
	if b == nil {
		isDir, err := s.rp.anyIsDir(members, t.path)
		if err != nil {
			return err
		}
		if isDir {
			redirect(w, req, prefix, repo.Name, t.path)
			return nil
		}
	}
	outcome := proxy.Missing
	if b != nil {
		outcome = proxy.Cache
	}
	if s.proxies != nil {
		var left bool
		if b, outcome, left, err = s.proxyCopy(w, req, st, repo, t.path, b); err != nil || left {
			return err // left: the client left; the fetch goes on without it
		}
	}
	if b != nil {
		return send(w, req, b, nil)
	}
	if outcome == proxy.Failed {
		writeText(w, req, http.StatusBadGateway, fmt.Sprintf("Bad gateway: the remote of repository %s failed", repo.Name))
		return nil
	}
	writeText(w, req, http.StatusNotFound, "Not found")
	return nil
}

// proxyCopy is the copy of a path that a proxy that fetches answers with: b
// is the copy its cache held when the request came, nil for none; a fresh
// one stays, a stale one is checked at the remote, a miss is fetched, and
// what the fetch stored is opened again (decision 5.7). outcome is the one
// of the proxies. left is true when the client left; b is closed then, and
// the fetch goes on without it.
func (s *Server) proxyCopy(w *recorder, req *request, st *state, member repos.Repo, path string, b *body) (*body, proxy.Outcome, bool, error) {
	q := proxy.Query{Repo: member, Path: path, Limit: member.WriteSettings().MaxBodyBytes, Look: s.lookFor(member.Name, path),
		Store: s.storeFor(member, path, req.subject, peerAddress(w))}
	if b != nil {
		q.Cached = &proxy.Cached{CreatedMS: createdOf(b)}
	}
	outcome, err := s.proxies.Get(req.r.Context(), q)
	if err != nil {
		if b != nil {
			b.close()
		}
		return nil, outcome, true, nil
	}
	if outcome == proxy.Stored || outcome == proxy.Cache && b == nil {
		if b != nil {
			b.close()
		}
		if b, err = s.rp.file(st, member, []repos.Repo{member}, path); err != nil {
			return nil, outcome, false, err
		}
	}
	return b, outcome, false, nil
}

// lookFor is the look of a fetch into the cache: the winning row of the
// path.
func (s *Server) lookFor(repo, path string) func() (*proxy.Cached, error) {
	return func() (*proxy.Cached, error) {
		asset, err := s.rp.index.Lookup(repo, path)
		if err != nil || asset == nil {
			return nil, err
		}
		return &proxy.Cached{CreatedMS: asset.CreatedMS}, nil
	}
}

// storeFor is the store of a fetch: the whole answer of the remote through
// ingest (seam Ingest, ruling P5-3), as a fetch, with the user and the
// address of the request that caused it (decision 5.20). Body limit, free
// space floor, the strict content check, the length the remote declares and
// the digests the bytes must have (decision 5.10) apply, and the floor holds
// again after every fetchFloorStep bytes of the body (floorReader); a
// refused or broken body leaves nothing. The error of a refusal names its
// rule, for the log line of the fetch.
func (s *Server) storeFor(repo repos.Repo, path string, subject *model.Subject, address string) func(proxy.Fetched) error {
	return s.storeTyped(repo, path, "", subject, address)
}

// storeTyped is storeFor with the content type the blob keeps; empty takes
// the one policy.ContentType gives the path and its first bytes.
func (s *Server) storeTyped(repo repos.Repo, path, contentType string, subject *model.Subject, address string) func(proxy.Fetched) error {
	return func(f proxy.Fetched) error {
		free := func() (uint64, error) { return s.writes.freeSpace(s.writes.writer.Root()) }
		body := newFloorReader(f.Body, free, repo.WriteSettings().FreeSpaceFloor, repo.Name, s.writes.writer.Name())
		_, err := s.ingest(ingestRequest{repo: repo, path: path, body: body, size: f.Size, digests: f.Digests, contentType: contentType,
			fetch: true, subject: subject, address: address})
		var refusal *writeRefusal
		if errors.As(err, &refusal) {
			return fmt.Errorf("rule %s: %w", refusal.rule, err)
		}
		return err
	}
}

// createdOf is the creation time of the blob of a file answer.
func createdOf(b *body) int64 {
	if b.modifiedMS == nil {
		return 0
	}
	return *b.modifiedMS
}

// fetchFloorStep is how much of the body of a fetch passes between two looks
// at the free space of the write store (fix part A of leg 5b). Up to 64
// fetches stream at once, and each one measured the room above the floor at
// its start; a fetch that finds the free space below the floor at a look ends
// its body there, so the fetches in flight write at most fetchFloorStep each
// beyond the floor.
const fetchFloorStep = 8 << 20

// floorReader is the body of a fetch that looks at the free space of the
// write store after every fetchFloorStep bytes and ends with the refusal
// no-space once it is below the floor of the repository. It never reads past
// the next look.
type floorReader struct {
	body        io.Reader
	free        func() (uint64, error)
	floor       int64
	repo, store string
	since       int64 // bytes since the last look
	err         error
}

func newFloorReader(body io.Reader, free func() (uint64, error), floor int64, repo, store string) io.Reader {
	return &floorReader{body: body, free: free, floor: max(floor, 0), repo: repo, store: store}
}

func (r *floorReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if left := fetchFloorStep - r.since; int64(len(p)) > left {
		p = p[:left]
	}
	n, err := r.body.Read(p)
	if r.since += int64(n); r.since < fetchFloorStep {
		return n, err
	}
	r.since = 0
	free, freeErr := r.free()
	switch {
	case freeErr != nil:
		r.err = freeErr
	case free < uint64(r.floor): // #nosec G115 -- the floor is at least zero.
		r.err = writeRefused(http.StatusInsufficientStorage, ruleNoSpace,
			"store %s has %d bytes free, below the %d bytes repository %s must leave; the fetch stops", r.store, free, r.floor, r.repo)
	default:
		return n, err
	}
	return n, r.err
}
