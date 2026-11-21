package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/policy"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/rest"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The ingest (seam Ingest of the phase 4 execution plan): every write
// handler, the REST upload and the proxy fetch of phase 5 hand their body
// to (*Server).ingest, and every delete goes through (*Server).erase. No
// other code writes a blob. The order of one upload is the one of section
// Layout of the phase 4 plan: policy, free space, stream into the work file
// with the four digests, size and digests, the lock of repository and path,
// the policy again with the right of a caller who holds add only, the
// commit, the row, the catalog hook, the delete mark on a replaced blob, the
// release.

// CatalogHook keeps the side data of one asset row (seam Catalog hook,
// decision E5): every write calls Put once its row is in the index, every
// delete calls Delete once its row is gone. Nil does nothing; serve sets
// the hook of internal/catalog (catalog_hook.go, leg F1b). An error of the hook is logged and fails nothing:
// the catalog is side data that a scan of the store fills again.
type CatalogHook interface {
	Put(asset index.Asset, digests stores.Digests, uploader, address string) error
	Delete(repo, path string) error
}

// The rules of a refused write besides those of internal/policy, which
// keep their names (read-only, redeploy, version, layout, content).
const (
	ruleTooLarge      = "too-large"       // 413: the body passes the limit of the repository (W14)
	ruleNoSpace       = "no-space"        // 507: the write store has less free space than the body and the floor (W14)
	ruleSize          = "size"            // 400: the body is not the length the request declares
	ruleDigest        = "digest"          // 400: a digest of the body differs from the one the request gives
	ruleReadOnlyStore = "read-only-store" // 409: a store of the old product holds the path
	ruleWriteStore    = "write-store"     // 409: the repository names a store this server does not write
	ruleEdit          = "edit"            // 403: the path exists under its lock, and the caller holds add, not edit
	ruleAdd           = "add"             // 403: the path is gone under its lock, and the caller holds edit, not add
	ruleBusy          = "busy"            // 503: the index stayed busy longer than the write may wait for it
)

// mayEdit reports a caller who holds edit of the view permission of a
// repository, which a write over a path the repository holds asks.
func (s *Server) mayEdit(subject *model.Subject, repo repos.Repo) bool {
	return s.sec == nil || s.sec.Permits(subject, rest.ViewPermission(repo.Format, repo.Name, "edit"))
}

// editRefused is the 403 of a write by a caller who holds add and not edit,
// of a path that exists under the lock of the write: the route asked add
// before the body, from the rows of that moment (finding S3 of review 4).
func editRefused() error {
	return writeRefused(http.StatusForbidden, ruleEdit, "%s", repositoryRefusal)
}

// mayAdd reports a caller who holds add of the view permission of a
// repository, which a write of a path the repository does not hold asks.
func (s *Server) mayAdd(subject *model.Subject, repo repos.Repo) bool {
	return s.sec == nil || s.sec.Permits(subject, rest.ViewPermission(repo.Format, repo.Name, "add"))
}

// addRefused is the 403 of a write by a caller who holds edit and not add,
// of a path that a delete removed before the lock of the write: the route
// asked edit before the body, from the row of that moment.
func addRefused() error {
	return writeRefused(http.StatusForbidden, ruleAdd, "%s", repositoryRefusal)
}

// heldAction refuses a write whose path, as the lock finds it, asks an
// action the caller does not hold: edit for a path that exists, add for one
// that does not.
func heldAction(row *index.Asset, addOnly, editOnly bool) error {
	if addOnly && row != nil {
		return editRefused()
	}
	if editOnly && row == nil {
		return addRefused()
	}
	return nil
}

// A write holds the lock of its path while it waits for the write lock of
// the index, and a scan holds that lock for the whole walk of its store, in
// this process or another. The wait is bounded (finding S6 of review 4):
// past it the write answers 503 with Retry-After. The blob is placed only
// under the write lock of the index (index.PutAfter), so a busy index leaves
// no blob and no work file, and no scan meets a blob whose row may not
// follow.
const (
	// defaultIndexWait is the bound. The write of another request holds the
	// index for milliseconds, a scan for seconds to minutes: 10 s lets the
	// first through under any load this server has seen, and gives a client
	// an answer to retry long before its own time limit.
	defaultIndexWait = 10 * time.Second
	// busyRetryAfter is the Retry-After of that 503, in seconds.
	busyRetryAfter = "10"
)

