package testkit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// ProxyHeaders are the request headers a Remote records: those an outbound
// client of a proxy sends, or must not send.
var ProxyHeaders = []string{"Authorization", "Range", "If-None-Match", "If-Modified-Since", "User-Agent", "Accept"}

// Request is one request a Remote received, recorded when it arrived and
// before it was answered.
type Request struct {
	Method string
	Target string      // the path with the query, as the request line carries it
	Host   string      // the Host header
	Header http.Header // the headers of ProxyHeaders the request carried
}

// Answer is what a Remote sends for one request. The constructors below give
// the twelve cases of task 1 of the phase 5 plan: OK, NotModified, NotFound,
// ServerError, SlowHeader, SlowBody, ShortBody, Redirect to the same host or
// to another, BasicChallenge, BearerChallenge and Closed.
type Answer struct {
	Status int // 200 when zero
	Header http.Header
	Body   []byte
	// DeclaredLength, when larger than the body, is sent as Content-Length,
	// and the connection closes after the body: a short body.
	DeclaredLength int64
	// HeaderDelay is the wait before the status line.
	HeaderDelay time.Duration
	// BodyDelay is the wait after the first half of the body.
	BodyDelay time.Duration
	// Close drops the connection without an answer.
	Close bool
	// With Challenge set, a request whose Authorization header is not
	// Authorization gets 401 with Challenge as WWW-Authenticate; one with it
	// gets the rest of the answer.
	Authorization string
	Challenge     string
	// Chunked sends the body without Content-Length, in chunks: a body of
	// unknown length.
	Chunked bool
}

// With returns the answer with one more header.
func (a Answer) With(key, value string) Answer {
	h := a.Header.Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Set(key, value)
	a.Header = h
	return a
}

// OK is 200 with the body as application/octet-stream.
func OK(body string) Answer {
	return Answer{Status: http.StatusOK, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: []byte(body)}
}

// NotModified is 304 without a body.
func NotModified() Answer { return Answer{Status: http.StatusNotModified} }

// NotFound is 404, also the answer of a path without a script.
func NotFound() Answer {
	return Answer{Status: http.StatusNotFound, Header: http.Header{"Content-Type": {"text/plain"}}, Body: []byte("not found")}
}

// ServerError is 500.
func ServerError() Answer {
	return Answer{Status: http.StatusInternalServerError, Header: http.Header{"Content-Type": {"text/plain"}},
		Body: []byte("remote failed")}
}

// SlowHeader is the answer after a wait of d before its status line.
func SlowHeader(d time.Duration, a Answer) Answer {
	a.HeaderDelay = d
	return a
}

// SlowBody is 200 that sends the first half of the body, waits d, then sends
// the rest.
func SlowBody(d time.Duration, body string) Answer {
	a := OK(body)
	a.BodyDelay = d
	return a
}

// ShortBody is 200 with Content-Length declared and fewer bytes, then the
// connection closes.
func ShortBody(declared int64, body string) Answer {
	a := OK(body)
	a.DeclaredLength = declared
	return a
}

// Chunked is 200 with the body in chunks and no Content-Length.
func Chunked(body string) Answer {
	a := OK(body)
	a.Chunked = true
	return a
}

// Redirect answers status with Location: a path for the same host, or an
// address of another remote, or of this one under another name (URLAt).
func Redirect(status int, location string) Answer {
	return Answer{Status: status, Header: http.Header{"Location": {location}}}
}

// BasicChallenge answers 401 with a Basic challenge unless the request
// carries Basic credentials of user and password, which get then.
func BasicChallenge(user, password string, then Answer) Answer {
	then.Authorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
	then.Challenge = `Basic realm="remote"`
	return then
}

// BearerChallenge answers 401 with a Bearer challenge unless the request
// carries the bearer token, which gets then.
func BearerChallenge(token string, then Answer) Answer {
	then.Authorization = "Bearer " + token
	then.Challenge = `Bearer realm="remote"`
	return then
}

// Closed drops the connection without an answer.
func Closed() Answer { return Answer{Close: true} }

// Remote is a scripted HTTP server on loopback for tests of an outbound
// client. Each path answers by its script and every request is recorded. A
// second Remote listens on another port of the same name; URLAt reaches this
// one under another name of loopback. It is safe for use by several
// goroutines and closes when the test ends.
type Remote struct {
	server  *httptest.Server
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	scripts map[string][]Answer
	served  map[string]int
	record  []Request
	running int // requests whose answer is not done
	most    int // the most requests at once since the start or the last Reset
}

// NewRemote starts a remote on 127.0.0.1 and a port the system chooses.
func NewRemote(t testing.TB) *Remote { return startRemote(t, true, nil) }

// NewTLSRemote starts a remote over TLS with a certificate of the test
// server, which no system root signs; TrustingClient trusts it.
func NewTLSRemote(t testing.TB) *Remote {
	return startRemote(t, false, &tls.Config{MinVersion: tls.VersionTLS12})
}

// NewTLSRemoteNamed starts a remote over TLS whose own certificate names the
// DNS names given and no address. A client that asks it by another name fails
// on the host name, and the error of that check lists the names as the
// certificate holds them: a certificate may hold any ASCII in a DNS name.
func NewTLSRemoteNamed(t testing.TB, names ...string) *Remote {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "demo-remote"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), DNSNames: names,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return startRemote(t, false, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})
}

