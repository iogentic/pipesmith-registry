package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The request record of phase 7: ServeHTTP defers the end of track,
// accessLine and subjectOf leave what they learn in the note of the request,
// and the record goes to Server.requests once the answer ended. Without a
// sink nothing is wrapped and nothing is noted.

// noteKey is the context key of the note of a tracked request.
type noteKey struct{}

// note is what the chain learns about one request while it answers it.
type note struct {
	accessed bool   // accessLine ran
	id       string // the request id, in the request line and the audit line
	client   string // the client of the access line, "<host>:<port>"
	path     string // the path of the access line
	subject  string
	realm    string
	download bool // the answer counts as a download (L11, figures.go)
}

func noteOf(r *http.Request) *note {
	n, _ := r.Context().Value(noteKey{}).(*note)
	return n
}

func noop() {}

// requestSink calls Logs, when it is set, and returns the sink it gives.
func (o ServeOptions) requestSink() func(logs.Request) {
	if o.Logs == nil {
		return nil
	}
	return o.Logs()
}

// track starts the record of one request: it replaces the request by one
// that carries a note and counts its body, and the writer by one that keeps
// the status and counts the body bytes. The function it returns builds the
// record and hands it to the sink.
func (s *Server) track(w *http.ResponseWriter, r **http.Request) func() {
	if s.requests == nil && s.figures == nil {
		return noop
	}
	start := time.Now()
	n := &note{id: newRequestID()}
	req := (*r).WithContext(context.WithValue((*r).Context(), noteKey{}, n))
	var body *countingBody
	if req.Body != nil && req.Body != http.NoBody {
		body = &countingBody{ReadCloser: req.Body}
		req.Body = body
	}
	cw := &countingWriter{ResponseWriter: *w, head: req.Method == http.MethodHead}
	*w, *r = cw, req
	return func() {
		rec := s.record(req, n, cw, body, start)
		if s.requests != nil {
			s.requests(rec)
		}
		s.figures.Request(rec, n.download)
	}
}

// noteAccess is the first step of accessLine: it masks the query of the
// path (L9), leaves client and path in the note of a tracked request and
// returns both for the line. The note keeps the client as it came; the line
// gets it escaped by logs.Escape, so a client that X-Forwarded-For names
// cannot start a line of its own (finding sa-23).
func (s *Server) noteAccess(r *http.Request, client, path string) (string, string) {
	path = logs.MaskTarget(path)
	if n := noteOf(r); n != nil {
		n.accessed, n.client, n.path = true, client, path
	}
	return logs.Escape(client), path
}

// noteSubject leaves the subject the chain resolved in the note of a
// tracked request.
func noteSubject(r *http.Request, subject *model.Subject) {
	if n := noteOf(r); n != nil && subject != nil {
		n.subject, n.realm = subject.User.ID, subject.Realm
	}
}

// record is the record of a tracked request whose answer ended now. A
// request that never reached accessLine (invalidRequest) takes its client
// from RemoteAddr and its path from RequestURI, the query and an npm token masked.
func (s *Server) record(r *http.Request, n *note, w *countingWriter, body *countingBody, start time.Time) logs.Request {
	end := time.Now()
	uri, _ := strings.CutPrefix(r.RequestURI, s.marker)
	rawPath, _, _ := strings.Cut(uri, "?")
	target, client := n.path, clientHost(n.client, r.RemoteAddr)
	if !n.accessed {
		target, client = logs.MaskTarget(maskToken(uri)), r.RemoteAddr
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			client = host
		}
	}
	path, query, _ := strings.Cut(target, "?")
	rec := logs.Request{
		ID:            n.id,
		Time:          end.UTC(),
		Client:        client,
		Subject:       n.subject,
		Realm:         n.realm,
		Method:        r.Method,
		Path:          path,
		Query:         query,
		Protocol:      "HTTP/" + strconv.Itoa(r.ProtoMajor) + "." + strconv.Itoa(r.ProtoMinor),
		Status:        w.status,
		ContentLength: r.Header.Get("Content-Length"),
		ResponseBytes: w.bytes,
		Duration:      end.Sub(start),
		UserAgent:     r.Header.Get("User-Agent"),
		Port:          max(localPort(r), 0),
	}
	if body != nil {
		rec.RequestBytes = body.n
	}
	rec.Repository, rec.Format = s.repositoryOf(r, rawPath)
	return rec
}

// clientHost is the client of an access line without its port. ServeHTTP
// adds a port only when RemoteAddr has one.
func clientHost(client, remoteAddr string) string {
	if _, _, err := net.SplitHostPort(remoteAddr); err != nil {
		return client
	}
	if i := strings.LastIndexByte(client, ':'); i >= 0 {
		return client[:i]
	}
	return client
}

// repositoryOf is the repository of a request and its format, both empty
// unless the repository exists: on a Docker port the repository of the
// port, else the name after /repository/, after the browse route or after
// /api/browse/.
func (s *Server) repositoryOf(r *http.Request, rawPath string) (string, string) {
	name, ok := s.dockerPorts[localPort(r)]
	if !ok {
		for _, prefix := range []string{repositoryPrefix, browsePrefix} {
			if t, err := parseTarget(rawPath, prefix); err == nil && t != nil {
				name = t.repo
			}
		}
		if rest, found := strings.CutPrefix(routePath(rawPath), "/api/browse/"); found {
			name, _, _ = strings.Cut(rest, "/")
		}
	}
	if name == "" || s.rp == nil {
		return "", ""
	}
	repo, found := s.rp.current.Load().repositories.Get(name)
	if !found {
		return "", ""
	}
	return repo.Name, repo.Format
}

// newRequestID is 16 lower case hex digits from 8 random bytes.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}

// countingBody counts the bytes of a request body the handler reads.
type countingBody struct {
	io.ReadCloser
	n int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	return n, err
}

// countingWriter keeps the status of the answer and counts the body bytes
// the client receives; net/http drops the body of a HEAD answer.
type countingWriter struct {
	http.ResponseWriter
	head   bool
	status int
	bytes  int64
}

func (w *countingWriter) WriteHeader(status int) {
	if w.status == 0 && (status >= 200 || status == http.StatusSwitchingProtocols) {
		w.status = status // 1xx other than 101 precede the answer
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	if !w.head {
		w.bytes += int64(n)
	}
	return n, err
}

// Unwrap is the writer of net/http, for http.ResponseController.
func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
