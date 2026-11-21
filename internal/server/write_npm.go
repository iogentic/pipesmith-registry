package server

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/npm"
	"github.com/iogentic/pipesmith-registry/internal/policy"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// writeNpm takes a write to a hosted npm repository: publish, dist tags,
// deprecate and unpublish (task 11 of the phase 4 plan, leg 4c).
func (s *Server) writeNpm(w *recorder, req *request, st *state, repo repos.Repo, t *target) error {
	return s.npmWrite(w, req, st, repo, t)
}

// The calls are those the npm command line sends (npm/cli, branch latest,
// read 2026-11-18): publish PUT /<name> with the tarball as an attachment
// (libnpmpublish publish.js); deprecate PUT /<name> with the document it
// read and a deprecated field per version (lib/commands/deprecate.js);
// unpublish of one version PUT /<name>/-rev/<rev> without that version,
// then DELETE /<name>/-/<file>.tgz/-rev/<rev>, and of the package DELETE
// /<name>/-rev/<rev> (libnpmpublish unpublish.js); dist-tag PUT and DELETE
// /-/package/<name>/dist-tags/<tag> with the version as a JSON string
// (lib/commands/dist-tag.js). The tarball and the merged document are two
// blobs (W10): <name>/-/<unscoped>-<version>.tgz and <name>, the names the
// read path of the port reads. The status codes of the answers are
// provisional (W6): the npm command line takes any 2xx.

// npmCallAllow are the methods of each kind of npm write path, for the
// Allow header of a 405.
var npmCallAllow = map[npm.WriteKind]string{
	npm.WriteDocument: "GET, HEAD, PUT",
	npm.WriteTarball:  "GET, HEAD, DELETE",
	npm.WriteDistTag:  "PUT, DELETE",
}

// maxTagBody is the most a dist-tag body holds: a version as a JSON string.
const maxTagBody = 4096

// npmWrite is the dispatch of writeNpm.
func (s *Server) npmWrite(w *recorder, req *request, st *state, repo repos.Repo, t *target) error {
	call, ok := npm.ParseWrite(t.path)
	if !ok || t.directory {
		if npmDash(t) {
			return s.refuseWrite(w, req, nil) // the npm 404 of the port below "-"
		}
		npmRefuse(w, req, http.StatusMethodNotAllowed, req.r.Method+" is not an npm write for this path", "GET, HEAD")
		return nil
	}
	method := req.r.Method
	settings, _, err := s.writeSettingsOf(repo)
	if err != nil {
		return s.npmFailed(w, req, err)
	}
	if err := policy.Write(settings.WritePolicy, false, false); err != nil {
		return s.npmFailed(w, req, refusalOf(err)) // before a byte of the body is read
	}
	switch {
	case call.Kind == npm.WriteDocument && call.Rev == "" && method == http.MethodPut:
		return s.npmPut(w, req, st, repo, settings, call)
	case call.Kind == npm.WriteDocument && call.Rev != "" && method == http.MethodPut:
		return s.npmRewrite(w, req, st, repo, call)
	case call.Kind == npm.WriteDocument && call.Rev != "" && method == http.MethodDelete:
		return s.npmUnpublish(w, req, st, repo, call)
	case call.Kind == npm.WriteTarball && method == http.MethodDelete:
		return s.npmDeleteTarball(w, req, st, repo, call)
	case call.Kind == npm.WriteDistTag && method == http.MethodPut:
		return s.npmPutTag(w, req, st, repo, call)
	case call.Kind == npm.WriteDistTag && method == http.MethodDelete:
		return s.npmDropTag(w, req, st, repo, call)
	}
	allowed := npmCallAllow[call.Kind]
	if call.Kind == npm.WriteDocument && call.Rev != "" {
		allowed = "PUT, DELETE"
	}
	npmRefuse(w, req, http.StatusMethodNotAllowed, method+" is not an npm write for this path", allowed)
	return nil
}

// npmRefuse is an npm answer that refuses: {"error": <text>}, the field the
// npm command line prints.
func npmRefuse(w http.ResponseWriter, req *request, status int, message, allowed string) {
	data, _ := pyjson.ResponseBody(pyjson.NewObject().Set("error", message)) // an object of one text always encodes
	if allowed != "" {
		w.Header().Set("Allow", allowed)
	}
	npmJSON(w, req, status, data)
}

