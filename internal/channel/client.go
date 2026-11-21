package channel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Refusals of a request.
var (
	ErrTooLarge = errors.New("larger than its cap")
	ErrShort    = errors.New("shorter than its Content-Length")
	ErrRedirect = errors.New("redirect refused")
	ErrToken    = errors.New("channel token")
	ErrDeadline = errors.New("stage deadline")
)

const maxRedirects = 3

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// client sends the requests of the fetch stage: no proxy, no compression,
// a redirect only to the same scheme, host and port and at most
// maxRedirects times, the token only to the configured server and only over
// https or to loopback, and the timeouts of the settings.
type client struct {
	http  *http.Client
	total time.Duration
	// stage ends with the deadline of the whole fetch stage, stageLimit long.
	stage      context.Context
	stageLimit time.Duration
	// github sends the headers of the GitHub API and reads its rate limit.
	github bool
	now    func() time.Time
}

func newClient(set *Settings, token string, dial dialFunc, tlsConfig *tls.Config) *client {
	if dial == nil {
		dial = (&net.Dialer{Timeout: set.Timeouts.Dial}).DialContext
	}
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dial,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   set.Timeouts.Dial,
		ResponseHeaderTimeout: set.Timeouts.Header,
		DisableCompression:    true,
		MaxIdleConnsPerHost:   2,
	}
	c := &client{
		total:  set.Timeouts.Total,
		stage:  context.Background(),
		github: set.GitHub != nil,
		now:    time.Now,
	}
	redirect := checkRedirect
	if c.github && set.Server.Scheme == "https" && set.Server.Hostname() == "api.github.com" {
		redirect = gitHubRedirect
	}
	c.http = &http.Client{
		Transport:     &tokenTransport{base: transport, server: set.Server, token: token},
		CheckRedirect: redirect,
	}
	return c
}

// gitHubRedirect is checkRedirect that also lets api.github.com send an
// asset to the hosts it stores assets on, over https. The checks after the
// download rest on the signature, not on the host.
func gitHubRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme == "https" && slices.Contains(gitHubAssetHosts, req.URL.Hostname()) && req.URL.Port() == "" {
		if len(via) > maxRedirects {
			return fmt.Errorf("%w: more than %d redirects", ErrRedirect, maxRedirects)
		}
		return nil
	}
	return checkRedirect(req, via)
}

// origin is scheme, host and port of u, the port spelled out.
func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return u.Scheme + "://" + net.JoinHostPort(u.Hostname(), port)
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if from, to := origin(via[0].URL), origin(req.URL); from != to {
		return fmt.Errorf("%w: %s answered with a redirect to %s", ErrRedirect, from, bounded(to))
	}
	if len(via) > maxRedirects {
		return fmt.Errorf("%w: more than %d redirects", ErrRedirect, maxRedirects)
	}
	return nil
}

// maxShown bounds a text of the forge in a message.
const maxShown = 100

// bounded is text quoted, cut after maxShown bytes with its length named.
func bounded(text string) string {
	if len(text) <= maxShown {
		return strconv.Quote(text)
	}
	return fmt.Sprintf("%s... (%d bytes)", strconv.Quote(text[:maxShown]), len(text))
}

// tokenAllowed reports whether a token may go to u: over https, or to a
// loopback host.
func tokenAllowed(u *url.URL) bool {
	return u.Scheme == "https" || Loopback(u.Hostname())
}

// tokenTransport adds the token to a request for the configured server and
// takes any Authorization header off every other request.
type tokenTransport struct {
	base   http.RoundTripper
	server *url.URL
	token  string
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Del("Authorization")
	if t.token != "" && origin(req.URL) == origin(t.server) && tokenAllowed(req.URL) {
		req.Header.Set("Authorization", "token "+t.token)
	}
	return t.base.RoundTrip(req)
}

// get sends a GET; the caller closes the body and then calls done.
func (c *client) get(target string) (*http.Response, func(), error) {
	ctx, cancel := context.WithTimeout(c.stage, c.total)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	// GitHub refuses a request without User-Agent; the name carries no version
	req.Header.Set("User-Agent", "pipesmith-registry")
	if c.github {
		req.Header.Set("X-GitHub-Api-Version", gitHubVersion)
		if strings.Contains(req.URL.Path, "/releases/assets/") {
			req.Header.Set("Accept", "application/octet-stream")
		} else {
			req.Header.Set("Accept", "application/vnd.github+json")
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		err = c.plain(ctx, err)
		cancel()
		return nil, nil, err
	}
	return resp, cancel, nil
}

// plain drops the address a url.Error carries, which can be the target of a
// redirect, and names a timeout.
func (c *client) plain(ctx context.Context, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(c.stage.Err(), context.Canceled) {
		return context.Canceled
	}
	if c.stage.Err() != nil {
		return c.deadline()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("no complete answer within the total timeout: %w", ctx.Err())
	}
	return err
}

func (c *client) deadline() error {
	return fmt.Errorf("%w: the fetch stage did not end within %v (timeouts.stage_seconds)", ErrDeadline, c.stageLimit)
}

// copyCapped writes the body of a 200 answer to w: a Content-Length above
// limit is refused before the body is read, a body that grows past limit is
// cut off and refused, and a body shorter than its Content-Length is
// refused.
func (c *client) copyCapped(target string, limit int64, w io.Writer) (int64, http.Header, error) {
	resp, done, err := c.get(target)
	if err != nil {
		return 0, nil, err
	}
	defer done()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if c.github {
			if err := rateLimit(resp, c.now()); err != nil {
				return 0, nil, err
			}
		}
		return 0, nil, statusError(resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return 0, nil, fmt.Errorf("%w: Content-Length %d, the cap is %d bytes", ErrTooLarge, resp.ContentLength, limit)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	switch {
	case err != nil && c.stage.Err() != nil:
		return n, nil, c.deadline()
	case n > limit:
		return n, nil, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, limit)
	case errors.Is(err, io.ErrUnexpectedEOF) && resp.ContentLength < 0:
		return n, nil, fmt.Errorf("%w: the answer has none and ended after %d bytes, before its last chunk", ErrShort, n)
	case errors.Is(err, io.ErrUnexpectedEOF), err == nil && resp.ContentLength >= 0 && n != resp.ContentLength:
		return n, nil, fmt.Errorf("%w: %d of %d bytes", ErrShort, n, resp.ContentLength)
	case err != nil:
		return n, nil, c.plain(resp.Request.Context(), err)
	}
	return n, resp.Header, nil
}

type buffer struct{ data []byte }

func (b *buffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

// fetch reads the body of a 200 answer of at most limit bytes.
func (c *client) fetch(target string, limit int64) ([]byte, http.Header, error) {
	var b buffer
	_, header, err := c.copyCapped(target, limit, &b)
	return b.data, header, err
}

// download writes the body of a 200 answer of at most limit bytes to w.
func (c *client) download(target string, limit int64, w io.Writer) (int64, error) {
	n, _, err := c.copyCapped(target, limit, w)
	return n, err
}

// status returns the status of a GET and closes the body unread.
func (c *client) status(target string) (int, error) {
	resp, done, err := c.get(target)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	done()
	return resp.StatusCode, nil
}

var errNotFound = errors.New("answers 404")

func statusError(code int) error {
	if code == http.StatusNotFound {
		return errNotFound
	}
	return errors.New("answers " + strconv.Itoa(code))
}