// indexWait replaces defaultIndexWait while it is above zero, in
// nanoseconds; tests shorten the wait with it.
var indexWait atomic.Int64

func indexWaitOf() time.Duration {
	if d := time.Duration(indexWait.Load()); d > 0 {
		return d
	}
	return defaultIndexWait
}

// busyRefused is the 503 of a write that waited indexWaitOf for the index.
func busyRefused() error {
	return writeRefused(http.StatusServiceUnavailable, ruleBusy, "the index is busy, a scan may hold it; nothing is stored, send the write again")
}

// retryAfter gives the answer to a write that waited for a busy index its
// Retry-After.
func retryAfter(w http.ResponseWriter, refusal *writeRefusal) {
	if refusal.rule == ruleBusy {
		w.Header().Set("Retry-After", busyRetryAfter)
	}
}

// The reasons of the delete marks this server writes.
const (
	reasonReplaced = "Replaced by a newer blob of the same path"
	reasonDeleted  = "Deleted through the write path"
	reasonNoRow    = "The index did not take the blob"
)

// writeRefusal is a write that a rule or a limit refuses, with the status
// of section Refusals of the phase 4 plan (provisional, W6). The handler of
// a format answers it in the shape of its protocol; rule names the rule, so
// that the Docker handler can give the codes of the registry protocol.
type writeRefusal struct {
	status  int
	rule    string
	message string
}

func (e *writeRefusal) Error() string { return e.message }

func writeRefused(status int, rule, format string, args ...any) error {
	return &writeRefusal{status: status, rule: rule, message: fmt.Sprintf(format, args...)}
}

// refusalOf gives a refusal of internal/policy the status 400 of the
// Refusals table.
func refusalOf(err error) error {
	var r *policy.Refusal
	if errors.As(err, &r) {
		return &writeRefusal{status: http.StatusBadRequest, rule: string(r.Rule), message: r.Message}
	}
	return err
}

// ingestRequest is one blob for the write path.
type ingestRequest struct {
	repo        repos.Repo        // the hosted repository, with its stored attributes
	path        string            // the asset path, without a leading slash
	body        io.Reader         // read once, to its end or to the limit; never held in memory
	size        int64             // the length the request declares, -1 when it declares none
	digests     map[string]string // the digests the request gives by algorithm (md5, sha1, sha256, sha512), hex
	contentType string            // the content type to store; "" takes policy.ContentType
	exempt      bool              // the write policy lets the path through again, beside the exemptions of policy
	fetch       bool              // a fetch of a proxy (ruling P5-3 of the phase 5 session): no write policy applies, so a stale path is replaced
	addOnly     bool              // the caller holds add and not edit: a path that exists under the lock is refused (editRefused)
	editOnly    bool              // the caller holds edit and not add: a path that is gone under the lock is refused (addRefused)
	subject     *model.Subject    // the caller; nil for the anonymous user
	address     string            // the address of the client, stored as created-by-ip
}

// ingestResult is a blob the write path wrote.
type ingestResult struct {
	asset    index.Asset    // the new row
	replaced *index.Asset   // the row of the write store it replaced, whose blob now carries the delete mark; nil for a new path
	digests  stores.Digests // the four digests of the body
}

// pathLocks let one write of a path of a repository run at a time: the
// second policy check, the commit, the row and the delete mark of the
// replaced blob run under the lock.
type pathLocks struct {
	mu   sync.Mutex
	held map[[2]string]*pathLock
}

type pathLock struct {
	mu    sync.Mutex
	users int
}

func (p *pathLocks) lock(repo, path string) (unlock func()) {
	key := [2]string{repo, path}
	p.mu.Lock()
	if p.held == nil {
		p.held = map[[2]string]*pathLock{}
	}
	l := p.held[key]
	if l == nil {
		l = &pathLock{}
		p.held[key] = l
	}
	l.users++
	p.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		p.mu.Lock()
		if l.users--; l.users == 0 {
			delete(p.held, key)
		}
		p.mu.Unlock()
	}
}

// freeBytes is the space a file system gives an unprivileged writer.
func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil // #nosec G115 -- the block size statfs gives is positive.
}