// npmDone is the answer of a write that is done, in the form CouchDB gives
// one: ok, the package and its new revision.
func npmDone(w http.ResponseWriter, req *request, status int, name, rev string) {
	answer := pyjson.NewObject().Set("ok", true).Set("id", name)
	if rev != "" {
		answer.Set("rev", rev)
	}
	data, _ := pyjson.ResponseBody(answer) // texts and a bool always encode
	npmJSON(w, req, status, data)
}

// npmFailed answers a write that ingest, erase or a rule refused, with the
// status of section Refusals of the phase 4 plan (W6) and its text. Any
// other error is one of the server.
func (s *Server) npmFailed(w *recorder, req *request, err error) error {
	var refusal *writeRefusal
	if errors.As(err, &refusal) {
		if refusal.rule == ruleTooLarge {
			w.Header().Set("Connection", "close") // the rest of the body is not read
		}
		retryAfter(w, refusal)
		npmRefuse(w, req, refusal.status, refusal.message, "")
		return nil
	}
	var rule *npm.Refusal
	if errors.As(err, &rule) {
		npmRefuse(w, req, http.StatusBadRequest, rule.Message, "")
		return nil
	}
	return err
}

// npmBodyLeft answers a write whose body did not arrive in full; nothing is
// written and its work file is gone.
func (s *Server) npmBodyLeft(w *recorder, req *request, repo repos.Repo, name string, body *timedBody) error {
	status, _ := body.cut()
	logAt(slogWarn, serverLogger, "%s of %s/%s: the body did not arrive in full, nothing is written: %v", req.r.Method, repo.Name, name, body.err)
	w.Header().Set("Connection", "close")
	npmRefuse(w, req, status, "the body did not arrive in full; nothing is written", "")
	return nil
}

// npmLock holds the package of a repository: one change of its document at
// a time. The key holds a NUL, which no path of a request has, so it never
// meets the lock of a path that ingest takes.
func (s *Server) npmLock(repo repos.Repo, name string) (unlock func()) {
	return s.writes.locks.lock(repo.Name, "\x00npm\x00"+name)
}

// storedDocument is the document of a package that the repository serves,
// nil when it has none or none that reads as a JSON object.
func (s *Server) storedDocument(st *state, repo repos.Repo, name string) (*pyjson.Object, error) {
	asset, f, size, err := s.rp.open(st, repo.Name, name)
	if err != nil || asset == nil {
		return nil, err
	}
	data, err := readBounded(f, size, maxDocumentBytes)
	if err != nil {
		return nil, err
	}
	value, decodeErr := pyjson.Decode(data)
	document, ok := value.(*pyjson.Object)
	if len(data) == 0 || int64(len(data)) > maxDocumentBytes || decodeErr != nil || !ok {
		logAt(slogWarn, serverLogger, "%s/%s in store %s is no package document that a write can merge; the write starts a new one",
			asset.Repo, name, asset.Store)
		return nil, nil
	}
	return document, nil
}

// writeDocument stores a package document as a blob of its own (W10), with
// a new _rev after previous, and returns that revision. The write policy
// lets it through on every change, as the metadata of Maven.
func (s *Server) writeDocument(repo repos.Repo, name string, document *pyjson.Object, previous string, subject *model.Subject,
	address string) (string, error) {
	encode := func() ([]byte, error) {
		return pyjson.Encode(document, pyjson.Options{EnsureASCII: true, ItemSep: ",", KeySep: ":"})
	}
	document.Set("_rev", "")
	draft, err := encode()
	if err != nil {
		return "", err
	}
	rev := npm.NextRev(previous, draft)
	document.Set("_rev", rev)
	data, err := encode()
	if err != nil {
		return "", err
	}
	if int64(len(data)) > maxDocumentBytes {
		return "", writeRefused(http.StatusRequestEntityTooLarge, ruleTooLarge, "the document of %s would pass %d bytes, the most the read path serves",
			name, maxDocumentBytes)
	}
	_, err = s.ingest(ingestRequest{repo: repo, path: name, body: bytes.NewReader(data), size: int64(len(data)), contentType: jsonType,
		exempt: true, subject: subject, address: address})
	return rev, err
}

