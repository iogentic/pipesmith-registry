package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/export"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// The export of a folder as one archive: GET /api/export/<repository>
// with path, form, platform and dry (docs/plans/browse-tools.md, section
// Export Contract). The route has the rule of a download, view(1, "read"),
// so every entry passes the check a GET of its path passes; an entry is
// read through rp.file, the function of the download, so it holds the bytes
// that GET answers, for a group as well.

// formOf is the export form of each access kind.
var formOf = map[string]string{"maven": "maven", "files": "zip", "npm": "npm", "docker": "oci"}

// exportHeaders go on every refusal of the export, as on the REST answers.
var exportHeaders = [][2]string{{"Content-Type", jsonType}, {"X-Content-Type-Options", "nosniff"}, {"Cache-Control", "no-store"}}

func exportRefusal(w http.ResponseWriter, req *request, status int, message string, extra ...[2]any) error {
	body := pyjson.NewObject().Set("message", message)
	for _, kv := range extra {
		body.Set(kv[0].(string), kv[1])
	}
	data, err := pyjson.ResponseBody(body)
	if err != nil {
		return err
	}
	writeBody(w, req, status, exportHeaders, data)
	return nil
}

// exportRequest is a request of the export after its parameters passed.
type exportRequest struct {
	name, folder, form string
	dry                bool
	repo               repos.Repo
	members            []repos.Repo
	st                 *state
}

// exportEntry is one listed path: the archive entry, the index row of the
// member that wins it, and the size the limits count: the index size, or
// for a merged maven-metadata.xml of a group the size of the merge.
type exportEntry struct {
	name  string
	asset index.Asset
	size  int64
}

// streamsAtATime bound the exports that stream at the same time (call
// B23); one export may read 2 GiB from a store.
const (
	streamsAtATime = 2
	retrySeconds   = 30
	// defaultStall is how long one write of an export may wait for the
	// client to read before the export ends and frees its place.
	defaultStall = 60 * time.Second
	// defaultTimeLimit is how long one export may run in all: a client
	// that reads just fast enough for the stall time would else hold its
	// place for days.
	defaultTimeLimit = 2 * time.Hour
	// leftOutShown bounds the paths a dry call names that the archive
	// leaves out; left_out counts them all
	leftOutShown = 20
)

// exports is the state of the export in a Server: its limits and one place
// per export that streams.
type exports struct {
	limits export.Limits
	slots  chan struct{}
	stall  time.Duration
	limit  time.Duration
	// running holds the accounts with an export that streams: one at a
	// time per account, so that one account cannot hold both places
	mu      sync.Mutex
	running map[string]bool
}

func newExports(opts Options) *exports {
	limits := export.DefaultLimits()
	if opts.ExportMaxFiles > 0 {
		limits.MaxFiles = opts.ExportMaxFiles
	}
	if opts.ExportMaxBytes > 0 {
		limits.MaxBytes = opts.ExportMaxBytes
	}
	stall := defaultStall
	if opts.ExportStall > 0 {
		stall = opts.ExportStall
	}
	limit := defaultTimeLimit
	if opts.ExportTimeLimit > 0 {
		limit = opts.ExportTimeLimit
	}
	return &exports{limits: limits, slots: make(chan struct{}, streamsAtATime), stall: stall, limit: limit, running: map[string]bool{}}
}

func (s *Server) limits() export.Limits { return s.exports.limits }

