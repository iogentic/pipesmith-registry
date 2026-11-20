package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// The limits of fetches at once (decision 5.8 of the phase 5 plan).
const (
	DefaultPerRepository = 16
	DefaultPerProcess    = 64
)

// StateStore is the table proxy_state of catalog.sqlite (section State of
// the phase 5 plan); *catalog.Catalog is one.
type StateStore interface {
	ProxyState(repo, path string) (catalog.ProxyState, error)
	PutProxyRow(catalog.ProxyRow) error
	ResetProxyState(repo string, atMS int64) error
}

// Config configures the proxies of one server process.
type Config struct {
	// DataDir is the data directory of the server.
	DataDir string
	// State is the table proxy_state; nil keeps no row.
	State StateStore
	// Now is the clock of the proxies; nil is time.Now.
	Now func() time.Time
	// Logger takes the lines of the proxies; nil is slog.Default() at the
	// time of the line.
	Logger *slog.Logger
	// Version names this server in the user agent; empty is version.Version.
	Version string
	// TotalTimeout bounds one request to a remote with the reading of its
	// body; 0 is DefaultTotalTimeout.
	TotalTimeout time.Duration
	// PerRepository and PerProcess limit the fetches at once; 0 is
	// DefaultPerRepository and DefaultPerProcess.
	PerRepository, PerProcess int

	dialer   contextDialer // the tests put a dialer that never connects here
	classify classifier    // the tests call a remote on loopback public here (ruling P5-8)
}

// Proxies are the proxy repositories of one server process: the client of
// each remote, the fetches in flight and what the read path asks of them.
// They are safe for use by several goroutines.
type Proxies struct {
	o       Config
	slots   chan struct{} // the fetches of the process at once
	flights flights

	mu       sync.Mutex
	remotes  map[string]*remote  // by repository
	refused  map[string]string   // the last settings error logged per repository
	tagLists map[tagKey]*tagList // the tag lists of the Docker proxies (registry.go)
	tagBytes int                 // the size of tagLists, at most maxTagListTotal
}

// remote is one proxy repository: its client, the fetches of it at once,
// what it needs for its sign in and its auto block state. The client is made
// again when the settings or the secret file change; the slots and the auto
// block state stay.
type remote struct {
	slots     chan struct{}
	format    string
	settings  repos.ProxySettings
	limit     int64
	secretKey fileKey // the secret file the client was made with
	client    *Client
	// guarded by Proxies.mu
	need    string // why the client cannot sign in; empty when it can or needs no sign in
	refused bool   // the remote answered 401 to the credentials of secretKey
	block   autoBlock
}

// New returns the proxies of a server.
func New(o Config) *Proxies {
	if o.PerRepository <= 0 {
		o.PerRepository = DefaultPerRepository
	}
	if o.PerProcess <= 0 {
		o.PerProcess = DefaultPerProcess
	}
	p := &Proxies{o: o, slots: make(chan struct{}, o.PerProcess), remotes: map[string]*remote{}, refused: map[string]string{},
		tagLists: map[tagKey]*tagList{}}
	p.flights.stopped = func(key flightKey, v any) {
		// the path a client chose and the text of the panic, quoted
		p.warn("proxy %q: %q: the fetch stopped on a panic and answers 502: %q", key.repo, key.path, fmt.Sprint(v))
	}
	return p
}

// Close closes the idle connections of every client.
func (p *Proxies) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.remotes {
		if r.client != nil {
			r.client.Close()
		}
	}
}