func nowISO() string {
	at, _ := npm.IsoTime(time.Now().UnixMilli()) // the year of now is in range
	return at
}

// spoolFile holds the data of an attachment while the rest of a publish
// document is read: a work file in content/tmp of the write store, named
// as the old product names its work files, so a start of the server
// removes one that a stop left (W2).
type spoolFile struct {
	path    string
	f       *os.File
	size    int64
	limit   int64
	room    *storeRoom // the room of the store, which the spools of one body share
	release func()     // ends the hold of the work file for the task of old work files (ruling KN15)
}

func (s *Server) newSpool(limit int64, room *storeRoom) (*spoolFile, error) {
	w := s.writes.writer
	if room != nil && room.writer != nil {
		w = room.writer // the store of the repository (review R3)
	}
	path := workFileIn(w.Root(), newUUID())
	release := w.Hold(filepath.Base(path))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- content/tmp of the write store and a name of two UUIDs.
	if err != nil {
		release()
		return nil, err
	}
	return &spoolFile{path: path, f: f, limit: limit, room: room, release: release}, nil
}

func (sp *spoolFile) Write(p []byte) (int, error) {
	if sp.size+int64(len(p)) > sp.limit {
		return 0, stores.ErrTooLarge
	}
	if err := sp.room.take(int64(len(p))); err != nil {
		return 0, err
	}
	n, err := sp.f.Write(p)
	sp.size += int64(n)
	return n, err
}

func (sp *spoolFile) reader() (io.Reader, error) {
	if _, err := sp.f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return sp.f, nil
}

func (sp *spoolFile) remove() {
	_ = sp.f.Close() // the file goes; its close error changes nothing
	if err := os.Remove(sp.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		logAt(slogWarn, serverLogger, "the work file %s of a publish is not removed: %v", sp.path, err)
	}
	sp.release()
}

// npmPut is PUT /<name>: a publish when the document carries an
// attachment, else a deprecation of its versions.
func (s *Server) npmPut(w *recorder, req *request, st *state, repo repos.Repo, settings repos.WriteSettings, call npm.WritePath) error {
	room, err := s.checkSpace(settings.WriteStore, max(req.r.ContentLength, 0), settings.FreeSpaceFloor)
	if err != nil {
		return s.npmFailed(w, req, err)
	}
	defer room.release()
	var tarball *spoolFile
	defer func() {
		if tarball != nil {
			tarball.remove()
		}
	}()
	body := newTimedBody(w, req)
	document, attachments, err := npm.ReadPublish(body, func(string) (io.Writer, error) {
		if tarball != nil {
			return nil, &npm.Refusal{Message: "a publish carries exactly one attachment, the tarball of its version"}
		}
		var err error
		tarball, err = s.newSpool(settings.MaxBodyBytes, room)
		return tarball, err
	})
	if err != nil {
		if _, cut := body.cut(); cut {
			return s.npmBodyLeft(w, req, repo, call.Name, body)
		}
		if errors.Is(err, stores.ErrTooLarge) {
			err = writeRefused(http.StatusRequestEntityTooLarge, ruleTooLarge, "the tarball is larger than the limit of %d bytes of repository %s",
				settings.MaxBodyBytes, repo.Name)
		}
		return s.npmFailed(w, req, err)
	}
	room.release() // the body is read; the ingest of the tarball holds its own room
	if len(attachments) == 0 {
		return s.npmDeprecate(w, req, st, repo, call, document)
	}
	names := make([]string, len(attachments))
	for i, a := range attachments {
		names[i] = a.Name
	}
	published, err := npm.CheckPublish(document, call.Name, names)
	if err != nil {
		return s.npmFailed(w, req, err)
	}
	if a := attachments[0]; a.Length >= 0 && a.Length != a.Size {
		return s.npmFailed(w, req, writeRefused(http.StatusBadRequest, ruleSize, "attachment %s declares %d bytes, its data has %d", a.Name, a.Length, a.Size))
	}
	subject, err := s.subjectOf(req)
	if err != nil {
		return err
	}
	address := peerAddress(w)
	unlock := s.npmLock(repo, call.Name)
	defer unlock()
	stored, err := s.storedDocument(st, repo, call.Name)
	if err != nil {
		return err
	}
	data, err := tarball.reader()
	if err != nil {
		return err
	}
	rev, err := s.publishVersionAs(repo, addressed(req, repo), call.Name, stored, document, published, data, tarball.size, subject, address)
	if err != nil {
		return s.npmFailed(w, req, err)
	}
	npmDone(w, req, http.StatusCreated, call.Name, rev)
	return nil
}

