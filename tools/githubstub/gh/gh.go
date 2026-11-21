// Package gh is a stub of the releases API of GitHub, for tests of the
// update channel and of the release check of serve. It serves a directory
// of release folders, <dir>/<version>/<file>, as tools/release.sh writes
// dist/<version>/: the folder name is the version without the leading v and
// the tag is v plus that name. notes.md of a folder is the body of the
// release; prerelease and draft, when present as empty files, mark the
// release so. It is a test tool and is never shipped.
//
// The routes and the answer shape follow the answers of api.github.com for
// iogentic/pipesmith-registry, read on 2026-11-21: GET
// /repos/{owner}/{repo}/releases with per_page and page, newest first, a Link
// header with rel="next" while a page follows; GET
// /repos/{owner}/{repo}/releases/assets/{id} with Accept:
// application/octet-stream answers a redirect to the file. The stub redirects
// to its own origin, where GitHub redirects to release-assets.githubusercontent.com.
// Every answer carries x-ratelimit-limit, x-ratelimit-remaining and
// x-ratelimit-reset; past the limit it answers 403 as GitHub does.
package gh

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options configure a stub.
type Options struct {
	Dir   string // the directory of release folders
	Owner string
	Repo  string
	// Limit is the number of requests answered before the stub answers 403
	// with x-ratelimit-remaining 0; 0 is no limit.
	Limit int
	// Reset is the time x-ratelimit-reset names; zero is an hour after the
	// first request.
	Reset time.Time
	// Published gives the published time of a version; nil takes the
	// modification time of its folder.
	Published func(version string) time.Time
}

// Request is one request the stub received.
type Request struct {
	Method string
	Path   string
	Query  string
	Accept string
	Agent  string
}

// Stub is the handler.
type Stub struct {
	opt      Options
	mu       sync.Mutex
	requests []Request
	reset    time.Time
}

// New returns a stub.
func New(opt Options) *Stub { return &Stub{opt: opt, reset: opt.Reset} }

// Requests returns the requests received so far, in order.
func (s *Stub) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// SetLimit changes the limit; the requests counted so far stay counted.
func (s *Stub) SetLimit(n int) {
	s.mu.Lock()
	s.opt.Limit = n
	s.mu.Unlock()
}

// Asset is one asset of a release in the answer.
type Asset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	URL                string `json:"url"`
	BrowserDownloadURL string `json:"browser_download_url"`
	ContentType        string `json:"content_type"`
	State              string `json:"state"`
}

// Release is one element of the release list.
type Release struct {
	ID          int64   `json:"id"`
	TagName     string  `json:"tag_name"`
	Name        string  `json:"name"`
	Body        string  `json:"body"`
	Draft       bool    `json:"draft"`
	Prerelease  bool    `json:"prerelease"`
	HTMLURL     string  `json:"html_url"`
	PublishedAt string  `json:"published_at"`
	Assets      []Asset `json:"assets"`
}

const maxPerPage = 100

var plainName = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)

// meta are the files of a folder that are no asset.
var meta = map[string]bool{"notes.md": true, "prerelease": true, "draft": true}

func (s *Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Accept: r.Header.Get("Accept"), Agent: r.Header.Get("User-Agent")})
	count := len(s.requests)
	if s.reset.IsZero() {
		s.reset = time.Now().Add(time.Hour)
	}
	limit, reset := s.opt.Limit, s.reset
	s.mu.Unlock()
	if limit > 0 {
		w.Header().Set("X-Ratelimit-Limit", strconv.Itoa(limit))
		w.Header().Set("X-Ratelimit-Remaining", strconv.Itoa(max(limit-count, 0)))
		w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		if count > limit {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"message":"API rate limit exceeded for 127.0.0.1.","documentation_url":"https://docs.github.com/rest/overview/resources-in-the-rest-api#rate-limiting"}`)
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		message(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	base := "/repos/" + s.opt.Owner + "/" + s.opt.Repo + "/releases"
	switch {
	case r.URL.Path == base:
		s.list(w, r)
	case strings.HasPrefix(r.URL.Path, base+"/assets/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, base+"/assets/"), 10, 64)
		if err != nil {
			message(w, http.StatusNotFound, "Not Found")
			return
		}
		s.asset(w, r, id)
	case strings.HasPrefix(r.URL.Path, "/download/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/download/"), 10, 64)
		if err != nil {
			message(w, http.StatusNotFound, "Not Found")
			return
		}
		s.download(w, r, id)
	default:
		message(w, http.StatusNotFound, "Not Found")
	}
}

func message(w http.ResponseWriter, code int, text string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"message":%q}`, text)
}

type file struct{ folder, name string }