// Query is one request of the read path for one path of a proxy.
type Query struct {
	// Repo is the proxy, with its attributes.
	Repo repos.Repo
	// Path is the decoded path below the repository, without a leading
	// slash: the key of the cache and of proxy_state.
	Path string
	// Cached is the copy the read path found, nil for a miss.
	Cached *Cached
	// Remote is the escaped path below the remote address that the fetch
	// asks for; empty is Path escaped segment by segment (remotePath).
	Remote string
	// Header holds request headers of the client that the fetch sends on,
	// as the Accept of a Docker client; the client sets User-Agent and
	// Authorization itself.
	Header http.Header
	// Digests are the digests by algorithm, in hex, that the bytes of a 200
	// must have, known before the request (decision 5.10). The store
	// compares them and keeps nothing of bytes that differ.
	Digests map[string]string
	// Limit is the largest body the store of the repository takes; 0 is
	// repos.DefaultMaxBodyBytes.
	Limit int64
	// Look is the copy the cache holds now, nil for none. The fetch asks it
	// before it sends a request, so that a query that waited while another
	// fetch of the path stored it asks nothing. Nil looks into nothing.
	Look func() (*Cached, error)
	// Store writes a 200 of the remote into the cache. It runs in the fetch
	// of the first query of a path, at most once per fetch, and goes on when
	// that query left (decision 5.8).
	Store func(Fetched) error
}

// Cached is the copy of a path the cache holds.
type Cached struct {
	CreatedMS int64 // the creation time of its blob
}

// Fetched is the answer of the remote that Store writes.
type Fetched struct {
	Body        io.Reader         // read once, to its end; a read fails when the remote breaks off
	Size        int64             // the declared length, -1 for none
	ContentType string            // the Content-Type of the remote, empty for none
	Header      http.Header       // the headers of the answer
	Digests     map[string]string // the digests the bytes must have, by algorithm in hex; nil for none
}

// shared reports a path the cache keeps for every name the remote knows it
// under: a Docker blob is asked for below the image name of the request and
// kept under DockerBlobPrefix. A 404 of the remote for one name says
// nothing of another, so such a path gets no not found mark, and a fetch
// of it is one flight per remote path (finding 5-1 of review 5).
func (q Query) shared() bool {
	return q.Repo.Format == "docker" && strings.HasPrefix(q.Path, DockerBlobPrefix)
}

// flight is the key of the single flight of a query.
func (q Query) flight() flightKey {
	if q.shared() {
		return flightKey{q.Repo.Name, q.Path + "\x00" + q.remote()}
	}
	return flightKey{q.Repo.Name, q.Path}
}

// remote is the escaped path a query asks the remote for.
func (q Query) remote() string {
	if q.Remote != "" {
		return q.Remote
	}
	return remotePath(q.Path)
}

// Outcome is what the read path answers for a Query (decisions 5.2, 5.6
// and 5.9 of the phase 5 plan).
type Outcome int

const (
	// Cache answers from the cached copy: the copy the read path found, or
	// the one the cache holds now.
	Cache Outcome = iota
	// Stored answers from the copy the fetch stored.
	Stored
	// Missing answers 404: the remote said 404, the path may not go to the
	// remote, or the proxy is not ready.
	Missing
	// Failed answers 502: the remote failed, or what it sent was not stored.
	Failed
)

func (o Outcome) String() string {
	switch o {
	case Cache:
		return "cache"
	case Stored:
		return "stored"
	case Missing:
		return "missing"
	case Failed:
		return "failed"
	}
	return fmt.Sprintf("outcome %d", int(o))
}

// keep is the outcome of a query that sends no request: the cached copy, or
// 404 for a miss.
func keep(q Query) Outcome {
	if q.Cached != nil {
		return Cache
	}
	return Missing
}

// resultKind is what one fetch came to.
type resultKind int

const (
	resultMissing resultKind = iota // the remote said no
	resultCache                     // the cache holds a copy that needs no request
	resultStored                    // the answer of the remote is stored
	resultFailed                    // the remote failed, or its answer was not stored
)

// result is the result of one fetch, which every caller of its key takes.
type result struct {
	kind resultKind
	err  error // why the fetch failed, for the log
}

// outcome is the result as the caller of q answers it: a caller that holds a
// copy serves it whenever the fetch stored nothing (decision 5.6).
func (r result) outcome(q Query) Outcome {
	switch r.kind {
	case resultStored:
		return Stored
	case resultCache:
		return Cache
	case resultFailed:
		if q.Cached == nil {
			return Failed
		}
	}
	return keep(q)
}