// uploader is the user a blob names as created-by.
func (s *Server) uploader(subject *model.Subject) string {
	if subject != nil {
		return subject.User.ID
	}
	if snap := s.sec.Snapshot(); snap != nil && snap.Anonymous.UserID != "" {
		return snap.Anonymous.UserID
	}
	return "anonymous"
}

// writeSettingsOf are the write settings of a repository, refused when they
// name a store this server does not write, and the writer of the store
// they name (SR2): local-write, or a created store, opened on first use.
func (s *Server) writeSettingsOf(repo repos.Repo) (repos.WriteSettings, *blobwrite.Writer, error) {
	settings := repo.WriteSettings()
	w, err := s.writes.writerOf(settings.WriteStore)
	if errors.Is(err, errNoWriteStore) {
		return settings, nil, writeRefused(http.StatusConflict, ruleWriteStore,
			"repository %s writes into store %s, which this server does not write; it writes into %s and the stores created below its folder stores",
			repo.Name, settings.WriteStore, s.writes.writer.Name())
	}
	if err != nil {
		return settings, nil, writeRefused(http.StatusConflict, ruleWriteStore, "repository %s writes into store %s, which does not open for writes: %v",
			repo.Name, settings.WriteStore, err)
	}
	return settings, w, nil
}

// checkWrite applies the policies to the path as the index holds it now,
// and returns its winning row.
func (s *Server) checkWrite(in *ingestRequest, settings repos.WriteSettings, exempt bool) (*index.Asset, error) {
	row, err := s.rp.index.Lookup(in.repo.Name, in.path)
	if err != nil {
		return nil, err
	}
	if err := policy.Write(settings.WritePolicy, row != nil, exempt); err != nil && !in.fetch {
		return nil, refusalOf(err)
	}
	if in.repo.Format == "maven2" {
		if err := policy.Version(settings.VersionPolicy, in.path); err != nil {
			return nil, refusalOf(err)
		}
		if err := policy.Layout(settings.LayoutPolicy, in.path); err != nil {
			return nil, refusalOf(err)
		}
	}
	return row, nil
}

// bounds are the creation times a new blob of a path keeps to, as
// stores.Attributes takes them. NewerThan is the newest row of the path in
// a store this server does not write: the new blob must win over such a
// row, which stays as it is (W4), also when the row it replaces lies in the
// write store. NotBefore is the newest blob of the path in the write store,
// the blob of its row or one a delete of the path marked in this process:
// the scan of the write store orders the blobs of a path by creationTime,
// so a clock that steps back must not put the new blob behind it (finding
// S5 of review 4). An equal time stays: the reconciliation log orders the
// blobs of one millisecond.
func (s *Server) bounds(repo, path, store string, a stores.Attributes) (stores.Attributes, error) {
	rows, err := s.rp.index.Rows(repo, path)
	if err != nil {
		return a, err
	}
	a.NotBefore = s.writes.deletedTime(repo, path)
	for _, row := range rows {
		if row.Store == store {
			a.NotBefore = max(a.NotBefore, row.CreatedMS)
		} else {
			a.NewerThan = max(a.NewerThan, row.CreatedMS)
		}
	}
	return a, nil
}

// checkSpace refuses a body the write store store has no room for: the floor of
// the repository stays free, and so do the bytes that the other bodies in
// flight hold (spaceHeld). It returns the room above them, which bounds the
// stream of a body that declares no length (storeRoom), and holds a
// declared length until the room is released.
func (s *Server) checkSpace(store string, size, floor int64) (*storeRoom, error) {
	w, err := s.writes.writerOf(store)
	if err != nil {
		return nil, err
	}
	free, err := s.writes.freeSpace(w.Root())
	if err != nil {
		return nil, err
	}
	floor = max(floor, 0)
	size = max(size, 0)
	held := &s.writes.space
	held.mu.Lock()
	defer held.mu.Unlock()
	need := uint64(floor) + uint64(size) + uint64(held.bytes) // #nosec G115 -- all three are at least zero.
	if free < need {
		return nil, writeRefused(http.StatusInsufficientStorage, ruleNoSpace, "store %s has %d bytes free; the write needs %d with the free space it must leave",
			w.Name(), free, need)
	}
	limit := int64(math.MaxInt64)
	if free-uint64(floor) < uint64(math.MaxInt64) { // #nosec G115 -- floor is at least zero and at most free here.
		limit = int64(free - uint64(floor)) // #nosec G115 -- below MaxInt64, as just checked.
	}
	left := limit
	if limit < math.MaxInt64 {
		left -= held.bytes
	}
	held.bytes += size
	return &storeRoom{left: left, limit: limit, held: held, reserved: size, writer: w, refusal: writeRefused(http.StatusInsufficientStorage, ruleNoSpace,
		"the body passes the %d bytes that store %s has free above the %d bytes a write must leave", left, w.Name(), floor)}, nil
}

