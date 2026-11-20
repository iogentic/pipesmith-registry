package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/version"
)

// The limits of the client.
const (
	// MaxRedirects is the number of redirects a request follows (decision
	// 5.11); the next one is refused.
	MaxRedirects = 5
	// DefaultTotalTimeout bounds one call of Do with its retries and the
	// reading of the body, when the options give none.
	DefaultTotalTimeout = time.Hour
	maxHeaderBytes      = 1 << 20
	idleConnections     = 16
	logger              = "pipesmith_registry.proxy"
)

// The errors a caller can tell apart.
var (
	ErrBodyTooLarge   = errors.New("the answer of the remote is larger than the limit of the repository")
	ErrRedirects      = fmt.Errorf("the remote redirected more than %d times", MaxRedirects)
	ErrRedirectScheme = errors.New("the remote redirected to an address that is neither http nor https")
	ErrRedirectUser   = errors.New("the remote redirected to an address with a user part")
)

// contextDialer opens a connection; *net.Dialer is one.
type contextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Options are what a Client of one repository is built from.
type Options struct {
	// Repository names the repository in errors and log lines.
	Repository string
	// Settings are the typed settings of the repository (task 2).
	Settings repos.ProxySettings
	// Secret is the secret half of the sign in: the password of Basic or
	// the bearer token. Empty sends no credentials. It never reaches a log
	// line or an error.
	Secret string
	// MaxBodyBytes caps the body of an answer; 0 is
	// repos.DefaultMaxBodyBytes, the limit of a write.
	MaxBodyBytes int64
	// TotalTimeout bounds one call of Do; 0 is DefaultTotalTimeout.
	TotalTimeout time.Duration
	// Version names this server in the user agent; empty is version.Version.
	Version string
	// Logger takes one line per repeated request; nil is slog.Default().
	Logger *slog.Logger

	dialer   contextDialer    // the tests put a dialer that never connects here
	classify classifier       // the kind of a dialled address; nil is kindOf, tests call a remote on loopback public
	tokens   bool             // the token flow of the registry protocol answers a Bearer challenge (a Docker proxy, tokens.go)
	now      func() time.Time // the clock of the tokens; nil is time.Now
}

// origin is scheme, host name and port of an address.
type origin struct{ scheme, host, port string }

func originOf(u *url.URL) origin {
	o := origin{scheme: strings.ToLower(u.Scheme), host: strings.ToLower(u.Hostname()), port: u.Port()}
	if o.port == "" {
		o.port = map[string]string{"http": "80", "https": "443"}[o.scheme]
	}
	return o
}

// Client asks the remote of one repository. The remote address is the base
// of every request (Join); the proxy variables of the environment play no
// part (decision 5.14); TLS uses the system roots and at least TLS 1.2, and
// no option turns the check of certificates off. It is safe for use by
// several goroutines.
type Client struct {
	repo      string
	remote    *url.URL
	origin    origin
	auth      repos.RemoteAuth
	secret    string
	agent     string
	retries   int
	limit     int64
	total     time.Duration
	log       *slog.Logger
	transport *http.Transport
	http      *http.Client
	tokens    *tokens          // the tokens of the realms of the remote; nil without the token flow
	tokenHTTP *http.Client     // the requests to a realm: a redirect never carries the credentials
	now       func() time.Time // the clock of the tokens
}

// NewClient builds the client of a repository. It refuses a remote address
// that is not http or https, has no host, a user part or a fragment, and a
// timeout below 1 s.
func NewClient(o Options) (*Client, error) {
	remote, err := url.Parse(o.Settings.RemoteURL)
	switch {
	case err != nil || o.Settings.RemoteURL == "":
		return nil, fmt.Errorf("repository %q: the remote address cannot be read", o.Repository)
	case remote.Scheme != "http" && remote.Scheme != "https":
		return nil, fmt.Errorf("repository %q: the remote address is neither http nor https", o.Repository)
	case remote.Hostname() == "" || remote.User != nil || strings.Contains(o.Settings.RemoteURL, "#"):
		return nil, fmt.Errorf("repository %q: the remote address needs a host, no user part and no fragment", o.Repository)
	case o.Settings.TimeoutSeconds < 1:
		return nil, fmt.Errorf("repository %q: the timeout is below 1 s", o.Repository)
	}
	timeout := time.Duration(o.Settings.TimeoutSeconds) * time.Second
	own := originOf(remote)
	g := &guard{host: own.host, port: own.port, classify: o.classify, publicOnly: o.Settings.PublicOnly}
	if g.classify == nil {
		g.classify = realKind
	}
	// the remote address of the repository is dialled as it is; every other
	// address, which only the remote can have named, passes the check of
	// ruling P5-8 on the address the dialer resolved
	var dialer, named contextDialer = &net.Dialer{KeepAlive: 30 * time.Second},
		&net.Dialer{KeepAlive: 30 * time.Second, ControlContext: g.check}
	if o.dialer != nil {
		dialer, named = o.dialer, o.dialer
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if g.own(address) && !g.publicOnly {
				return dialer.DialContext(ctx, network, address)
			}
			return named.DialContext(ctx, network, address)
		},
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    timeout,
		ResponseHeaderTimeout:  timeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: maxHeaderBytes,
		DisableCompression:     true, // the bytes as the remote has them, for their digests
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           idleConnections,
		MaxIdleConnsPerHost:    idleConnections,
		IdleConnTimeout:        90 * time.Second,
	}
	c := &Client{repo: o.Repository, remote: remote, origin: originOf(remote), auth: o.Settings.Auth, secret: o.Secret,
		retries: o.Settings.Retries, limit: o.MaxBodyBytes, total: o.TotalTimeout, log: o.Logger, transport: transport}
	if c.limit <= 0 {
		c.limit = repos.DefaultMaxBodyBytes
	}
	if c.total <= 0 {
		c.total = DefaultTotalTimeout
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	agent := o.Version
	if agent == "" {
		agent = version.Version
	}
	c.agent = "Pipesmith-Registry/" + agent
	if o.Settings.UserAgentSuffix != "" {
		c.agent += " " + o.Settings.UserAgentSuffix
	}
	c.http = &http.Client{Transport: c.logged(transport), CheckRedirect: c.checkRedirect}
	c.now = o.now
	if c.now == nil {
		c.now = time.Now
	}
	if o.tokens {
		c.tokens = newTokens()
		c.tokenHTTP = &http.Client{Transport: c.logged(transport), CheckRedirect: func(req *http.Request, via []*http.Request) error {
			err := c.checkRedirect(req, via)
			req.Header.Del("Authorization") // a redirect of the realm never carries the credentials (ruling P5-10)
			return err
		}}
	}
	return c, nil
}