// startRemote starts a remote: plain, or over TLS with the configuration
// given, whose certificate is the one of the test server when it names none.
func startRemote(t testing.TB, plain bool, config *tls.Config) *Remote {
	t.Helper()
	r := &Remote{done: make(chan struct{}), scripts: map[string][]Answer{}, served: map[string]int{}}
	s := httptest.NewUnstartedServer(http.HandlerFunc(r.serve))
	s.Config.ErrorLog = log.New(io.Discard, "", 0) // the cases break connections on purpose
	if plain {
		s.Start()
	} else {
		s.TLS = config
		s.StartTLS()
	}
	r.server = s
	t.Cleanup(r.Close)
	return r
}

// URL is the address of the remote: http://127.0.0.1:<port>, https for TLS.
func (r *Remote) URL() string { return r.server.URL }

// URLAt is the address of the remote under another name of the same
// listener, as http://localhost:<port>.
func (r *Remote) URLAt(host string) string {
	u, err := url.Parse(r.server.URL)
	if err != nil {
		panic(err) // the URL of httptest always parses
	}
	u.Host = net.JoinHostPort(host, u.Port())
	return u.String()
}

// TrustingClient is a client that trusts the certificate of a TLS remote.
func (r *Remote) TrustingClient() *http.Client { return r.server.Client() }

// CertificatePEM is the certificate of a TLS remote in PEM, as a file that
// SSL_CERT_FILE names holds it; nil for a remote without TLS.
func (r *Remote) CertificatePEM() []byte {
	certificate := r.server.Certificate()
	if certificate == nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
}

// Script sets the answers of a path, compared in its escaped form: the n-th
// request gets the n-th answer, and the last one answers every later
// request. A path without a script answers NotFound.
func (r *Remote) Script(path string, answers ...Answer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scripts[path] = slices.Clone(answers)
}

// Requests is a copy of the record, in the order of arrival.
func (r *Remote) Requests() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Request, len(r.record))
	for i, req := range r.record {
		req.Header = req.Header.Clone()
		out[i] = req
	}
	return out
}

// Count is the number of requests for a path since the start or the last
// Reset.
func (r *Remote) Count(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.served[path]
}

// MaxInFlight is the most requests the remote answered at once since the
// start or the last Reset: a request counts from its arrival to the end of
// its answer, its delays included.
func (r *Remote) MaxInFlight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.most
}

// Reset forgets the record, the position of every script and the most
// requests at once.
func (r *Remote) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record = nil
	r.served = map[string]int{}
	r.most = r.running
}

// Close stops the remote. A request that waits for its delay ends at once
// without an answer. A second Close does nothing.
func (r *Remote) Close() {
	r.once.Do(func() {
		close(r.done)
		r.server.Close()
	})
}

func (r *Remote) serve(w http.ResponseWriter, req *http.Request) {
	path := req.URL.EscapedPath()
	header := http.Header{}
	for _, name := range ProxyHeaders {
		key := http.CanonicalHeaderKey(name)
		if values, ok := req.Header[key]; ok {
			header[key] = slices.Clone(values)
		}
	}
	r.mu.Lock()
	r.record = append(r.record, Request{Method: req.Method, Target: req.RequestURI, Host: req.Host, Header: header})
	n := r.served[path]
	r.served[path] = n + 1
	answers := r.scripts[path]
	r.running++
	r.most = max(r.most, r.running)
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running--
		r.mu.Unlock()
	}()
	a := NotFound()
	if len(answers) > 0 {
		a = answers[min(n, len(answers)-1)]
	}
	r.answer(w, req, a)
}

// answer sends a. A wait that ends because the client left or the remote
// closed aborts the answer, so the client sees a broken connection.
func (r *Remote) answer(w http.ResponseWriter, req *http.Request, a Answer) {
	if a.HeaderDelay > 0 && !r.wait(req, a.HeaderDelay) {
		panic(http.ErrAbortHandler)
	}
	if a.Close {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			panic(http.ErrAbortHandler)
		}
		if conn, _, err := hijacker.Hijack(); err == nil {
			_ = conn.Close() // the case is the closed connection
		}
		return
	}
	if a.Challenge != "" && req.Header.Get("Authorization") != a.Authorization {
		w.Header().Set("WWW-Authenticate", a.Challenge)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	for key, values := range a.Header {
		w.Header()[key] = slices.Clone(values)
	}
	status := a.Status
	if status == 0 {
		status = http.StatusOK
	}
	switch {
	case a.Chunked:
	case a.DeclaredLength > int64(len(a.Body)):
		w.Header().Set("Content-Length", strconv.FormatInt(a.DeclaredLength, 10))
	case status != http.StatusNotModified && status != http.StatusNoContent:
		w.Header().Set("Content-Length", strconv.Itoa(len(a.Body)))
	}
	w.WriteHeader(status)
	body := a.Body
	if a.Chunked {
		half := len(body) / 2
		_, _ = w.Write(body[:half]) // a client that left is the case of the test
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		body = body[half:]
	}
	if a.BodyDelay > 0 {
		half := len(body) / 2
		_, _ = w.Write(body[:half]) // a client that left is the case of the test
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if !r.wait(req, a.BodyDelay) {
			panic(http.ErrAbortHandler)
		}
		body = body[half:]
	}
	_, _ = w.Write(body) // a client that left is the case of the test
}

func (r *Remote) wait(req *http.Request, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-req.Context().Done():
		return false
	case <-r.done:
		return false
	}
}
