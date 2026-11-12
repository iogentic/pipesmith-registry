package server

import (
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/pystr"
	"github.com/mirosso25/pipesmith-registry/internal/repos"
	"github.com/mirosso25/pipesmith-registry/internal/stores"
	"github.com/mirosso25/pipesmith-registry/internal/version"
)

// The JSON API: /api/status, /api/repositories, /api/browse/<repo> and
// POST /api/scan. /api/health and /api/asset are in server.go and asset.go.

const (
	productName    = "Pipesmith Registry"
	postHeader     = "X-Artifact-Repository" // the wire name the page and the clients send
	browseLimit    = 500
	maxBrowseLimit = 10_000
)

// listingPage is LISTING_PAGE: the children read per query when a group
// pages over its members. A variable, so a test can cross the page bounds.
var listingPage = 10_000

func optionalInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// clampInt is a page offset as an int; an offset beyond the rows of any
// index skips them all, as the Python loop does.
func clampInt(n *big.Int) int {
	if !n.IsInt64() || n.Int64() > math.MaxInt {
		return math.MaxInt
	}
	return int(n.Int64())
}

// apiStatus is GET /api/status: ReadPath.status with the snapshot of the
// scan job, taken first as Python takes it.
func (s *Server) apiStatus(w *recorder, req *request, _ []string) error {
	if s.sec != nil {
		if subject, err := s.subjectOf(req); err != nil || subject == nil {
			return shortStatus(w, req)
		}
	}
	scanState := s.job.snapshot()
	st, err := s.rp.sync()
	if err != nil {
		return err
	}
	storeStats, err := s.rp.index.StoreStats()
	if err != nil {
		return err
	}
	perStore := map[string]index.StoreStat{}
	for _, stat := range storeStats {
		perStore[stat.Store] = stat
	}
	list := []any{}
	for _, sc := range st.configs {
		var repoNames, manifest any
		if sc.Repos != nil {
			names := make([]any, len(sc.Repos))
			for i, name := range sc.Repos {
				names[i] = name
			}
			repoNames = names
		}
		if sc.Manifest != nil {
			manifest = *sc.Manifest
		}
		stat := perStore[sc.Name]
		list = append(list, pyjson.NewObject().Set("name", sc.Name).Set("kind", sc.Kind).Set("root", sc.Root).
			Set("repos", repoNames).Set("manifest", manifest).Set("available", !stores.LooksUnmounted(st.stores[sc.Name])).
			Set("files", stat.Files).Set("bytes", stat.Bytes))
	}
	repoStats, err := s.rp.index.RepoStats()
	if err != nil {
		return err
	}
	var files, size int64
	for _, stat := range repoStats {
		files += stat.Files
		size += stat.Bytes
	}
	ports := []any{}
	for _, port := range sortedPorts(s.dockerPorts) {
		ports = append(ports, pyjson.NewObject().Set("port", port).Set("repo", s.dockerPorts[port]))
	}
	totals := pyjson.NewObject().Set("stores", len(list)).Set("repositories", len(st.repositories.All())).
		Set("files", files).Set("bytes", size)
	return writeJSON(w, req, http.StatusOK, pyjson.NewObject().
		Set("name", productName).
		Set("version", version.Version).
		Set("data_dir", s.rp.cfg.DataDir).
		Set("bind", s.rp.cfg.Bind).
		Set("port", s.port).
		Set("docker_ports", ports).
		Set("stores", list).
		Set("totals", totals).
		Set("scan", scanState))
}

// apiRepositories is GET /api/repositories. The Docker ports are those of
// the configuration the server started with, as Python keeps its cfg.
func (s *Server) apiRepositories(w *recorder, req *request, _ []string) error {
	st, err := s.rp.sync()
	if err != nil {
		return err
	}
	list, err := s.rp.index.RepoStats()
	if err != nil {
		return err
	}
	stats := make([]repos.Stat, len(list))
	for i, stat := range list {
		stats[i] = repos.Stat{Repo: stat.Repo, Files: stat.Files, Bytes: stat.Bytes, NewestMS: stat.NewestMS}
	}
	rows := repos.RepositoryRows(st.repositories, stats, s.rp.cfg.DockerPorts)
	if s.sec != nil {
		rows = slices.DeleteFunc(rows, func(row repos.Row) bool { return !s.mayBrowse(req, repos.Repo{Name: row.Name, Format: row.Format}) })
	}
	return writeJSON(w, req, http.StatusOK, repos.RowsDocument(rows))
}

