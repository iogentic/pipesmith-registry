// Package forge is a stub of the generic package registry of a Forgejo
// instance, for tests of the update channel. It serves a directory of release
// folders, <dir>/<version>/<file>, as tools/release.sh writes dist/<version>/:
// the folder name is the version without the leading v, and the forge
// version is v plus that name. A folder without cosign.pub is an incomplete
// version. It is a test tool and is never shipped.
//
// The routes and the answer shape follow the Forgejo API documentation and
// source (swagger of code.forgejo.org, info.version 17.0.2+gitea-1.22.0;
// routers/api/v1/packages/package.go, modules/structs/package.go,
// services/context/api.go SetLinkHeader, routers/api/packages/generic), as
// work/plans/exec/scout-phase8.md digests them on 2026-11-18. They were not
// compared with a recorded answer of a forge. Unverified there and chosen
// here: q matches a part of the name, the list is sorted by version folder
// name, the owner object holds id and login, and a missing or wrong token
// answers 404 like a package the caller cannot see.
package forge

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

// Package is the only package name the stub serves.
const Package = "pipesmith-registry"

// Page sizes of a forge with the defaults of app.ini.
const (
	DefaultPageSize = 30
	MaxPageSize     = 50
)

// User is the owner object of an entry.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// Entry is one element of the package list: one version of one package.
type Entry struct {
	ID        int64     `json:"id"`
	Owner     User      `json:"owner"`
	Type      string    `json:"type"`
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
}

// Options configure a stub.
type Options struct {
	Dir   string // the directory of release folders
	Owner string // the only owner that exists
	Token string // when set, every request needs Authorization: token <Token>
	// Extra entries are listed after the folders, whatever type and q ask
	// for, as a forge whose filters match loosely would list them.
	Extra []Entry
}

// Request is one request the stub received.
type Request struct {
	Method        string
	Path          string
	Query         string
	Authorization string
}

// Stub is the handler.
type Stub struct {
	opt      Options
	mu       sync.Mutex
	requests []Request
}

// New returns a stub.
func New(opt Options) *Stub { return &Stub{opt: opt} }

// Requests returns the requests received so far, in order.
func (s *Stub) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

var fileName = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)

func (s *Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Authorization: r.Header.Get("Authorization")})
	s.mu.Unlock()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, `{"message":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if s.opt.Token != "" && r.Header.Get("Authorization") != "token "+s.opt.Token {
		notFound(w)
		return
	}
	if owner, ok := strings.CutPrefix(r.URL.Path, "/api/v1/packages/"); ok {
		if owner != s.opt.Owner {
			notFound(w)
			return
		}
		s.list(w, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/packages/"+s.opt.Owner+"/generic/"+Package+"/")
	if !ok {
		notFound(w)
		return
	}
	version, file, ok := strings.Cut(rest, "/")
	if !ok || !fileName.MatchString(file) || !strings.HasPrefix(version, "v") || !fileName.MatchString(version[1:]) {
		notFound(w)
		return
	}
	s.download(w, r, version[1:], file)
}

func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	io.WriteString(w, `{"message":"The target couldn't be found."}`)
}

// entries lists the folders of the directory as entries, then Extra.
func (s *Stub) entries(r *http.Request) ([]Entry, error) {
	dirs, err := os.ReadDir(s.opt.Dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range dirs {
		if d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
			names = append(names, d.Name())
		}
	}
	sort.Strings(names)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	typ, q := r.URL.Query().Get("type"), r.URL.Query().Get("q")
	var out []Entry
	for i, name := range names {
		info, err := os.Stat(filepath.Join(s.opt.Dir, name))
		if err != nil {
			return nil, err
		}
		if typ != "" && typ != "generic" || !strings.Contains(Package, q) {
			continue
		}
		out = append(out, Entry{ID: int64(i + 1), Owner: User{ID: 1, Login: s.opt.Owner}, Type: "generic", Name: Package,
			Version:   "v" + name,
			HTMLURL:   fmt.Sprintf("%s://%s/%s/-/packages/generic/%s/v%s", scheme, r.Host, s.opt.Owner, Package, name),
			CreatedAt: info.ModTime().UTC().Truncate(time.Second)})
	}
	return append(out, s.opt.Extra...), nil
}

func (s *Stub) list(w http.ResponseWriter, r *http.Request) {
	all, err := s.entries(r)
	if err != nil {
		http.Error(w, `{"message":"internal error"}`, http.StatusInternalServerError)
		return
	}
	query := r.URL.Query()
	page, _ := strconv.Atoi(query.Get("page"))
	if page <= 0 {
		page = 1
	}
	size, _ := strconv.Atoi(query.Get("limit"))
	if size <= 0 {
		size = DefaultPageSize
	}
	size = min(size, MaxPageSize)
	start := min((page-1)*size, len(all))
	end := min(start+size, len(all))
	items := all[start:end]
	if items == nil {
		items = []Entry{}
	}
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	if link := linkHeader(r, page, size, len(all)); link != "" {
		w.Header().Set("Link", link)
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(len(all)))
	w.Header().Set("Access-Control-Expose-Headers", "X-Total-Count, Link")
	json.NewEncoder(w).Encode(items)
}

// linkHeader is the Link header of a page: next and last when the page is
// not the last one, first and prev when it is not the first one.
func linkHeader(r *http.Request, page, size, total int) string {
	last := (total + size - 1) / size
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	at := func(p int, rel string) string {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(p))
		u := url.URL{Scheme: scheme, Host: r.Host, Path: r.URL.Path, RawQuery: q.Encode()}
		return fmt.Sprintf("<%s>; rel=%q", u.String(), rel)
	}
	var links []string
	if page < last {
		links = append(links, at(page+1, "next"), at(last, "last"))
	}
	if page > 1 {
		links = append(links, at(1, "first"), at(page-1, "prev"))
	}
	return strings.Join(links, ",")
}

func (s *Stub) download(w http.ResponseWriter, r *http.Request, folder, file string) {
	root, err := os.OpenRoot(filepath.Join(s.opt.Dir, folder))
	if err != nil {
		notFound(w)
		return
	}
	defer root.Close()
	f, err := root.Open(file)
	if err != nil {
		notFound(w)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		io.Copy(w, f)
	}
}