// releases reads the folders, newest version first, and the asset ids.
func (s *Stub) releases(r *http.Request) ([]Release, map[int64]file, error) {
	dirs, err := os.ReadDir(s.opt.Dir)
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, d := range dirs {
		if d.IsDir() && plainName.MatchString(d.Name()) && !strings.HasPrefix(d.Name(), ".") {
			names = append(names, d.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool { return newer(names[i], names[j]) })
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	origin := scheme + "://" + r.Host
	ids := map[int64]file{}
	var out []Release
	next := int64(1000)
	for i, name := range names {
		folder := filepath.Join(s.opt.Dir, name)
		entries, err := os.ReadDir(folder)
		if err != nil {
			return nil, nil, err
		}
		rel := Release{ID: int64(i + 1), TagName: "v" + name, Name: "v" + name,
			HTMLURL: fmt.Sprintf("%s/%s/%s/releases/tag/v%s", origin, s.opt.Owner, s.opt.Repo, name), Assets: []Asset{}}
		info, err := os.Stat(folder)
		if err != nil {
			return nil, nil, err
		}
		published := info.ModTime()
		if s.opt.Published != nil {
			published = s.opt.Published(name)
		}
		rel.PublishedAt = published.UTC().Truncate(time.Second).Format(time.RFC3339)
		for _, e := range entries {
			n := e.Name()
			switch {
			case n == "notes.md":
				body, err := os.ReadFile(filepath.Join(folder, n)) // #nosec G304 -- a file of the test directory.
				if err != nil {
					return nil, nil, err
				}
				rel.Body = string(body)
			case n == "prerelease":
				rel.Prerelease = true
			case n == "draft":
				rel.Draft = true
			case meta[n] || !e.Type().IsRegular() || !plainName.MatchString(n):
			default:
				fi, err := e.Info()
				if err != nil {
					return nil, nil, err
				}
				next++
				ids[next] = file{name, n}
				rel.Assets = append(rel.Assets, Asset{ID: next, Name: n, Size: fi.Size(), State: "uploaded",
					ContentType: "application/octet-stream",
					URL:         fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d", origin, s.opt.Owner, s.opt.Repo, next),
					BrowserDownloadURL: fmt.Sprintf("%s/%s/%s/releases/download/v%s/%s", origin, s.opt.Owner, s.opt.Repo,
						name, url.PathEscape(n))})
			}
		}
		out = append(out, rel)
	}
	return out, ids, nil
}

// newer orders version folder names X.Y.Z by number, highest first; other
// names sort after them by text.
func newer(a, b string) bool {
	pa, oka := parts(a)
	pb, okb := parts(b)
	switch {
	case oka && okb:
		for i := range pa {
			if pa[i] != pb[i] {
				return pa[i] > pb[i]
			}
		}
		return false
	case oka != okb:
		return oka
	}
	return a > b
}

func parts(name string) ([3]int, bool) {
	var out [3]int
	fields := strings.Split(name, ".")
	if len(fields) != 3 {
		return out, false
	}
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func (s *Stub) list(w http.ResponseWriter, r *http.Request) {
	all, _, err := s.releases(r)
	if err != nil {
		message(w, http.StatusInternalServerError, "internal error")
		return
	}
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	size, _ := strconv.Atoi(q.Get("per_page"))
	if size <= 0 {
		size = 30
	}
	size = min(size, maxPerPage)
	start := min((page-1)*size, len(all))
	end := min(start+size, len(all))
	items := all[start:end]
	if items == nil {
		items = []Release{}
	}
	if end < len(all) {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		q.Set("page", strconv.Itoa(page+1))
		u := url.URL{Scheme: scheme, Host: r.Host, Path: r.URL.Path, RawQuery: q.Encode()}
		w.Header().Set("Link", fmt.Sprintf("<%s>; rel=\"next\"", u.String()))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(items)
}

func (s *Stub) asset(w http.ResponseWriter, r *http.Request, id int64) {
	_, ids, err := s.releases(r)
	if err != nil {
		message(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, ok := ids[id]; !ok {
		message(w, http.StatusNotFound, "Not Found")
		return
	}
	if r.Header.Get("Accept") != "application/octet-stream" {
		message(w, http.StatusOK, "asset metadata")
		return
	}
	http.Redirect(w, r, "/download/"+strconv.FormatInt(id, 10), http.StatusFound)
}

func (s *Stub) download(w http.ResponseWriter, r *http.Request, id int64) {
	_, ids, err := s.releases(r)
	if err != nil {
		message(w, http.StatusInternalServerError, "internal error")
		return
	}
	f, ok := ids[id]
	if !ok {
		message(w, http.StatusNotFound, "Not Found")
		return
	}
	root, err := os.OpenRoot(filepath.Join(s.opt.Dir, f.folder))
	if err != nil {
		message(w, http.StatusNotFound, "Not Found")
		return
	}
	defer root.Close()
	in, err := root.Open(f.name)
	if err != nil {
		message(w, http.StatusNotFound, "Not Found")
		return
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		message(w, http.StatusNotFound, "Not Found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		io.Copy(w, in)
	}
}