// spaceHeld counts the bytes that the bodies in flight hold of the free
// space of the write store: each its declared length, and what a body
// without one has read so far. The free space is measured once per body, so
// without it bodies that run at the same time would each see the same room
// (row Bodies in parallel of the 0.2.0 plan). A body holds its bytes until
// it ends, so the bytes it wrote count twice meanwhile, once measured and
// once held: a refusal may come early, never late.
type spaceHeld struct {
	mu    sync.Mutex
	bytes int64
}

// inFlight is the bytes held now.
func (h *spaceHeld) inFlight() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bytes
}

// storeRoom is what a body may still bring into the write store: the room
// above the floor of its repository and the bytes of the other bodies in
// flight, measured once before the body (W14). A body without a declared
// length passed checkSpace with no size to add to the floor, so its stream
// stops with the refusal 507 once it passes the room, or once all bodies in
// flight together pass the room above the floor (finding S2 and assessment
// A5 of review 4). The holder of a room releases it once the body is
// written or refused.
type storeRoom struct {
	left     int64      // the bytes this body may still read
	limit    int64      // the room above the floor, as measured, which the bodies in flight share
	held     *spaceHeld // the bytes of the bodies in flight; nil once released, and for a room of its own
	reserved int64      // the bytes this body holds in held
	taken    int64      // the bytes this body read
	refusal  error
	writer   *blobwrite.Writer // the writer of the store the room is of; work files of the body go into its content/tmp (review R3)
}

// take counts n more bytes of the body; past the room it is the refusal.
func (r *storeRoom) take(n int64) error {
	r.left -= n
	r.taken += n
	if r.held != nil && r.taken > r.reserved {
		r.held.mu.Lock()
		r.held.bytes += r.taken - r.reserved
		over := r.held.bytes > r.limit
		r.held.mu.Unlock()
		r.reserved = r.taken
		if over {
			r.left = min(r.left, -1)
		}
	}
	if r.left < 0 {
		return r.refusal
	}
	return nil
}

// release gives the bytes the body holds back; a second call does nothing.
func (r *storeRoom) release() {
	if r == nil || r.held == nil {
		return
	}
	r.held.mu.Lock()
	r.held.bytes -= r.reserved
	r.held.mu.Unlock()
	r.held, r.reserved = nil, 0
}

// reader is a body whose read fails with the refusal of the room once the
// body passes it; at most one byte beyond the room is read.
func (r *storeRoom) reader(body io.Reader) io.Reader { return &roomReader{body: body, room: r} }

type roomReader struct {
	body io.Reader
	room *storeRoom
}

func (b *roomReader) Read(p []byte) (int, error) {
	if b.room.left < 0 {
		return 0, b.room.refusal
	}
	if left := b.room.left; left < math.MaxInt64 && int64(len(p)) > left+1 {
		p = p[:left+1]
	}
	n, err := b.body.Read(p)
	if refused := b.room.take(int64(n)); refused != nil {
		return 0, refused
	}
	return n, err
}

// checkDigests compares the digests a request gives with those of the body.
func checkDigests(given map[string]string, got stores.Digests) error {
	for algorithm, value := range given {
		var actual string
		switch strings.ToLower(algorithm) {
		case "md5":
			actual = got.MD5
		case "sha1":
			actual = got.SHA1
		case "sha256":
			actual = got.SHA256
		case "sha512":
			actual = got.SHA512
		default:
			return fmt.Errorf("unknown digest algorithm %q", algorithm)
		}
		if !strings.EqualFold(value, actual) {
			return writeRefused(http.StatusBadRequest, ruleDigest, "the %s digest of the body is %s, the request gives %s", algorithm, actual, value)
		}
	}
	return nil
}

