package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// outgoing writes one line at level info per request this server sends to a
// remote (task 16 of the phase 5 plan): the repository, the method, scheme,
// host and port of the address dialled, the status or the kind of the
// failure, and the duration up to the header of the answer. Each hop of a
// redirect, each attempt of a retry and each request to the realm of a token
// flow is a round trip and so a line. Path, query and credentials stay out.
// A client exists only while upstream is on, so a default start writes no
// such line. tools/check_outgoing.py reads the lines.
type outgoing struct {
	next http.RoundTripper
	repo string
	log  *slog.Logger
}

func (o outgoing) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := o.next.RoundTrip(req)
	result := ""
	if err != nil {
		result = "failed " + failureKind(err)
	} else {
		result = fmt.Sprintf("status %d", resp.StatusCode)
	}
	where := originOf(req.URL)
	o.log.Info(fmt.Sprintf("outgoing request: repository %q, %s %s host %q port %s, %s, %d ms", o.repo, req.Method,
		where.scheme, where.host, where.port, result, time.Since(start).Milliseconds()), "logger", logger)
	return resp, err
}

// failureKind names the kind of a failure before an answer, never its text,
// which a remote may have chosen.
func failureKind(err error) string {
	var netErr net.Error
	var opErr *net.OpError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	var verify *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	switch {
	case errors.Is(err, ErrInnerAddress):
		return "not dialled (inner address)"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.As(err, &record), errors.As(err, &alert), errors.As(err, &verify), errors.As(err, &unknown),
		errors.As(err, &invalid), errors.As(err, &hostname):
		return "tls"
	case errors.As(err, &opErr), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection"
	}
	return "other"
}

// logged wraps the transport of a client in the line of each request.
func (c *Client) logged(next http.RoundTripper) http.RoundTripper {
	return outgoing{next: next, repo: c.repo, log: c.log}
}