func (p *Proxies) logger() *slog.Logger {
	if p.o.Logger != nil {
		return p.o.Logger
	}
	return slog.Default()
}

func (p *Proxies) warn(format string, args ...any) {
	p.logger().Warn(fmt.Sprintf(format, args...), "logger", logger)
}

// CheckSettings reads the typed settings of a proxy and logs a refusal, as
// the start log of the server does for each proxy (the rulings of brief 5b).
func (p *Proxies) CheckSettings(repo repos.Repo) error {
	_, err := p.settingsOf(repo)
	return err
}

// settingsOf are the typed settings of the proxy. A refused setting makes
// the proxy not ready; it is logged once per change, with repository and key
// and never the value.
func (p *Proxies) settingsOf(repo repos.Repo) (repos.ProxySettings, error) {
	settings, err := repo.ProxySettings()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		delete(p.refused, repo.Name)
		return settings, nil
	}
	if text := err.Error(); p.refused[repo.Name] != text {
		p.refused[repo.Name] = text
		p.warn("proxy %s serves its cache only and asks no remote: %v", repo.Name, err)
	}
	return settings, err
}

// remoteOf is the remote of a repository and the client for its settings
// and its secret file, made again when either changed. The secret file is
// read without the lock; a client another request made meanwhile for the
// same settings and file wins. A new client leaves the state needs secret
// of a refused secret (decision 5.12).
func (p *Proxies) remoteOf(name, format string, settings repos.ProxySettings, limit int64) (*remote, *Client, error) {
	if limit <= 0 {
		limit = repos.DefaultMaxBodyBytes // as NewClient takes it, so that every call of a repository meets one client
	}
	var key fileKey
	if settings.Auth.Built() {
		key = secretKey(p.o.DataDir, name)
	}
	p.mu.Lock()
	r := p.remotes[name]
	if r == nil {
		r = &remote{slots: make(chan struct{}, p.o.PerRepository)}
		p.remotes[name] = r
	}
	current := func() bool {
		return r.client != nil && r.format == format && r.settings == settings && r.limit == limit && r.secretKey == key
	}
	if current() {
		client := r.client
		p.mu.Unlock()
		return r, client, nil
	}
	p.mu.Unlock()
	secret, need := p.secretOf(name, settings.Auth)
	client, err := NewClient(Options{Repository: name, Settings: settings, Secret: secret, MaxBodyBytes: limit,
		TotalTimeout: p.o.TotalTimeout, Version: p.o.Version, Logger: p.o.Logger, dialer: p.o.dialer, classify: p.o.classify,
		tokens: formats[format].tokens, now: p.now})
	if err != nil {
		return nil, nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if current() {
		client.Close()
		return r, r.client, nil
	}
	if r.client != nil {
		r.client.Close() // its fetches in flight keep their connections
	}
	if need != "" && need != r.need {
		p.warn("proxy %s needs its secret and asks no remote: %s", name, need)
	}
	r.format, r.settings, r.limit, r.secretKey, r.client, r.need, r.refused = format, settings, limit, key, client, need, false
	return r, client, nil
}

// remotePath is the path of a request below the remote address: each
// segment of the decoded path escaped on its own, which Join decodes and
// escapes again.
func remotePath(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// Get answers one query of the read path. A fresh cached copy is served. A
// stale copy is asked for at the remote with its validators, and a miss of a
// ready proxy is fetched: one fetch per repository and path at a time, at
// most PerRepository of one repository and PerProcess in all at once; the
// whole answer of the remote goes through Store before the read path answers
// (decisions 5.4 to 5.8). The query of the client request never reaches the
// remote (ruling P5-9 of the phase 5 session). A proxy whose settings are
// refused, or that is blocked, sends nothing (decision 5.2). The error is the
// one of ctx when the caller left; the fetch goes on without it.
func (p *Proxies) Get(ctx context.Context, q Query) (Outcome, error) {
	settings, err := p.settingsOf(q.Repo)
	if err != nil || settings.Blocked || !Fetches(q.Repo.Format) {
		return keep(q), nil
	}
	if q.Cached != nil && permanent(q.Repo, q.Path) {
		return Cache, nil // its address is its digest: never checked again
	}
	st := p.stateOf(q.Repo.Name, q.Path)
	if q.Cached != nil {
		if !stale(st, q.Cached.CreatedMS, p.nowMS(), ageOf(q.Repo, settings, q.Path)) {
			return Cache, nil
		}
	} else if !fetchable(q.Repo, q.Path) || !q.shared() && marked(st, settings, p.nowMS()) {
		return Missing, nil
	}
	r, client, err := p.remoteOf(q.Repo.Name, q.Repo.Format, settings, q.Limit)
	if err != nil {
		p.warn("proxy %s serves its cache only and asks no remote: %v", q.Repo.Name, err)
		return keep(q), nil
	}
	if p.needs(q.Repo.Name, r) != "" || p.waiting(r, settings) {
		return keep(q), nil // needs secret or auto blocked (decisions 5.2, 5.3, 5.12)
	}
	res, err := p.flights.do(ctx, q.flight(), func() result {
		res := p.fetch(q, r, client, settings)
		if res.err != nil {
			// the path a client chose and an error a remote may have worded, quoted
			p.warn("proxy %s: %q: %q", q.Repo.Name, q.Path, res.err)
		}
		return res
	})
	if err != nil {
		return keep(q), err
	}
	return res.outcome(q), nil
}

// fetch is one fetch of a path: the look into the cache and into
// proxy_state, the admission of auto block, a slot of the repository and one
// of the process, the request, with the validators of a stale copy, and the
// store. The verdict of the request goes to auto block.
func (p *Proxies) fetch(q Query, r *remote, client *Client, settings repos.ProxySettings) result {
	cached := q.Cached
	if q.Look != nil {
		var err error
		if cached, err = q.Look(); err != nil {
			return result{kind: resultFailed, err: err}
		}
	}
	st := p.stateOf(q.Repo.Name, q.Path)
	header := http.Header{}
	switch {
	case cached != nil && (permanent(q.Repo, q.Path) || !stale(st, cached.CreatedMS, p.nowMS(), ageOf(q.Repo, settings, q.Path))):
		return result{kind: resultCache} // a fetch that ended while this one waited checked it
	case cached != nil:
		header = validators(st.Row)
	case !q.shared() && marked(st, settings, p.nowMS()):
		return result{kind: resultMissing} // a fetch that ended while this one waited met a 404
	}
	for key, values := range q.Header {
		header[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
	probe, admitted := p.admit(r, settings)
	if !admitted {
		return notReady(cached)
	}
	v := unjudged
	defer func() { p.report(q.Repo.Name, r, settings, v, probe) }()
	r.slots <- struct{}{}
	defer func() { <-r.slots }()
	p.slots <- struct{}{}
	defer func() { <-p.slots }()
	resp, err := client.Do(context.Background(), http.MethodGet, q.remote(), "", header)
	if err != nil {
		v = judge(err)
		return p.failed(cached, err)
	}
	defer resp.Body.Close()
	v = answered
	if resp.StatusCode >= 500 {
		v = failure
	}
	switch status := resp.StatusCode; {
	case status == http.StatusOK:
		digests, err := p.expected(q, client)
		if err != nil {
			return p.failed(cached, err)
		}
		body := &watched{r: resp.Body}
		res := p.store(q, resp, body, digests)
		if body.err != nil {
			v = failure // the remote broke the body off
		}
		if res.kind == resultStored {
			p.putRow(checked(q.Repo.Name, q.Path, nil, p.nowMS(), resp.Header))
		}
		if res.kind == resultFailed && cached != nil {
			res.kind = resultCache // the stale copy stays (decision 5.6)
		}
		return res
	case status == http.StatusNotModified && cached != nil:
		p.putRow(checked(q.Repo.Name, q.Path, st.Row, p.nowMS(), resp.Header))
		return result{kind: resultCache}
	case status == http.StatusNotFound && cached != nil:
		return result{kind: resultCache} // a removed file of the remote does not break a build (decision 5.6)
	case status == http.StatusNotFound:
		if !q.shared() {
			p.remember(q.Repo.Name, q.Path, settings)
		}
		return result{kind: resultMissing}
	case status == http.StatusUnauthorized && settings.Auth.Built() && credentialsRefused(resp):
		p.refuse(q.Repo.Name, r)
		return notReady(cached)
	case status >= 400 && status < 500:
		if cached != nil {
			return result{kind: resultCache, err: fmt.Errorf("the remote answered %d; the cached copy is served", status)}
		}
		return result{kind: resultMissing, err: fmt.Errorf("the remote answered %d", status)}
	}
	return p.failed(cached, fmt.Errorf("the remote answered %d", resp.StatusCode))
}

// notReady is the result of a fetch the state of its proxy keeps from the
// remote: the cached copy, else 404 (decision 5.9).
func notReady(cached *Cached) result {
	if cached != nil {
		return result{kind: resultCache}
	}
	return result{kind: resultMissing}
}

// failed is the result of a remote that failed: the cached copy when there
// is one (decision 5.6), else 502 (decision 5.9).
func (p *Proxies) failed(cached *Cached, err error) result {
	if cached != nil {
		return result{kind: resultCache, err: fmt.Errorf("%w; the cached copy is served", err)}
	}
	return result{kind: resultFailed, err: err}
}

// store hands a 200 of the remote to the store of the read path, with the
// digests the bytes must have. Nothing of a body the store refuses, that
// breaks off or that has other digests is kept (ruling P5-3, decision 5.10).
func (p *Proxies) store(q Query, resp *http.Response, body io.Reader, digests map[string]string) result {
	if q.Store == nil {
		return result{kind: resultFailed, err: errors.New("the read path stores nothing")}
	}
	f := Fetched{Body: body, Size: resp.ContentLength, ContentType: resp.Header.Get("Content-Type"), Header: resp.Header.Clone(),
		Digests: digests}
	if err := q.Store(f); err != nil {
		return result{kind: resultFailed, err: fmt.Errorf("the answer of the remote is not stored: %w", err)}
	}
	return result{kind: resultStored}
}

// maxChecksumBytes bounds the checksum file of the remote: 40 hex digits,
// maybe a file name after them.
const maxChecksumBytes = 4096

// expected are the digests a 200 of the remote for q must have: those of
// the query, and for a new artifact of a format with a checksum file (the
// .sha1 of maven2) the SHA-1 that file at the remote holds, asked once per
// fetch (decision 5.10). A remote without that file, which answers a 4xx
// for it, is accepted. A checksum file the remote fails to give, or one that
// holds no SHA-1, fails the fetch.
func (p *Proxies) expected(q Query, client *Client) (map[string]string, error) {
	digests := maps.Clone(q.Digests)
	if f := formats[q.Repo.Format]; f.artifact == nil || !f.artifact(q.Path) {
		return digests, nil
	}
	resp, err := client.Do(context.Background(), http.MethodGet, q.remote()+".sha1", "", nil)
	if err != nil {
		return nil, fmt.Errorf("the .sha1 of the remote is not read: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return digests, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("the remote answered %d for the .sha1", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumBytes+1))
	if err != nil {
		return nil, fmt.Errorf("the .sha1 of the remote is not read: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(data) > maxChecksumBytes || len(fields) == 0 || !isSHA1(fields[0]) {
		return nil, errors.New("the .sha1 of the remote holds no SHA-1")
	}
	if digests == nil {
		digests = map[string]string{}
	}
	digests["sha1"] = strings.ToLower(fields[0])
	return digests, nil
}

// isSHA1 reports 40 hex digits.
func isSHA1(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}