// apiBrowse is GET /api/browse/<repo>?path=&limit=&offset=: the parameters
// are checked before the repository.
func (s *Server) apiBrowse(w *recorder, req *request, match []string) error {
	name := match[1]
	q := req.params()
	var param *string
	if value, ok := q.get("path"); ok {
		param = &value
	}
	directory, apiErr := apiPath(param)
	if apiErr != nil {
		return jsonError(w, req, apiErr.status, apiErr.message)
	}
	high := int64(maxBrowseLimit)
	limit, apiErr := intParam(q, "limit", browseLimit, 1, &high)
	if apiErr != nil {
		return jsonError(w, req, apiErr.status, apiErr.message)
	}
	offset, apiErr := intParam(q, "offset", 0, 0, nil)
	if apiErr != nil {
		return jsonError(w, req, apiErr.status, apiErr.message)
	}
	st, err := s.rp.sync()
	if err != nil {
		return err
	}
	if _, ok := st.repositories.Get(name); !ok {
		return jsonError(w, req, http.StatusNotFound, "repository "+name+" not found")
	}
	members := st.repositories.Resolve(name)
	if directory != "" {
		isDir, err := s.rp.anyIsDir(members, directory)
		if err != nil {
			return err
		}
		if !isDir {
			return jsonError(w, req, http.StatusNotFound, directory+" is not a directory of "+name)
		}
	}
	children, more, err := s.rp.page(members, directory, int(limit.Int64()), clampInt(offset))
	if err != nil {
		return err
	}
	prefix := ""
	if directory != "" {
		prefix = directory + "/"
	}
	list := make([]any, len(children))
	for i, c := range children {
		list[i] = pyjson.NewObject().Set("name", c.Name).Set("path", prefix+c.Name).Set("is_dir", c.IsDir).
			Set("size", optionalInt(c.Size)).Set("created_ms", optionalInt(c.CreatedMS))
	}
	return writeJSON(w, req, http.StatusOK, pyjson.NewObject().
		Set("repo", name).
		Set("path", directory).
		Set("limit", limit).
		Set("offset", offset).
		Set("more", more).
		Set("children", list))
}

// childCursor is _all_children: the children of one member, read in pages
// of listingPage as the merge asks for them.
type childCursor struct {
	ix         *index.Index
	repo, dir  string
	page       []index.Child
	pos, next  int
	readToLast bool
}

func (c *childCursor) peek() (*index.Child, error) {
	for c.pos == len(c.page) {
		if c.readToLast {
			return nil, nil
		}
		page, err := c.ix.Children(c.repo, c.dir, listingPage, c.next)
		if err != nil {
			return nil, err
		}
		c.page, c.pos = page, 0
		c.next += len(page)
		c.readToLast = len(page) < listingPage
	}
	return &c.page[c.pos], nil
}

// childBefore orders by the key (name, not is_dir) of _child_key: of one
// name the directory comes first.
func childBefore(a, b *index.Child) bool {
	return a.Name < b.Name || a.Name == b.Name && a.IsDir && !b.IsDir
}