// exportRoute is the handler of the route.
func (s *Server) exportRoute(w *recorder, req *request, match []string) error {
	q := req.params()
	er := &exportRequest{name: match[1]}
	var param *string
	if value, ok := q.get("path"); ok {
		param = &value
	}
	folder, apiErr := apiPath(param)
	if apiErr != nil {
		return exportRefusal(w, req, apiErr.status, apiErr.message)
	}
	er.folder = folder
	switch dry, _ := q.get("dry"); dry {
	case "", "0":
	case "1":
		er.dry = true
	default:
		return exportRefusal(w, req, http.StatusBadRequest, "dry takes 1 or 0")
	}
	st, err := s.rp.sync()
	if err != nil {
		return err
	}
	er.st = st
	repo, ok := st.repositories.Get(er.name)
	if !ok {
		return exportRefusal(w, req, http.StatusNotFound, "repository "+er.name+" not found")
	}
	er.repo = repo
	access := repos.AccessOf(repo)
	native := formOf[access]
	er.form, _ = q.get("form")
	if er.form == "" {
		er.form = native
	}
	if er.form != native {
		known := false
		for _, f := range formOf {
			known = known || f == er.form
		}
		if !known {
			return exportRefusal(w, req, http.StatusBadRequest, fmt.Sprintf("unknown form %q; the forms are maven, zip, npm and oci", er.form))
		}
		return exportRefusal(w, req, http.StatusBadRequest,
			fmt.Sprintf("the form %s does not fit repository %s of format %s, which exports as %s", er.form, repo.Name, repo.Format, native))
	}
	if er.form == "oci" {
		return s.exportImage(w, req, er)
	}
	kinds := fileAccess[:]
	if access == "npm" {
		kinds = []string{"npm"}
	}
	er.members = filterAccess(st.repositories, repo.Name, kinds...)
	if er.folder != "" {
		isDir, err := s.rp.anyIsDir(er.members, er.folder)
		if err != nil {
			return err
		}
		if !isDir {
			return exportRefusal(w, req, http.StatusNotFound, fmt.Sprintf("%s is no folder of %s", er.folder, repo.Name))
		}
	}
	limits := s.limits()
	entries, tally, err := s.exportListing(er, limits)
	if err != nil {
		return err
	}
	if err := tally.Err(); err != nil {
		var over *export.LimitError
		errors.As(err, &over)
		return exportRefusal(w, req, http.StatusRequestEntityTooLarge, over.Error(),
			[2]any{"files", over.Files}, [2]any{"bytes", over.Bytes}, [2]any{"max_files", over.MaxFiles},
			[2]any{"max_bytes", over.MaxBytes}, [2]any{"complete", over.Complete})
	}
	entries, leftOut := keepArchivable(entries)
	if len(entries) == 0 && len(leftOut) > 0 {
		// the export would hold only paths no archive may hold
		return exportRefusal(w, req, http.StatusConflict, leftOut[0].reason, [2]any{"path", leftOut[0].path})
	}
	if len(entries) == 0 {
		return exportRefusal(w, req, http.StatusNotFound, fmt.Sprintf("%s holds nothing the form %s exports", er.shown(), er.form))
	}
	archive := export.ArchiveName(repo.Name, er.folder)
	if er.dry {
		var total int64
		for _, e := range entries {
			total += e.size
		}
		shown := []any{}
		for _, l := range leftOut[:min(len(leftOut), leftOutShown)] {
			shown = append(shown, l.path)
		}
		return writeExportJSON(w, req, s.withPlaces(req, pyjson.NewObject().Set("repository", repo.Name).Set("path", er.folder).Set("form", er.form).
			Set("name", archive).Set("files", len(entries)).Set("bytes", total).Set("exact", repo.Type != "group").
			Set("max_files", limits.MaxFiles).Set("max_bytes", limits.MaxBytes).Set("left_out", len(leftOut)).Set("left_out_paths", shown)))
	}
	release, ok := s.takeSlot(w, req)
	if !ok {
		return nil
	}
	defer release()
	return s.streamZip(w, req, er, archive, entries, len(leftOut))
}

// leftOutEntry is a listed path the archive leaves out, with why.
type leftOutEntry struct {
	path, reason string
}