// publishVersion stores the tarball of a published version through ingest
// and then the package document with the version merged in, and returns
// its revision: the publish of npmPut and the npm upload of the REST
// writes. The caller holds the lock of the package (npmLock). When the
// document fails, a tarball of a new path goes again through erase, so
// that no live tarball of a failed publish stays to build the version from
// and a second publish of it is taken under ALLOW_ONCE too; a tarball that
// replaced one stays, since the blob it replaced carries the delete mark.
// A package whose document exists under the lock asks edit, as the route
// of a publish asks it once the document exists: a caller who holds add
// only is refused with 403 before the tarball (finding S3 of review 4).
func (s *Server) publishVersion(repo repos.Repo, name string, stored, document *pyjson.Object, published *npm.Published, tarball io.Reader,
	size int64, subject *model.Subject, address string) (string, error) {
	return s.publishVersionAs(repo, repo, name, stored, document, published, tarball, size, subject, address)
}

// publishVersionAs is publishVersion with the repository whose permission
// it asks: the group a publish came through (decision 5.16 of the phase 5
// plan), else the repository itself.
func (s *Server) publishVersionAs(repo, rights repos.Repo, name string, stored, document *pyjson.Object, published *npm.Published,
	tarball io.Reader, size int64, subject *model.Subject, address string) (string, error) {
	addOnly := !s.mayEdit(subject, rights)
	if addOnly {
		row, err := s.rp.index.Lookup(repo.Name, name)
		if err != nil {
			return "", err
		}
		if row != nil {
			return "", editRefused()
		}
	}
	path := npm.TarballPath(name, published.Version)
	result, err := s.ingest(ingestRequest{repo: repo, path: path, body: tarball, size: size,
		digests: published.Digests, addOnly: addOnly, subject: subject, address: address})
	if err != nil {
		return "", err
	}
	rev, err := s.mergeVersion(repo, name, stored, document, published, result, subject, address)
	if err == nil {
		return rev, nil
	}
	switch {
	case result.replaced != nil:
		logAt(slogWarn, serverLogger, "the tarball of %s@%s in %s is stored, its document is not: %v", name, published.Version, repo.Name, err)
	default:
		if _, eraseErr := s.erase(repo, path); eraseErr != nil {
			logAt(slogWarn, serverLogger, "the tarball of %s@%s in %s is stored, its document is not (%v), and the tarball keeps no delete mark: %v",
				name, published.Version, repo.Name, err, eraseErr)
		} else {
			logAt(slogWarn, serverLogger, "the document of %s@%s in %s is not written, the tarball of the publish is marked deleted: %v",
				name, published.Version, repo.Name, err)
		}
	}
	return "", err
}

// mergeVersion writes the package document with a stored tarball merged in.
func (s *Server) mergeVersion(repo repos.Repo, name string, stored, document *pyjson.Object, published *npm.Published, result *ingestResult,
	subject *model.Subject, address string) (string, error) {
	at, err := npm.IsoTime(result.asset.CreatedMS)
	if err != nil {
		return "", err
	}
	long, err := hex.DecodeString(result.digests.SHA512)
	if err != nil {
		return "", err
	}
	merged := npm.MergePublish(stored, document, name, published, result.digests.SHA1, "sha512-"+base64.StdEncoding.EncodeToString(long), at)
	return s.writeDocument(repo, name, merged, npm.Rev(stored), subject, address)
}

// revChanged answers 409 for a write that names a revision other than the
// one of the stored document, as CouchDB does; a document without a
// revision takes any.
func revChanged(w http.ResponseWriter, req *request, stored *pyjson.Object, rev string) bool {
	have := npm.Rev(stored)
	if rev == "" || have == "" || rev == have {
		return false
	}
	npmRefuse(w, req, http.StatusConflict, "the document has the revision "+have+", the write names "+rev+"; read it again", "")
	return true
}