// contentChecked are the formats of the strict content type validation in
// section Refusals of the phase 4 plan.
func contentChecked(format string) bool {
	return format == "maven2" || format == "raw" || format == "npm"
}

// replacedBlob is the row a Put of a new blob replaced, nil when that row
// names the new blob itself: a scan of the write store that ran while the
// write waited for the index met the new blob and wrote its row first.
func replacedBlob(replaced *index.Asset, written stores.Entry) *index.Asset {
	if replaced != nil && replaced.Location == written.Location {
		return nil
	}
	return replaced
}

// rowRefused is the error of a blob that PutAfter did not give a row. A
// busy index is the 503 of busyRefused: the blob was never placed, so no
// scan met it. A blob that was placed gets its delete mark: a live blob
// without its row would come back with the next scan, although the client
// was told the write failed.
func (wp *writePath) rowRefused(store string, written stores.Written, err error) error {
	if written.ID == "" {
		if errors.Is(err, index.ErrBusy) {
			return busyRefused()
		}
		return err
	}
	return errors.Join(err, wp.mark(store, written.Entry.Location, reasonNoRow))
}

func assetOf(store string, e stores.Entry) index.Asset {
	return index.Asset{Repo: e.Repo, Path: e.Path, Store: store, Location: e.Location, Size: e.Size, SHA1: e.SHA1,
		ContentType: e.ContentType, CreatedMS: e.CreatedMS}
}

// ingest writes one blob into the write store and its row into the index.
// A refusal is a *writeRefusal; nothing of a refused body stays: a refusal
// of a policy, of the limit or of the free space comes before a byte is
// written, a refusal of size or digest removes the work file.
func (s *Server) ingest(in ingestRequest) (*ingestResult, error) {
	if s.writes.writer == nil {
		return nil, errors.New("writes are off")
	}
	settings, writer, err := s.writeSettingsOf(in.repo)
	if err != nil {
		return nil, err
	}
	exempt := in.exempt || in.repo.Format == "maven2" && policy.MavenMetadata(in.path) ||
		in.repo.Format == "docker" && settings.LatestRedeploy && policy.LatestTag(in.path)
	if _, err := s.checkWrite(&in, settings, exempt); err != nil {
		return nil, err
	}
	tooLarge := func() error {
		return writeRefused(http.StatusRequestEntityTooLarge, ruleTooLarge, "the body is larger than the limit of %d bytes of repository %s",
			settings.MaxBodyBytes, in.repo.Name)
	}
	if in.size > settings.MaxBodyBytes {
		return nil, tooLarge()
	}
	room, err := s.checkSpace(writer.Name(), in.size, settings.FreeSpaceFloor)
	if err != nil {
		return nil, err
	}
	defer room.release()
	body := bufio.NewReaderSize(room.reader(in.body), policy.HeadBytes)
	head, err := body.Peek(policy.HeadBytes)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, err
	}
	if settings.StrictContentType && contentChecked(in.repo.Format) {
		if err := policy.Content(in.path, head); err != nil {
			return nil, refusalOf(err)
		}
	}
	contentType := in.contentType
	if contentType == "" {
		contentType = policy.ContentType(in.path, head)
	}
	upload, err := writer.Upload(body, settings.MaxBodyBytes)
	if errors.Is(err, stores.ErrTooLarge) {
		return nil, tooLarge()
	}
	if err != nil {
		return nil, err
	}
	defer upload.Discard()
	if in.size >= 0 && upload.Size() != in.size {
		return nil, writeRefused(http.StatusBadRequest, ruleSize, "the body has %d bytes, the request declares %d", upload.Size(), in.size)
	}
	if err := checkDigests(in.digests, upload.Digests()); err != nil {
		return nil, err
	}
	unlock := s.writes.locks.lock(in.repo.Name, in.path)
	defer unlock()
	row, err := s.checkWrite(&in, settings, exempt)
	if err != nil {
		return nil, err
	}
	if err := heldAction(row, in.addOnly, in.editOnly); err != nil {
		return nil, err
	}
	uploader := s.uploader(in.subject)
	attributes, err := s.bounds(in.repo.Name, in.path, writer.Name(), stores.Attributes{Repo: in.repo.Name, Path: in.path, ContentType: contentType,
		CreatedBy: uploader, CreatedByIP: in.address})
	if err != nil {
		return nil, err
	}
	// The sync of the properties file goes before the wait for the index, so
	// that the write lock of the index covers the renames and the syncs of
	// their directory only (the drop of the small write rate, 0.2.0).
	if err := upload.Prepare(attributes); err != nil {
		return nil, err
	}
	var written stores.Written
	replaced, err := s.rp.index.PutAfter(writer.Name(), indexWaitOf(), func() (stores.Entry, error) {
		var err error
		written, err = upload.Commit(attributes)
		return written.Entry, err
	})
	if err != nil {
		return nil, s.writes.rowRefused(writer.Name(), written, err)
	}
	replaced = replacedBlob(replaced, written.Entry)
	asset := assetOf(writer.Name(), written.Entry)
	if s.writes.catalog != nil {
		if err := s.writes.catalog.Put(asset, written.Digests, uploader, in.address); err != nil {
			logAt(slogWarn, serverLogger, "the catalog row of %s/%s is not written: %v", in.repo.Name, in.path, err)
		}
	}
	if replaced != nil {
		s.writes.markReplaced(in.repo.Name, in.path, writer.Name(), replaced.Location)
	}
	s.writes.retryPath(in.repo.Name, in.path)
	s.writes.forgetDeleted(in.repo.Name, in.path)
	return &ingestResult{asset: asset, replaced: replaced, digests: written.Digests}, nil
}

