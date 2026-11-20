package logship

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxAnswer is the bound of an answer body that every kind passes to post.
// elasticsearch passes maxAnswer plus 1 KiB per line of the batch, as its
// answer holds one item per line.
const maxAnswer = 64 << 10

// maxAnswerHead is the most bytes of the head of an answer the transport of
// a target reads; a longer head fails the attempt as network.
const maxAnswerHead = 64 << 10

// webClient sends the requests of one target of an HTTP kind: one client
// per target that follows no redirect, takes no proxy from the environment,
// keeps no cookie and speaks TLS 1.2 at least.
type webClient struct {
	client    *http.Client
	transport *http.Transport
	endpoint  string // the URL of every request; never in an error or a log line
	agent     string
	auth      string // the Authorization of the secret, empty without one
	clock     clock
}

// newWebClient makes the client of t. With ca_file its certificates are the
// only trusted roots.
func newWebClient(t Target, secret string, e env) (*webClient, error) {
	conf := &tls.Config{MinVersion: tls.VersionTLS12}
	if t.CAFile != "" {
		roots, err := rootsOf(t.CAFile)
		if err != nil {
			return nil, err
		}
		conf.RootCAs = roots
	}
	transport := &http.Transport{
		Proxy:              nil,
		DialContext:        (&net.Dialer{}).DialContext,
		TLSClientConfig:    conf,
		DisableCompression: true,
		IdleConnTimeout:    90 * time.Second,
	}
	transport.MaxResponseHeaderBytes = maxAnswerHead
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return &webClient{client: client, transport: transport, endpoint: t.endpoint().String(), agent: userAgent(e.version),
		auth: authorization(t, secret), clock: e.clock}, nil
}

// rootsOf reads the certificates of a PEM file.
func rootsOf(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the CA file the administrator named in the setting
	if err != nil {
		return nil, fmt.Errorf("ca_file not read: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("ca_file %s holds no PEM certificate", path)
	}
	return roots, nil
}

// authorization is the header value of the secret: HTTP Basic with a user,
// else Splunk for splunk-hec, ApiKey for elasticsearch and Bearer for the
// other kinds.
func authorization(t Target, secret string) string {
	switch {
	case secret == "":
		return ""
	case t.User != "":
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(t.User+":"+secret))
	case t.Kind == kindSplunkHEC:
		return "Splunk " + secret
	case t.Kind == kindElasticsearch:
		return "ApiKey " + secret
	}
	return "Bearer " + secret
}

// userAgent is the User-Agent of every request.
func userAgent(version string) string {
	if version == "" {
		return "pipesmith-registry"
	}
	return "pipesmith-registry/" + version
}

// answer is what an adapter reads of an answer.
type answer struct {
	status int
	header http.Header
	body   []byte // the first bytes, up to the limit of post
}

// post sends body to the endpoint with the headers of the target and
// header, and reads at most limit bytes of the answer body; a limit of 0 or
// below reads maxAnswer. A failure of the transport is a *Failure that is
// tried again.
func (w *webClient) post(ctx context.Context, contentType string, body []byte, header http.Header, limit int64) (answer, error) {
	if limit <= 0 {
		limit = maxAnswer
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.endpoint, bytes.NewReader(body))
	if err != nil {
		return answer{}, &Failure{Class: "request", Final: true}
	}
	for key, values := range header {
		req.Header[key] = values
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", w.agent)
	if w.auth != "" {
		req.Header.Set("Authorization", w.auth)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return answer{}, transportFailure(err)
	}
	// The bound may leave bytes unread; the connection is not reused then. A
	// read that fails after the status is a body cut short.
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
	return answer{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

// check is nil for a 2xx answer. 429, 502, 503 and 504 are tried again,
// after the pause of Retry-After when the answer gives one; every other
// status, a redirect among them, drops the lines at once.
func (w *webClient) check(a answer) error {
	if a.status >= 200 && a.status <= 299 {
		return nil
	}
	f := &Failure{Class: "status " + strconv.Itoa(a.status), Final: true}
	switch a.status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		f.Final, f.RetryAfter = false, retryAfter(a.header.Get("Retry-After"), w.clock.Now())
	}
	return f
}

// close closes the idle connections, so no goroutine of the transport stays.
func (w *webClient) close() { w.transport.CloseIdleConnections() }

// retryAfter reads Retry-After as seconds or as an HTTP date, at most
// maxPause; 0 when it is absent, unreadable or not in the future.
func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(min(secs, int(maxPause/time.Second))) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return min(at.Sub(now), maxPause)
	}
	return 0
}

// transportFailure is the class of a failure of the transport, which is
// tried again: stopped (the sender stopped), timeout, tls, dial or network.
func transportFailure(err error) *Failure {
	var (
		netErr    net.Error
		opErr     *net.OpError
		verify    *tls.CertificateVerificationError
		alert     tls.AlertError
		record    tls.RecordHeaderError
		authority x509.UnknownAuthorityError
		hostname  x509.HostnameError
	)
	class := "network"
	switch {
	case errors.Is(err, context.Canceled):
		class = "stopped"
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout():
		class = "timeout"
	case errors.As(err, &verify) || errors.As(err, &alert) || errors.As(err, &record) || errors.As(err, &authority) ||
		errors.As(err, &hostname):
		class = "tls"
	case errors.As(err, &opErr) && opErr.Op == "dial":
		class = "dial"
	}
	return &Failure{Class: class}
}