// npmDeprecate is PUT /<name> without an attachment: the deprecated field
// of each version it names, every other field left out.
func (s *Server) npmDeprecate(w *recorder, req *request, st *state, repo repos.Repo, call npm.WritePath, update *pyjson.Object) error {
	subject, err := s.subjectOf(req)
	if err != nil {
		return err
	}
	unlock := s.npmLock(repo, call.Name)
	defer unlock()
	stored, err := s.storedDocument(st, repo, call.Name)
	if err != nil {
		return err
	}
	if stored == nil {
		npmNotFound(w, req)
		return nil
	}
	if !s.mayEdit(subject, addressed(req, repo)) {
		return s.npmFailed(w, req, editRefused()) // the document appeared while the request came in
	}
	if revChanged(w, req, stored, npm.Rev(update)) {
		return nil
	}
	changed, err := npm.Deprecate(stored, update, nowISO())
	if err != nil {
		return s.npmFailed(w, req, err)
	}
	rev, err := s.writeDocument(repo, call.Name, changed, npm.Rev(stored), subject, peerAddress(w))
	if err != nil {
		return s.npmFailed(w, req, err)
	}
	npmDone(w, req, http.StatusOK, call.Name, rev)
	return nil
}

// npmRewrite is PUT /<name>/-rev/<rev>, the first half of an unpublish of
// versions: the versions the rewrite leaves out go with their tarballs, so
// that the read path does not build them again from the tarballs.
func (s *Server) npmRewrite(w *recorder, req *request, st *state, repo repos.Repo, call npm.WritePath) error {
	body := newTimedBody(w, req)
	rewrite, _, err := npm.ReadPublish(body, func(string) (io.Writer, error) {
		return nil, &npm.Refusal{Message: "the rewrite of a document under its revision carries no attachment"}
	})
	if err != nil {
		if _, cut := body.cut(); cut {
			return s.npmBodyLeft(w, req, repo, call.Name, body)
		}
		return s.npmFailed(w, req, err)
	}
	subject, err := s.subjectOf(req)
	if err != nil {
		return err
	}
	unlock := s.npmLock(repo, call.Name)
	defer unlock()
	stored, err := s.storedDocument(st, repo, call.Name)
	if err != nil {
		return err
	}
	if stored == nil {
		npmNotFound(w, req)
		return nil
	}
	if revChanged(w, req, stored, call.Rev) {
		return nil
	}
	removed, err := npm.Unpublished(stored, rewrite)
	if err != nil {
		return s.npmFailed(w, req, err)
	}
	gone := map[string]bool{}
	for _, v := range removed {
		gone[v] = true
	}
	var paths []string
	for _, vp := range npm.TarballPaths(stored) {
		if gone[vp.Version] {
			paths = append(paths, vp.Path)
		}
	}
	if err := s.erasePaths(repo, paths); err != nil {
		return s.npmFailed(w, req, err)
	}
	changed, err := npm.RemoveVersions(stored, removed, nowISO())
	if err != nil {
		return err
	}
	rev, err := s.writeDocument(repo, call.Name, changed, npm.Rev(stored), subject, peerAddress(w))
	if err != nil {
		return s.npmFailed(w, req, err)
	}
	npmDone(w, req, http.StatusOK, call.Name, rev)
	return nil
}

// npmUnpublish is DELETE /<name>/-rev/<rev>: the delete mark on every
// tarball of the package and on its document.
func (s *Server) npmUnpublish(w *recorder, req *request, st *state, repo repos.Repo, call npm.WritePath) error {
	unlock := s.npmLock(repo, call.Name)
	defer unlock()
	stored, err := s.storedDocument(st, repo, call.Name)
	if err != nil {
		return err
	}
	if revChanged(w, req, stored, call.Rev) {
		return nil
	}
	directory := call.Name + "/" + npm.TarballDirectory
	children, err := s.rp.allChildren(repo.Name, directory)
	if err != nil {
		return err
	}
	var paths []string
	for _, c := range children {
		if !c.IsDir {
			paths = append(paths, directory+"/"+c.Name)
		}
	}
	row, err := s.rp.index.Lookup(repo.Name, call.Name)
	if err != nil {
		return err
	}
	if row != nil {
		paths = append(paths, call.Name)
	}
	if len(paths) == 0 {
		npmNotFound(w, req)
		return nil
	}
	if err := s.erasePaths(repo, paths); err != nil {
		return s.npmFailed(w, req, err)
	}
	npmDone(w, req, http.StatusOK, call.Name, "")
	return nil
}