// Close closes the idle connections of the client.
func (c *Client) Close() { c.transport.CloseIdleConnections() }

// checkRedirect follows at most MaxRedirects redirects, http and https
// only, and sends the credentials to the origin of the repository only. The
// standard library keeps Authorization on a redirect to another port of
// the same host name, so the client decides itself.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	switch {
	case len(via) > MaxRedirects:
		return ErrRedirects
	case req.URL.Scheme != "http" && req.URL.Scheme != "https":
		return ErrRedirectScheme
	case req.URL.User != nil:
		return ErrRedirectUser
	}
	if originOf(req.URL) != c.origin {
		req.Header.Del("Authorization")
	}
	req.Header.Del("Referer")
	return nil
}

// signs reports whether the client has credentials to send.
func (c *Client) signs() bool { return c.auth.Built() && c.secret != "" }

func (c *Client) credentials() string {
	if c.auth.Type == repos.AuthBearer {
		return "Bearer " + c.secret
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.auth.Username+":"+c.secret))
}

// challenged reports whether an answer of the origin asks for the scheme of
// the credentials.
func (c *Client) challenged(resp *http.Response) bool {
	if resp.StatusCode != http.StatusUnauthorized || originOf(resp.Request.URL) != c.origin {
		return false
	}
	scheme := map[string]string{repos.AuthBasic: "basic", repos.AuthBearer: "bearer"}[c.auth.Type]
	for _, value := range resp.Header.Values("WWW-Authenticate") {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == scheme || strings.HasPrefix(value, scheme+" ") || strings.Contains(value, ", "+scheme+" ") {
			return true
		}
	}
	return false
}

// Do asks the remote for the request path and query (Join), with the
// headers of the caller, the user agent of this server and, at once or after
// a challenge of the remote, the credentials of the repository. It returns
// the answer of the remote, whatever its status, with a body that ends with
// ErrBodyTooLarge at the first byte past the limit; a declared length past
// it returns ErrBodyTooLarge at once. GET and HEAD go out once more per
// retry of the settings after a failure before an answer: a connection or
// TLS error, a timeout of the dial or of the header, a connection the
// remote closed. Nothing else is repeated. The caller closes the body.
func (c *Client) Do(ctx context.Context, method, path, query string, header http.Header) (*http.Response, error) {
	target, err := Join(c.remote, path, query)
	if err != nil {
		return nil, fmt.Errorf("repository %q: %w", c.repo, err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.total)
	name, first := imageName(path), ""
	if c.tokens != nil {
		if value := c.tokens.forName(name, c.now()); value != "" {
			first = "Bearer " + value // the token the last challenge of this image led to, while it lasts
		}
	}
	if first == "" && c.signs() && c.auth.Preemptive {
		first = c.credentials()
	}
	resp, err := c.attempts(ctx, method, target, header, first)
	withToken := strings.HasPrefix(first, "Bearer ") && c.tokens != nil
	if err == nil && resp.StatusCode == http.StatusUnauthorized && originOf(resp.Request.URL) == c.origin {
		resp, withToken, err = c.answerChallenge(ctx, method, target, header, resp, name, first)
	}
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.ContentLength > c.limit {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("repository %q: %w: %d bytes declared, %d allowed", c.repo, ErrBodyTooLarge,
			resp.ContentLength, c.limit)
	}
	resp.Body = &limited{body: resp.Body, left: c.limit, cancel: cancel, repo: c.repo, limit: c.limit, withToken: withToken}
	if resp.Request != nil {
		seen := *resp.Request
		seen.Header = resp.Request.Header.Clone()
		seen.Header.Del("Authorization")
		resp.Request = &seen
	}
	return resp, nil
}

// answerChallenge answers a 401 of the origin once: with the credentials of
// the repository on a challenge of their scheme, or, for a Docker proxy, with
// a token of the realm a Bearer challenge names. Without an answer, and when
// the realm refuses the token, the 401 stands. withToken reports an answer
// to a request that carried a token of the realm.
func (c *Client) answerChallenge(ctx context.Context, method string, target *url.URL, header http.Header, resp *http.Response,
	name, first string) (_ *http.Response, withToken bool, _ error) {
	drain := func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // a short challenge, so the connection stays
		_ = resp.Body.Close()
	}
	if first == "" && c.signs() && c.challenged(resp) {
		drain()
		resp, err := c.attempts(ctx, method, target, header, c.credentials())
		return resp, false, err
	}
	if c.tokens == nil {
		return resp, false, nil
	}
	ch, ok := bearerChallenge(resp)
	if !ok {
		return resp, false, nil
	}
	drain()
	value, err := c.token(ctx, ch, name, strings.TrimPrefix(first, "Bearer "))
	if errors.Is(err, errTokenRefused) || errors.Is(err, errScopeRefused) {
		resp.Body = http.NoBody
		return resp, errors.Is(err, errScopeRefused), nil // a refused scope refuses the name, not the credentials
	}
	if err != nil {
		return nil, false, err
	}
	resp, err = c.attempts(ctx, method, target, header, "Bearer "+value)
	return resp, true, err
}