// keepArchivable leaves out every entry an unpack cannot hold beside the
// others (calls B19, B20): a name CheckEntryName refuses, a name below a
// file of the archive ("a/b" beside the file "a"; the file stays), and the
// second entry of one name. The rest keep their order; the left out ones
// are in the order of the listing.
func keepArchivable(entries []exportEntry) ([]exportEntry, []leftOutEntry) {
	type out struct {
		at int
		leftOutEntry
	}
	var left []out
	names := map[string]bool{}
	valid := make([]bool, len(entries))
	for i, e := range entries {
		if err := export.CheckEntryName(e.name); err != nil {
			left = append(left, out{i, leftOutEntry{e.asset.Path, err.Error()}})
			continue
		}
		valid[i], names[e.name] = true, true
	}
	var kept []exportEntry
	seen := map[string]bool{}
	for i, e := range entries {
		if !valid[i] {
			continue
		}
		if file := fileAbove(e.name, names); file != "" {
			left = append(left, out{i, leftOutEntry{e.asset.Path, fmt.Sprintf("the entry %q lies below the file %q of the archive", e.name, file)}})
			continue
		}
		if seen[e.name] {
			left = append(left, out{i, leftOutEntry{e.asset.Path, fmt.Sprintf("the entry %q stands twice in the archive", e.name)}})
			continue
		}
		seen[e.name] = true
		kept = append(kept, e)
	}
	sort.SliceStable(left, func(i, j int) bool { return left[i].at < left[j].at })
	leftOut := make([]leftOutEntry, len(left))
	for i, l := range left {
		leftOut[i] = l.leftOutEntry
	}
	return kept, leftOut
}

// fileAbove is the first folder of name that is itself a name of names, or
// "".
func fileAbove(name string, names map[string]bool) string {
	for i := strings.IndexByte(name, '/'); i >= 0; i = nextSlash(name, i) {
		if names[name[:i]] {
			return name[:i]
		}
	}
	return ""
}

func nextSlash(name string, at int) int {
	if j := strings.IndexByte(name[at+1:], '/'); j >= 0 {
		return at + 1 + j
	}
	return -1
}

// exportAccount is the key of the account of a request in running, or ""
// without a subject (security off): the bound of one export per account
// needs an account.
func (s *Server) exportAccount(req *request) string {
	if s.sec == nil {
		return ""
	}
	subject, err := s.subjectOf(req)
	if err != nil || subject == nil {
		return ""
	}
	return subject.Realm + "\x00" + subject.User.ID
}

// takeSlot takes one of the places of an export that streams, or answers
// 429 when both are taken or when the account streams an export already.
// A dry call takes none.
func (s *Server) takeSlot(w *recorder, req *request) (release func(), ok bool) {
	refuse := func(message string) (func(), bool) {
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
		_ = exportRefusal(w, req, http.StatusTooManyRequests, message)
		return nil, false
	}
	account := s.exportAccount(req)
	if account != "" {
		s.exports.mu.Lock()
		if s.exports.running[account] {
			s.exports.mu.Unlock()
			return refuse(fmt.Sprintf("an export of this account streams already: one export at a time per account; try again in %d s", retrySeconds))
		}
		s.exports.running[account] = true
		s.exports.mu.Unlock()
	}
	free := func() {
		if account != "" {
			s.exports.mu.Lock()
			delete(s.exports.running, account)
			s.exports.mu.Unlock()
		}
	}
	select {
	case s.exports.slots <- struct{}{}:
		return func() { <-s.exports.slots; free() }, true
	default:
		free()
		return refuse(fmt.Sprintf("two exports stream already; try again in %d s", retrySeconds))
	}
}

// withPlaces adds to a dry answer whether both places are taken: busy, and
// with it retry_after in seconds. The dry call takes no place and answers
// 200 (call B23); a download that met 429 would fail in the browser where
// no page can show why, so the dialog shows the wait before it starts.
func (s *Server) withPlaces(req *request, body *pyjson.Object) *pyjson.Object {
	own := false
	if account := s.exportAccount(req); account != "" {
		s.exports.mu.Lock()
		own = s.exports.running[account]
		s.exports.mu.Unlock()
	}
	busy := own || len(s.exports.slots) == cap(s.exports.slots)
	body.Set("busy", busy)
	body.Set("own_export", own)
	if busy {
		body.Set("retry_after", retrySeconds)
	}
	return body
}

func (er *exportRequest) shown() string {
	if er.folder == "" {
		return "the root of " + er.name
	}
	return er.folder
}

func writeExportJSON(w http.ResponseWriter, req *request, body *pyjson.Object) error {
	data, err := pyjson.ResponseBody(body)
	if err != nil {
		return err
	}
	writeBody(w, req, http.StatusOK, exportHeaders, data)
	return nil
}

