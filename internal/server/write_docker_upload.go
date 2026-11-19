package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/docker"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/rest"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The blob uploads of the registry protocol (task 13 of the phase 4 plan;
// OCI distribution spec v1.1.1, sections Pushing blobs and Mounting a blob
// from another repository, read 2026-11-18). A session lives in the memory
// of the process (W3): its id, its repository and name, the user who
// started it, and the bytes the chunks brought in a work file of
// content/tmp of the write store, named as the old product names its work
// files, so that a start of the server removes it. The closing PUT hands
// the work file and the last chunk to ingest, which writes the blob
// v2/-/blobs/<digest> of the repository.

const (
	// maxUploadSessions is the most sessions open at once in one process.
	maxUploadSessions = 256
	// maxUserSessions is the most sessions one user holds at once
	// (assessment A4 of review 4): a quarter of the process, so that a user
	// who opens sessions and leaves them open leaves room for the pushes of
	// the others. A client pushes the layers of an image a few at a time, so
	// one user still has room for many pushes at once.
	maxUserSessions = 64
	// uploadIdle is the time after which a session without a request ends
	// (W3).
	uploadIdle = time.Hour
	// copyBufferBytes is the buffer of every copy of a body.
	copyBufferBytes = 256 * 1024
)

// uploadSession is one upload in progress.
type uploadSession struct {
	mu       sync.Mutex // one request of the session at a time
	id       string
	repo     string
	name     string
	owner    string // the user who started it; no other user sees it
	work     string // the work file, "" while the session holds no byte
	size     int64
	lastUsed time.Time
	inUse    int  // requests that hold or wait for the session; guarded by ociWrites.mu
	ended    bool // guarded by ociWrites.mu
}

// newUUID is a random lower case UUID of version 4.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read does not fail
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// workFileIn is a new work file name in content/tmp of a store root:
// tmp$<id>.<uuid>.bytes, the name a start of the writer removes (W2).
func workFileIn(root, id string) string {
	return filepath.Join(root, stores.ContentDir, blobwrite.TmpDir, "tmp$"+id+"."+newUUID()+stores.BytesSuffix)
}

// sweep ends every session without a request for uploadIdle; o.mu is held.
func (o *ociWrites) sweep(now time.Time) {
	for id, sess := range o.sessions {
		if sess.inUse == 0 && now.Sub(sess.lastUsed) >= uploadIdle {
			o.endLocked(id, sess)
		}
	}
}

func (o *ociWrites) endLocked(id string, sess *uploadSession) {
	delete(o.sessions, id)
	sess.ended = true
	if sess.work != "" {
		if err := os.Remove(sess.work); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logAt(slogWarn, serverLogger, "the work file of upload %s is not removed: %v", id, err)
		}
	}
}

// endAll ends every session and removes its work file, at the stop of the
// server, when no request runs any more.
func (o *ociWrites) endAll() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, sess := range o.sessions {
		o.endLocked(id, sess)
	}
}

// open starts a session; nil with the bound that refuses it while
// maxUploadSessions are open, or maxUserSessions of the owner.
func (o *ociWrites) open(repo, name, owner string) (*uploadSession, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.clock()
	o.sweep(now)
	if len(o.sessions) >= maxUploadSessions {
		return nil, maxUploadSessions
	}
	held := 0
	for _, sess := range o.sessions {
		if sess.owner == owner {
			held++
		}
	}
	if held >= maxUserSessions {
		return nil, maxUserSessions
	}
	if o.sessions == nil {
		o.sessions = map[string]*uploadSession{}
	}
	sess := &uploadSession{id: newUUID(), repo: repo, name: name, owner: owner, lastUsed: now}
	o.sessions[sess.id] = sess
	return sess, 0
}