// credentialsRefused reports whether a 401 of the remote refuses the
// credentials of the repository: not when the request carried a token of
// the realm, since a registry answers 401 to a valid token for a name its
// account may not pull (insufficient_scope of the token specification), and
// not when the realm refused the token of that scope only (errScopeRefused).
func credentialsRefused(resp *http.Response) bool {
	l, ok := resp.Body.(*limited)
	return !ok || !l.withToken
}

// attempts sends the request once, and for GET and HEAD once more per retry
// after a failure before an answer, with authorization as its Authorization
// header, none when it is empty.
func (c *Client) attempts(ctx context.Context, method string, target *url.URL, header http.Header, authorization string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("repository %q: %w", c.repo, err)
		}
		joined := *target
		req.URL = &joined // the joined address with its encoded segments, not a parse of its text
		for key, values := range header {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
		req.Header.Del("Authorization")
		req.Header.Set("User-Agent", c.agent)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := c.http.Do(req)
		if err == nil {
			return resp, nil
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		err = withoutUserPart(err)
		if attempt >= c.retries || !repeatable(ctx, method, err) {
			return nil, fmt.Errorf("repository %q: %w", c.repo, err)
		}
		c.log.Warn(fmt.Sprintf("repository %s: attempt %d of %d failed, trying again: %q", c.repo, attempt+1, c.retries+1, err),
			"logger", logger)
	}
}

// withoutUserPart takes a user part out of the address an error names. The
// standard library puts the Location of a refused redirect into its error as
// the remote sent it, user part and all.
func withoutUserPart(err error) error {
	var named *url.Error
	if !errors.As(err, &named) {
		return err
	}
	u, parseErr := url.Parse(named.URL)
	switch {
	case parseErr != nil:
		named.URL = "an address that cannot be read"
	case u.User != nil:
		u.User = nil
		named.URL = u.String()
	}
	return err
}

// repeatable reports whether a failed request goes out once more: GET and
// HEAD after a failure before an answer, while the time of the call lasts.
// A refused redirect and a certificate the system roots do not trust stay
// as they are.
func repeatable(ctx context.Context, method string, err error) bool {
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var verify *tls.CertificateVerificationError
	switch {
	case method != http.MethodGet && method != http.MethodHead, ctx.Err() != nil:
		return false
	case errors.Is(err, ErrRedirects), errors.Is(err, ErrRedirectScheme), errors.Is(err, ErrRedirectUser), errors.Is(err, ErrInnerAddress):
		return false
	case errors.As(err, &unknown), errors.As(err, &invalid), errors.As(err, &hostname), errors.As(err, &verify):
		return false
	}
	return true
}

// limited is a body that ends with ErrBodyTooLarge at the first byte past
// the limit and then closes the answer; closing it ends the time of the call.
type limited struct {
	body   io.ReadCloser
	left   int64
	cancel context.CancelFunc
	err    error
	repo   string
	limit  int64
	// withToken: the answer is to a request with a token of the realm
	withToken bool
}

func (l *limited) Read(p []byte) (int, error) {
	if l.err != nil {
		return 0, l.err
	}
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1]
	}
	n, err := l.body.Read(p)
	if int64(n) > l.left {
		l.err = fmt.Errorf("repository %q: %w: more than %d bytes", l.repo, ErrBodyTooLarge, l.limit)
		_ = l.body.Close()
		l.cancel()
		return int(l.left), l.err
	}
	l.left -= int64(n)
	return n, err
}

func (l *limited) Close() error {
	err := l.body.Close()
	l.cancel()
	return err
}