// page is ReadPath._page: one page of the children and whether more
// follow. A group pages over the union of its members; of one name and
// kind the first member wins, as heapq.merge keeps the order of its inputs.
func (rp *readPath) page(members []repos.Repo, directory string, limit, offset int) ([]index.Child, bool, error) {
	if len(members) == 0 {
		return nil, false, nil
	}
	var rows []index.Child
	if len(members) == 1 {
		var err error
		if rows, err = rp.index.Children(members[0].Name, directory, limit+1, offset); err != nil {
			return nil, false, err
		}
	} else {
		cursors := make([]*childCursor, len(members))
		for i, m := range members {
			cursors[i] = &childCursor{ix: rp.index, repo: m.Name, dir: directory}
		}
		var previous *index.Child
		skipped := 0
		for len(rows) <= limit {
			best, bestChild := -1, (*index.Child)(nil)
			for i, c := range cursors {
				head, err := c.peek()
				if err != nil {
					return nil, false, err
				}
				if head != nil && (bestChild == nil || childBefore(head, bestChild)) {
					best, bestChild = i, head
				}
			}
			if best < 0 {
				break
			}
			child := *bestChild
			cursors[best].pos++
			if previous != nil && previous.Name == child.Name && previous.IsDir == child.IsDir {
				continue // equal keys are adjacent, and the first member comes first
			}
			previous = &child
			if skipped < offset {
				skipped++
				continue
			}
			rows = append(rows, child)
		}
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// latin1Lower is str.lower() of a latin-1 text: A to Z and À to Þ without ×.
func latin1Lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' || c >= 0xC0 && c <= 0xDE && c != 0xD7 {
			b[i] = c + 32
		}
	}
	return string(b)
}

// latin1Text is the str Starlette makes of the bytes of a header value.
func latin1Text(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		b = utf8.AppendRune(b, rune(s[i]))
	}
	return string(b)
}

// postRefusal is _post_refusal: why a state changing request is refused,
// a guard against requests from other sites. port is the port the server
// listens on, names the host names of Options.HostNames.
func postRefusal(r *http.Request, port int, names []string) (string, bool) {
	if values := r.Header.Values(postHeader); len(values) == 0 || values[0] != "1" {
		return "the header " + postHeader + ": 1 is missing", true
	}
	p := strconv.Itoa(port)
	hosts := []string{"localhost:" + p, "127.0.0.1:" + p, "[::1]:" + p}
	for _, name := range names {
		hosts = append(hosts, name+":"+p)
	}
	host := latin1Lower(r.Host)
	if !slices.Contains(hosts, host) {
		return fmt.Sprintf("the Host %s is not this server", pystr.Repr(latin1Text(host))), true
	}
	if origins := r.Header.Values("Origin"); len(origins) > 0 {
		origin := origins[0]
		if !slices.Contains(hosts, strings.TrimPrefix(latin1Lower(origin), "http://")) || !strings.HasPrefix(latin1Lower(origin), "http://") {
			return fmt.Sprintf("the Origin %s is not this server", pystr.Repr(latin1Text(origin))), true
		}
	}
	return "", false
}

// apiScan is POST /api/scan with {"store": <name>}: 202 and the state of
// the job when the scan starts, 409 while one runs.
func (s *Server) apiScan(w *recorder, req *request, _ []string) error {
	if refusal, refused := postRefusal(req.r, s.port, s.hostNames); refused {
		return jsonError(w, req, http.StatusForbidden, refusal)
	}
	data, err := io.ReadAll(req.r.Body)
	if err != nil {
		// a body h11 cannot read ends the request as uvicorn ends it
		s.invalidRequest(w.ResponseWriter)
		return nil
	}
	value, err := pyjson.Decode(data)
	if err != nil {
		if pyjson.IsValueError(err) {
			return jsonError(w, req, http.StatusBadRequest, "the body is not JSON")
		}
		return err
	}
	name, ok := "", false
	if body, isObject := value.(*pyjson.Object); isObject {
		if v, found := body.Get("store"); found {
			name, ok = v.(string)
		}
	}
	if !ok || name == "" {
		return jsonError(w, req, http.StatusBadRequest, `the body needs {"store": <name>}`)
	}
	st, err := s.rp.sync()
	if err != nil {
		return err
	}
	store := st.stores[name]
	if store == nil {
		return jsonError(w, req, http.StatusNotFound, "store "+name+" is not attached")
	}
	if !s.job.start(store) {
		return writeJSON(w, req, http.StatusConflict, pyjson.NewObject().Set("error", "a scan is running").Set("scan", s.job.snapshot()))
	}
	return writeJSON(w, req, http.StatusAccepted, s.job.snapshot())
}