// erase is the delete of every write handler: the delete mark on the blob
// of the write store (W5), its row out of the index and the catalog hook.
// The write policy DENY refuses it as a write. A path that a store of the
// old product holds is refused with 409 (section Refusals), also when a
// newer blob of the write store covers it: the old store is never written,
// and the path would be served from it again. It returns the removed row,
// nil when no store holds the path.
func (s *Server) erase(repo repos.Repo, path string) (*index.Asset, error) {
	if s.writes.writer == nil {
		return nil, errors.New("writes are off")
	}
	settings, _, err := s.writeSettingsOf(repo)
	if err != nil {
		return nil, err
	}
	if err := policy.Write(settings.WritePolicy, false, false); err != nil {
		return nil, refusalOf(err)
	}
	unlock := s.writes.locks.lock(repo.Name, path)
	defer unlock()
	// The index holds one row of a path per store, so the write store holds
	// one row of it; a blob a write replaced whose mark failed is live beside
	// it, and gets its mark here, so that no scan brings the path back
	// (risk 4 of the 4a report, marks.go).
	s.writes.retryPath(repo.Name, path)
	rows, err := s.rp.index.Rows(repo.Name, path)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	for _, row := range rows {
		if !s.writes.isWriteStore(row.Store) {
			return nil, writeRefused(http.StatusConflict, ruleReadOnlyStore, "%s of repository %s lies in store %s, which is read only",
				path, repo.Name, row.Store)
		}
	}
	// The mark comes once the index takes the delete, so that a busy index
	// leaves the blob as it was.
	marked := false
	removed, err := s.rp.index.DeleteWithin(repo.Name, path, rows[0].Store, indexWaitOf(), func() error {
		if err := s.writes.mark(rows[0].Store, rows[0].Location, reasonDeleted); err != nil {
			return err
		}
		s.writes.noteDeleted(repo.Name, path, rows[0].CreatedMS)
		marked = true
		return nil
	})
	if errors.Is(err, index.ErrBusy) {
		return nil, busyRefused()
	}
	if err != nil {
		return nil, err
	}
	if removed == nil && marked {
		// A scan of the write store took the row away while the delete
		// waited for the index (note N5 of the 0.2.0 plan); the blob carries
		// the mark now, so the delete took place.
		removed = &rows[0]
	}
	if s.writes.catalog != nil {
		if err := s.writes.catalog.Delete(repo.Name, path); err != nil {
			logAt(slogWarn, serverLogger, "the catalog row of %s/%s is not removed: %v", repo.Name, path, err)
		}
	}
	return removed, nil
}