// take is the session of an id for a request of its repository, name and
// owner, locked; nil when there is none. release gives it back.
func (o *ociWrites) take(id, repo, name, owner string) *uploadSession {
	o.mu.Lock()
	o.sweep(o.clock())
	sess := o.sessions[id]
	if sess == nil || sess.repo != repo || sess.name != name || sess.owner != owner {
		o.mu.Unlock()
		return nil
	}
	sess.inUse++
	o.mu.Unlock()
	sess.mu.Lock()
	o.mu.Lock()
	defer o.mu.Unlock()
	if sess.ended {
		sess.inUse--
		sess.mu.Unlock()
		return nil
	}
	return sess
}

// release gives a session back after a request; end ends it as well.
func (o *ociWrites) release(sess *uploadSession, end bool) {
	o.mu.Lock()
	sess.inUse--
	sess.lastUsed = o.clock()
	if end && !sess.ended {
		o.endLocked(sess.id, sess)
	}
	o.mu.Unlock()
	sess.mu.Unlock()
}

// count is the number of open sessions, for tests.
func (o *ociWrites) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.sessions)
}

// dockerBase is the prefix of a registry URL of an answer: /v2 on a Docker
// port, /repository/<repo>/v2 on the main port.
func dockerBase(req *request, repo repos.Repo) string {
	if req.dockerRepo != "" {
		return "/v2"
	}
	return repositoryPrefix + quote(repo.Name, "") + "/v2"
}

// answerSession is the answer of a session that goes on: status 202 for a
// start or a chunk, 204 for a status request.
func answerSession(w http.ResponseWriter, req *request, repo repos.Repo, sess *uploadSession, status int) {
	headers := [][2]string{{"Location", dockerBase(req, repo) + "/" + sess.name + "/blobs/uploads/" + sess.id},
		{"Range", docker.RangeHeader(sess.size)}, {docker.UploadUUIDHeader, sess.id}, {docker.APIVersionHeader, docker.APIVersion}}
	if status == http.StatusNoContent {
		answerNoContent(w, headers)
		return
	}
	writeBody(w, req, status, headers, nil)
}

// answerNoContent is a 204 with the headers and no body and no length.
func answerNoContent(w http.ResponseWriter, headers [][2]string) {
	h := w.Header()
	for _, kv := range headers {
		h.Set(kv[0], kv[1])
	}
	h.Del("Content-Length")
	w.WriteHeader(http.StatusNoContent)
}

// answerBlob is the 201 of a blob the repository holds now.
func answerBlob(w http.ResponseWriter, req *request, repo repos.Repo, name, digest string) {
	writeBody(w, req, http.StatusCreated, [][2]string{{"Location", dockerBase(req, repo) + "/" + name + "/blobs/" + digest},
		{docker.DigestHeader, digest}, {docker.APIVersionHeader, docker.APIVersion}}, nil)
}