// exportListing lists the entries of the form below the folder, over every
// member, the first member winning a path, in the order of parent and name.
// The count stops at the first entry after a limit was passed.
func (s *Server) exportListing(er *exportRequest, limits export.Limits) ([]exportEntry, *export.Tally, error) {
	tally := export.NewTally(limits)
	seen := map[string]bool{}
	var entries []exportEntry
	stopped := false
	// an npm export holds tarballs only: the index leaves out every other
	// row, so package documents neither cost memory nor pass the limit
	ending := ""
	if er.form == "npm" {
		ending = ".tgz"
	}
	for _, member := range er.members {
		if stopped {
			break
		}
		err := s.rp.index.WalkPrefixEnding(member.Name, er.folder, ending, func(a index.Asset) error {
			name, ok := er.entryName(a.Path)
			if !ok || seen[a.Path] {
				return nil
			}
			seen[a.Path] = true
			if !tally.Add(a.Size) {
				stopped = true
				return index.ErrStop
			}
			entries = append(entries, exportEntry{name: name, asset: a, size: a.Size})
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	if merges(er.repo) && !stopped {
		var err error
		if entries, tally, err = s.countMerges(er, entries, limits); err != nil {
			return nil, nil, err
		}
	}
	if len(er.members) > 1 {
		sort.SliceStable(entries, func(i, j int) bool {
			pi, ni := splitPath(entries[i].asset.Path)
			pj, nj := splitPath(entries[j].asset.Path)
			if pi != pj {
				return pi < pj
			}
			return ni < nj
		})
	}
	return entries, tally, nil
}

// merges is true for a group whose read path merges maven-metadata.xml.
func merges(repo repos.Repo) bool { return repo.Type == "group" && repo.Format == "maven2" }

// countMerges counts each merged maven-metadata.xml and its checksums with
// the size of the bytes the group answers, and leaves out such a path the
// group answers no bytes for. The tally starts again over the entries. A
// listing that stopped at a limit keeps its count, which refuses it.
func (s *Server) countMerges(er *exportRequest, entries []exportEntry, limits export.Limits) ([]exportEntry, *export.Tally, error) {
	tally := export.NewTally(limits)
	kept := entries[:0]
	for _, e := range entries {
		if base, _ := maven.SplitChecksum(e.asset.Path); maven.IsMetadataPath(base) {
			b, err := s.rp.file(er.st, er.repo, er.members, e.asset.Path)
			if err != nil {
				return nil, nil, err
			}
			if b == nil {
				continue
			}
			b.close()
			e.size = b.size
		}
		kept = append(kept, e)
		tally.Add(e.size)
	}
	return kept, tally, nil
}

func splitPath(p string) (parent, name string) {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}

// entryName is the name of a stored path in the archive of the form; false
// leaves the path out.
func (er *exportRequest) entryName(path string) (string, bool) {
	switch er.form {
	case "maven":
		return path, true
	case "npm":
		return export.NpmEntryName(path)
	default:
		if er.folder == "" {
			return path, true
		}
		return strings.TrimPrefix(path, er.folder+"/"), true
	}
}

// streamHeaders start the answer of an archive and send the head at once,
// so that net/http frames the body as a stream (chunked for HTTP/1.1, up to
// the close for HTTP/1.0) and never with a Content-Length it computed from a
// short archive.
func streamHeaders(w *recorder, contentType, name string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Del("Content-Length")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// streamZip writes the archive. An entry that cannot be read ends it early,
// without its central directory, and breaks the transfer; the server log
// names why.
func (s *Server) streamZip(w *recorder, req *request, er *exportRequest, archive string, entries []exportEntry, leftOut int) error {
	started := time.Now()
	list := make([]export.Entry, len(entries))
	var written int64
	skipped := 0
	for i, e := range entries {
		path, counted := e.asset.Path, e.size
		list[i] = export.Entry{Name: e.name, Size: e.size, ModifiedMS: e.asset.CreatedMS,
			Open: func() (io.ReadCloser, int64, error) {
				if err := req.r.Context().Err(); err != nil {
					return nil, 0, fmt.Errorf("the client left: %w", err)
				}
				b, err := s.rp.file(er.st, er.repo, er.members, path)
				if err != nil {
					return nil, 0, err
				}
				if b == nil && er.repo.Type == "group" {
					// a GET of the path through the group answers 404, as for a
					// maven-metadata.xml whose merge reads no document
					logAt(slogWarn, serverLogger, "export of %q leaves out %q: the group answers no bytes for it", er.repo.Name, path)
					skipped++
					return nil, 0, export.ErrSkip
				}
				if b == nil {
					return nil, 0, fmt.Errorf("%s is gone from %s", path, er.repo.Name)
				}
				if b.size > counted {
					// the store grew after the listing: no entry sends more
					// than the limits counted for it
					b.close()
					return nil, 0, &export.SizeError{Name: path, Want: counted, Read: b.size}
				}
				written += b.size
				if b.file != nil {
					return b.file, b.size, nil
				}
				return io.NopCloser(bytes.NewReader(b.data)), b.size, nil
			}}
	}
	streamHeaders(w, "application/zip", archive)
	out, done := s.stallBound(w)
	err := export.WriteZip(out, list)
	done()
	s.exportLog(er, len(entries)-skipped, written, leftOut, started, err)
	endStream(err)
	return nil
}

// stallBound is the writer of an archive: each write may wait the stall
// time for the client to read, so a client that stops reading ends its
// export and frees its place instead of holding it without end. The server
// has no write timeout, which would cut a long answer to a client that
// reads. done clears the deadline for the next request of the connection.
// The whole export may run for the time limit; a write after it, or one
// that the limit cuts, ends the export as any failure after the head.
func (s *Server) stallBound(w *recorder) (io.Writer, func()) {
	rc := http.NewResponseController(w.ResponseWriter)
	limit := s.exports.limit
	return &stallWriter{w: w, rc: rc, stall: s.exports.stall, limit: limit, end: time.Now().Add(limit)},
		func() { _ = rc.SetWriteDeadline(time.Time{}) }
}

type stallWriter struct {
	w     io.Writer
	rc    *http.ResponseController
	stall time.Duration
	limit time.Duration
	end   time.Time
}

func (sw *stallWriter) Write(p []byte) (int, error) {
	now := time.Now()
	if !now.Before(sw.end) {
		return 0, fmt.Errorf("the export ran past its time limit of %s", sw.limit)
	}
	deadline := now.Add(sw.stall)
	if sw.end.Before(deadline) {
		deadline = sw.end
	}
	if err := sw.rc.SetWriteDeadline(deadline); err != nil {
		return 0, err
	}
	n, err := sw.w.Write(p)
	if err != nil && !time.Now().Before(sw.end) {
		err = fmt.Errorf("the export ran past its time limit of %s: %w", sw.limit, err)
	}
	return n, err
}

// endStream breaks the transfer of an archive that ended early. The head
// went out with 200, so only the end of the body can tell the client:
// net/http closes the connection without the last chunk, and curl or a
// browser reports a failed download instead of keeping a short archive.
func endStream(err error) {
	if err != nil {
		panic(http.ErrAbortHandler)
	}
}

// exportLog is the server log line at the end of an export (call B27); it
// counts the paths the archive left out.
func (s *Server) exportLog(er *exportRequest, files int, bytes int64, leftOut int, started time.Time, err error) {
	path := er.folder
	if path == "" {
		path = "/"
	}
	ms := time.Since(started).Milliseconds()
	if err != nil {
		logAt(slogWarn, serverLogger, "export of %q path %q form %q ended early after %d ms: %q",
			er.repo.Name, path, er.form, ms, PyText(err))
		return
	}
	left := ""
	if leftOut > 0 {
		left = fmt.Sprintf(", %d left out", leftOut)
	}
	logAt(slogInfo, serverLogger, "export of %q path %q form %q: %d files, %d bytes%s, %d ms", er.repo.Name, path, er.form, files, bytes, left, ms)
}