// npmDeleteTarball is DELETE /<name>/-/<file>.tgz, with the revision of
// the document as the npm command line sends it: the delete mark on the
// tarball. A version of the document whose tarball is gone is no longer
// served.
func (s *Server) npmDeleteTarball(w *recorder, req *request, st *state, repo repos.Repo, call npm.WritePath) error {
	unlock := s.npmLock(repo, call.Name)
	defer unlock()
	stored, err := s.storedDocument(st, repo, call.Name)
	if err != nil {
		return err
	}
	if revChanged(w, req, stored, call.Rev) {
		return nil
	}
	removed, err := s.erase(repo, call.Name+"/"+npm.TarballDirectory+"/"+call.File)
	if err != nil {
		return s.npmFailed(w, req, err)
	}
	if removed == nil {
		npmNotFound(w, req)
		return nil
	}
	npmDone(w, req, http.StatusOK, call.Name, "")
	return nil
}

// npmPutTag is PUT /-/package/<name>/dist-tags/<tag> with the version as a
// JSON string.
func (s *Server) npmPutTag(w *recorder, req *request, st *state, repo repos.Repo, call npm.WritePath) error {
	if !npm.IsTag(call.Tag) {
		npmRefuse(w, req, http.StatusBadRequest, "the tag is no name of letters, digits, dots, underscores and hyphens", "")
		return nil
	}
	body := newTimedBody(w, req)
	data, err := io.ReadAll(io.LimitReader(body, maxTagBody+1))
	if err != nil {
		if _, cut := body.cut(); cut {
			return s.npmBodyLeft(w, req, repo, call.Name, body)
		}
		return err
	}
	value, decodeErr := pyjson.Decode(data)
	version, ok := value.(string)
	if len(data) > maxTagBody || decodeErr != nil || !ok {
		npmRefuse(w, req, http.StatusBadRequest, "the body of a dist-tag is its version as a JSON string", "")
		return nil
	}
	subject, err := s.subjectOf(req)
	if err != nil {
		return err
	}
	unlock := s.npmLock(repo, call.Name)
	defer unlock()
	stored, err := s.storedDocument(st, repo, call.Name)
	if err != nil {
		return err
	}
	if stored == nil {
		npmNotFound(w, req)
		return nil
	}
	if !s.mayEdit(subject, addressed(req, repo)) {
		return s.npmFailed(w, req, editRefused()) // the document appeared while the request came in
	}
	changed, err := npm.SetTag(stored, call.Tag, version, nowISO())
	if err != nil {
		return s.npmFailed(w, req, err)
	}
	rev, err := s.writeDocument(repo, call.Name, changed, npm.Rev(stored), subject, peerAddress(w))
	if err != nil {
		return s.npmFailed(w, req, err)
	}
	npmDone(w, req, http.StatusOK, call.Name, rev)
	return nil
}

// npmDropTag is DELETE /-/package/<name>/dist-tags/<tag>.
func (s *Server) npmDropTag(w *recorder, req *request, st *state, repo repos.Repo, call npm.WritePath) error {
	subject, err := s.subjectOf(req)
	if err != nil {
		return err
	}
	unlock := s.npmLock(repo, call.Name)
	defer unlock()
	stored, err := s.storedDocument(st, repo, call.Name)
	if err != nil {
		return err
	}
	if stored == nil || !npm.HasTag(stored, call.Tag) {
		npmNotFound(w, req)
		return nil
	}
	if !s.mayEdit(subject, addressed(req, repo)) {
		return s.npmFailed(w, req, editRefused()) // the document appeared while the request came in
	}
	changed, err := npm.DropTag(stored, call.Tag, nowISO())
	if err != nil {
		return s.npmFailed(w, req, err)
	}
	rev, err := s.writeDocument(repo, call.Name, changed, npm.Rev(stored), subject, peerAddress(w))
	if err != nil {
		return s.npmFailed(w, req, err)
	}
	npmDone(w, req, http.StatusOK, call.Name, rev)
	return nil
}