func unknownUpload(w http.ResponseWriter, req *request, id string) {
	dockerError(w, req, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", fmt.Sprintf("upload %s is not known", id), nil)
}

// startUpload is POST <name>/blobs/uploads/: a monolithic upload with
// ?digest=, a mount with ?mount=&from=, else a new session.
func (s *Server) startUpload(w *recorder, req *request, st *state, repo repos.Repo, settings repos.WriteSettings, call *docker.V2Path) error {
	q := req.params()
	if digest, ok := q.get("digest"); ok {
		err := s.finishBlob(w, req, repo, settings, call.Name, nil, digest)
		var kept *keepSession
		if errors.As(err, &kept) {
			return nil // answered; a monolithic upload has no session to keep
		}
		return err
	}
	if mount, ok := q.get("mount"); ok {
		from, _ := q.get("from")
		mounted, err := s.mountBlob(w, req, st, repo, call.Name, mount, from)
		if err != nil || mounted {
			return err
		}
	} else if req.r.ContentLength > 0 {
		dockerError(w, req, http.StatusBadRequest, "BLOB_UPLOAD_INVALID",
			"a POST without a digest starts a session and carries no body; send the blob with PATCH or with the closing PUT", nil)
		return nil
	}
	subject, err := s.subjectOf(req)
	if err != nil {
		return err
	}
	sess, bound := s.oci.open(repo.Name, call.Name, s.uploader(subject))
	if sess == nil {
		text := fmt.Sprintf("%d upload sessions are open; finish or cancel one first", maxUploadSessions)
		if bound == maxUserSessions {
			text = fmt.Sprintf("you hold %d upload sessions, the most one user may hold; finish or cancel one first", maxUserSessions)
		}
		dockerError(w, req, http.StatusTooManyRequests, "TOOMANYREQUESTS", text, [][2]string{{"Retry-After", "60"}})
		return nil
	}
	answerSession(w, req, repo, sess, http.StatusAccepted)
	return nil
}

// mountBlob is a mount: the repository holds the blob already, or the blob
// of the Docker repository whose name is the first segment of from, which
// the caller may read, becomes a copy in this repository. false with no
// error means no mount: the caller starts a session instead, as the
// specification asks.
func (s *Server) mountBlob(w *recorder, req *request, st *state, repo repos.Repo, name, digest, from string) (bool, error) {
	if !docker.IsDigest(digest) {
		dockerError(w, req, http.StatusBadRequest, "DIGEST_INVALID", fmt.Sprintf("%q is not a digest", digest), nil)
		return true, nil
	}
	path := dockerBlobPrefix + digest
	held, err := s.rp.index.Lookup(repo.Name, path)
	if err != nil {
		return false, err
	}
	if held != nil {
		answerBlob(w, req, repo, name, digest)
		return true, nil
	}
	source, err := s.mountSource(req, st, repo, from, path)
	if err != nil || source == nil {
		return false, err
	}
	subject, err := s.subjectOf(req)
	if err != nil {
		return false, err
	}
	address := peerAddress(w)
	if source.Store == s.writes.writer.Name() {
		_, err = s.linkCopy(repo, path, source, octet, stores.Digests{}, false, subject, address) // a blob by its digest: the same bytes
	} else {
		err = s.copyFromStore(st, repo, path, source, digest, subject, address)
	}
	if err != nil {
		return true, s.dockerFailed(w, req, err)
	}
	answerBlob(w, req, repo, name, digest)
	return true, nil
}

// mountSource is the row of the blob in the Docker repository that from
// names by its first segment, when it is another repository of this server
// and the caller holds read on it; nil otherwise.
func (s *Server) mountSource(req *request, st *state, repo repos.Repo, from, path string) (*index.Asset, error) {
	name, _, _ := strings.Cut(from, "/")
	source, ok := st.repositories.Get(name)
	if !ok || name == repo.Name || repos.AccessOf(source) != "docker" {
		return nil, nil
	}
	subject, err := s.subjectOf(req)
	if err != nil {
		return nil, err
	}
	// The route asked the rights of the target repository; a mount also
	// reads the source, which the route does not name.
	if !s.sec.Permits(subject, rest.ViewPermission(source.Format, source.Name, "read")) {
		return nil, nil
	}
	for _, member := range dockerMembers(st.repositories, source.Name) {
		row, err := s.rp.index.Lookup(member.Name, path)
		if err != nil || row != nil {
			return row, err
		}
	}
	return nil, nil
}

// copyFromStore is the copy of a blob of a read only store: its bytes,
// read and never written there, through ingest.
func (s *Server) copyFromStore(st *state, repo repos.Repo, path string, source *index.Asset, digest string,
	subject *model.Subject, address string) error {
	store := st.stores[source.Store]
	if store == nil {
		return &unavailable{fmt.Sprintf("Store %s is not configured", source.Store)}
	}
	f, err := store.Open(source.Location)
	if err != nil {
		return err
	}
	defer f.Close()
	algorithm, value := docker.DigestParts(digest)
	_, err = s.ingest(ingestRequest{repo: repo, path: path, body: f, size: source.Size, digests: map[string]string{algorithm: value},
		contentType: octet, subject: subject, address: address})
	return err
}

// patchUpload is PATCH of a session: one chunk, with Content-Range at the
// offset of the session or without it after the last byte, appended to its
// work file. A chunk that fails leaves the session at the offset before it.
func (s *Server) patchUpload(w *recorder, req *request, repo repos.Repo, settings repos.WriteSettings, call *docker.V2Path) error {
	subject, err := s.subjectOf(req)
	if err != nil {
		return err
	}
	sess := s.oci.take(call.Reference, repo.Name, call.Name, s.uploader(subject))
	if sess == nil {
		unknownUpload(w, req, call.Reference)
		return nil
	}
	end := false
	defer func() { s.oci.release(sess, end) }()
	if !s.chunkFits(w, req, repo, sess) {
		return nil
	}
	length := req.r.ContentLength
	if length >= 0 && sess.size+length > settings.MaxBodyBytes {
		end = true
		return s.dockerFailed(w, req, writeRefused(http.StatusRequestEntityTooLarge, ruleTooLarge,
			"the upload passes the limit of %d bytes of repository %s", settings.MaxBodyBytes, repo.Name))
	}
	room, err := s.checkSpace(max(length, 0), settings.FreeSpaceFloor)
	if err != nil {
		return s.dockerFailed(w, req, err)
	}
	body := newTimedBody(w, req)
	n, err := s.appendChunk(sess, room.reader(body), settings.MaxBodyBytes-sess.size)
	var refusal *writeRefusal
	switch {
	case errors.Is(err, stores.ErrTooLarge):
		end = true
		return s.dockerFailed(w, req, writeRefused(http.StatusRequestEntityTooLarge, ruleTooLarge,
			"the upload passes the limit of %d bytes of repository %s", settings.MaxBodyBytes, repo.Name))
	case errors.As(err, &refusal):
		return s.dockerFailed(w, req, err) // past the room of the store; the session stays at its offset
	case err != nil:
		if status, cut := body.cut(); cut {
			return s.bodyLeft(w, req, repo, call, status, body.err)
		}
		return err
	}
	sess.size += n
	answerSession(w, req, repo, sess, http.StatusAccepted)
	return nil
}

// chunkFits checks the Content-Range of a chunk against the session: the
// first byte at its offset, a range of the length of the body. false means
// the answer is written: 416 with the offset of the session for a chunk out
// of order, 400 for a range that is no range.
func (s *Server) chunkFits(w *recorder, req *request, repo repos.Repo, sess *uploadSession) bool {
	value := req.r.Header.Get("Content-Range")
	if value == "" {
		return true
	}
	first, last, ok := docker.ParseRange(value)
	switch {
	case !ok:
		dockerError(w, req, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", fmt.Sprintf("Content-Range %q is no range <first>-<last>", value), nil)
	case first != sess.size:
		dockerError(w, req, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID",
			fmt.Sprintf("the chunk starts at byte %d, the upload holds %d bytes", first, sess.size),
			[][2]string{{"Location", dockerBase(req, repo) + "/" + sess.name + "/blobs/uploads/" + sess.id},
				{"Range", docker.RangeHeader(sess.size)}, {docker.UploadUUIDHeader, sess.id}})
	case req.r.ContentLength >= 0 && last-first+1 != req.r.ContentLength:
		dockerError(w, req, http.StatusBadRequest, "SIZE_INVALID",
			fmt.Sprintf("Content-Range %s names %d bytes, the body declares %d", value, last-first+1, req.r.ContentLength), nil)
	default:
		return true
	}
	return false
}

// appendChunk appends at most limit bytes of body to the work file of the
// session and returns their count; more than limit fail with
// stores.ErrTooLarge. On an error the work file is as it was before.
func (s *Server) appendChunk(sess *uploadSession, body io.Reader, limit int64) (int64, error) {
	if sess.work == "" {
		sess.work = workFileIn(s.writes.writer.Root(), sess.id)
	}
	f, err := os.OpenFile(sess.work, os.O_WRONLY|os.O_CREATE, 0o600) // #nosec G304 -- content/tmp of the write store and a name of two UUIDs.
	if err != nil {
		return 0, err
	}
	n, err := func() (int64, error) {
		if _, err := f.Seek(sess.size, io.SeekStart); err != nil {
			return 0, err
		}
		n, err := io.CopyBuffer(f, io.LimitReader(body, limit+1), make([]byte, copyBufferBytes))
		if err == nil && n > limit {
			err = stores.ErrTooLarge
		}
		return n, err
	}()
	if err != nil {
		if truncErr := f.Truncate(sess.size); truncErr != nil {
			err = errors.Join(err, truncErr)
		}
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil && sess.size == 0 {
		if rmErr := os.Remove(sess.work); rmErr == nil || errors.Is(rmErr, fs.ErrNotExist) {
			sess.work = ""
		}
	}
	return n, err
}

// bodyLeft answers a chunk or a closing PUT whose body did not arrive in
// full: the session stays at the offset before it.
func (s *Server) bodyLeft(w *recorder, req *request, repo repos.Repo, call *docker.V2Path, status int, cause error) error {
	logAt(slogWarn, serverLogger, "%s of an upload of %s/%s: the body did not arrive in full, the upload stays where it was: %v",
		req.r.Method, repo.Name, call.Name, cause)
	w.Header().Set("Connection", "close")
	dockerError(w, req, status, "BLOB_UPLOAD_INVALID", "the body did not arrive in full; the upload stays at the offset before it", nil)
	return nil
}

// finishUpload is the closing PUT of a session: the digest of the whole
// blob, and the last chunk when the body holds one.
func (s *Server) finishUpload(w *recorder, req *request, repo repos.Repo, settings repos.WriteSettings, call *docker.V2Path) error {
	subject, err := s.subjectOf(req)
	if err != nil {
		return err
	}
	sess := s.oci.take(call.Reference, repo.Name, call.Name, s.uploader(subject))
	if sess == nil {
		unknownUpload(w, req, call.Reference)
		return nil
	}
	end := false
	defer func() { s.oci.release(sess, end) }()
	if !s.chunkFits(w, req, repo, sess) {
		return nil
	}
	digest, _ := req.params().get("digest")
	err = s.finishBlob(w, req, repo, settings, call.Name, sess, digest)
	var kept *keepSession
	if errors.As(err, &kept) {
		return nil
	}
	end = true
	return err
}

// keepSession is the error of finishBlob after an answer that leaves the
// session open: the body did not arrive in full, or the store has no room.
type keepSession struct{}

func (*keepSession) Error() string { return "the session stays" }

// finishBlob makes the blob of digest from the bytes of a session (nil for
// a monolithic POST) and the body of the request: through ingest, or, when
// the repository holds the blob already, by checking the digest of the
// bytes and writing nothing. It answers every case; an error that is a
// *keepSession says the answer leaves the session open.
func (s *Server) finishBlob(w *recorder, req *request, repo repos.Repo, settings repos.WriteSettings, name string, sess *uploadSession,
	digest string) error {
	if !docker.IsDigest(digest) {
		dockerError(w, req, http.StatusBadRequest, "DIGEST_INVALID", "the closing request names the digest of the whole blob in ?digest=", nil)
		return nil
	}
	held := int64(0)
	parts := []io.Reader{}
	if sess != nil && sess.size > 0 {
		f, err := os.Open(sess.work) // #nosec G304 -- the work file of the session in content/tmp of the write store.
		if err != nil {
			return err
		}
		defer f.Close()
		parts, held = append(parts, f), sess.size
	}
	body := newTimedBody(w, req)
	parts = append(parts, body)
	declared := int64(-1)
	if req.r.ContentLength >= 0 {
		declared = held + req.r.ContentLength
	}
	if declared > settings.MaxBodyBytes {
		return s.dockerFailed(w, req, writeRefused(http.StatusRequestEntityTooLarge, ruleTooLarge,
			"the blob passes the limit of %d bytes of repository %s", settings.MaxBodyBytes, repo.Name))
	}
	path := dockerBlobPrefix + digest
	row, err := s.rp.index.Lookup(repo.Name, path)
	if err != nil {
		return err
	}
	if row != nil {
		return s.checkHeldBlob(w, req, repo, name, digest, io.MultiReader(parts...), body, settings.MaxBodyBytes)
	}
	subject, err := s.subjectOf(req)
	if err != nil {
		return err
	}
	algorithm, value := docker.DigestParts(digest)
	_, err = s.ingest(ingestRequest{repo: repo, path: path, body: io.MultiReader(parts...), size: declared,
		digests: map[string]string{algorithm: value}, contentType: octet, subject: subject, address: peerAddress(w)})
	if err != nil {
		if status, cut := body.cut(); cut {
			_ = s.bodyLeft(w, req, repo, &docker.V2Path{Name: name}, status, body.err)
			return &keepSession{}
		}
		var refusal *writeRefusal
		if errors.As(err, &refusal) && (refusal.rule == ruleNoSpace || refusal.rule == ruleBusy) {
			_ = s.dockerFailed(w, req, err)
			return &keepSession{}
		}
		return s.dockerFailed(w, req, err)
	}
	answerBlob(w, req, repo, name, digest)
	return nil
}

// checkHeldBlob is the end of an upload of a blob the repository holds:
// the bytes are hashed and dropped, no second blob is written. They are read
// under the limit of the repository, as every body is (finding N4 of review
// 4): a body without a declared length is cut there with 413.
func (s *Server) checkHeldBlob(w *recorder, req *request, repo repos.Repo, name, digest string, data io.Reader, body *timedBody, limit int64) error {
	h := docker.NewDigest(digest)
	n, err := io.CopyBuffer(h, io.LimitReader(data, limit+1), make([]byte, copyBufferBytes))
	if err != nil {
		if status, cut := body.cut(); cut {
			_ = s.bodyLeft(w, req, repo, &docker.V2Path{Name: name}, status, body.err)
			return &keepSession{}
		}
		return err
	}
	if n > limit {
		return s.dockerFailed(w, req, writeRefused(http.StatusRequestEntityTooLarge, ruleTooLarge,
			"the blob passes the limit of %d bytes of repository %s", limit, repo.Name))
	}
	algorithm, _ := docker.DigestParts(digest)
	if actual := docker.SumOf(algorithm, h); actual != digest {
		dockerError(w, req, http.StatusBadRequest, "DIGEST_INVALID", fmt.Sprintf("the bytes have the digest %s, the request names %s", actual, digest), nil)
		return nil
	}
	answerBlob(w, req, repo, name, digest)
	return nil
}

// cancelUpload is DELETE of a session: it ends, its work file goes.
func (s *Server) cancelUpload(w *recorder, req *request, repo repos.Repo, call *docker.V2Path) error {
	subject, err := s.subjectOf(req)
	if err != nil {
		return err
	}
	sess := s.oci.take(call.Reference, repo.Name, call.Name, s.uploader(subject))
	if sess == nil {
		unknownUpload(w, req, call.Reference)
		return nil
	}
	s.oci.release(sess, true)
	answerNoContent(w, [][2]string{{docker.APIVersionHeader, docker.APIVersion}})
	return nil
}

// uploadCall is the session a read path names below v2/ while writes are
// on, nil for any other path and with writes off.
func (s *Server) uploadCall(rest string) *docker.V2Path {
	if s.writes.writer == nil {
		return nil
	}
	call, err := docker.ParseWrite(rest)
	if err != nil || call == nil || call.Kind != "upload" || call.Reference == "" {
		return nil
	}
	return call
}

// uploadStatus is GET of a session, which the read path hands over while
// writes are on: 204 with its offset.
func (s *Server) uploadStatus(w http.ResponseWriter, req *request, repo repos.Repo, call *docker.V2Path) error {
	subject, err := s.subjectOf(req)
	if err != nil {
		return err
	}
	sess := s.oci.take(call.Reference, repo.Name, call.Name, s.uploader(subject))
	if sess == nil {
		unknownUpload(w, req, call.Reference)
		return nil
	}
	defer s.oci.release(sess, false)
	answerSession(w, req, repo, sess, http.StatusNoContent)
	return nil
}

// sessionCount is the number of open sessions; tests read it.
func (s *Server) sessionCount() int { return s.oci.count() }
