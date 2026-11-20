package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// The token flow of the registry protocol for a Docker proxy (Token
// Authentication Specification, CNCF Distribution, read 2026-11-19): the
// remote answers 401 with a challenge "Bearer realm=...,service=...,
// scope=...", the client asks the realm with GET and the parameters service
// and scope for a token, and sends its request again with "Authorization:
// Bearer <token>". The answer of the realm holds token or access_token, and
// expires_in, 60 s when it gives none, counted from issued_at or from the
// end of the exchange.
//
// Ruling P5-10 of the phase 5 session: the credentials of the repository go
// to the realm only when the challenge came from the origin of the
// repository and the realm is https or has that origin; a redirect from the
// realm never carries them. A token lives in the memory of the client of
// one repository, per scope, until it runs out, and never reaches a file or
// a log line. The realm is dialled under ruling P5-8.

// ErrToken is a token flow that came to no token: a challenge whose realm is
// no http or https address, or an answer of the realm without a token.
var ErrToken = errors.New("the token flow of the remote gave no token")

// errTokenRefused is a realm that answered 401 to the credentials of the
// repository: the 401 of the remote stands as its answer, and it refuses
// the credentials.
var errTokenRefused = errors.New("the realm refused the credentials")

// errScopeRefused is a realm that refused the token of one scope: 403, or
// 401 to a request that carried no credentials (finding 5-4 of review 5).
// The 401 of the remote stands as the answer for that name only.
var errScopeRefused = errors.New("the realm refused the token of the scope")

const (
	defaultTokenLife = 60 * time.Second
	maxTokenAnswer   = 1 << 20
)

// challenge is a Bearer challenge of the remote.
type challenge struct{ realm, service, scope string }

func (c challenge) key() string { return c.realm + "\x00" + c.service + "\x00" + c.scope }

// bearerChallenge is the first Bearer challenge with a realm of a 401: the
// auth parameters of RFC 9110, section 11.2, a value quoted or a token.
func bearerChallenge(resp *http.Response) (challenge, bool) {
	for _, value := range resp.Header.Values("WWW-Authenticate") {
		scheme, params, _ := strings.Cut(strings.TrimSpace(value), " ")
		if !strings.EqualFold(scheme, "bearer") {
			continue
		}
		found := authParams(params)
		if found["realm"] != "" {
			return challenge{realm: found["realm"], service: found["service"], scope: found["scope"]}, true
		}
	}
	return challenge{}, false
}

// authParams reads name=value pairs separated by commas; a quoted value may
// hold commas and escaped characters. A name is in lower case.
func authParams(s string) map[string]string {
	out := map[string]string{}
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		name, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		name = strings.ToLower(strings.TrimSpace(strings.TrimLeft(name, ", ")))
		rest = strings.TrimSpace(rest)
		var value strings.Builder
		if strings.HasPrefix(rest, `"`) {
			i := 1
			for ; i < len(rest) && rest[i] != '"'; i++ {
				if rest[i] == '\\' && i+1 < len(rest) {
					i++
				}
				value.WriteByte(rest[i])
			}
			rest = rest[min(i+1, len(rest)):]
		} else {
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				end = len(rest)
			}
			value.WriteString(strings.TrimSpace(rest[:end]))
			rest = rest[end:]
		}
		if _, seen := out[name]; !seen {
			out[name] = value.String()
		}
		s = strings.TrimPrefix(strings.TrimSpace(rest), ",")
	}
	return out
}

// tokens are the tokens of one client: per scope (with realm and service),
// and the scope the last challenge named for each image name, so that the
// next request of that name carries its token at once.
type tokens struct {
	mu      sync.Mutex
	byScope map[string]token
	byName  map[string]string
}

type token struct {
	value string
	until time.Time
}

func newTokens() *tokens {
	return &tokens{byScope: map[string]token{}, byName: map[string]string{}}
}

// forName is the token the last challenge of a name led to, while it lasts.
func (t *tokens) forName(name string, now time.Time) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tok, ok := t.byScope[t.byName[name]]; ok && now.Before(tok.until) {
		return tok.value
	}
	return ""
}

// forChallenge is the token of a challenge while it lasts, unless it is the
// one the remote just refused.
func (t *tokens) forChallenge(c challenge, now time.Time, refused string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tok, ok := t.byScope[c.key()]; ok && now.Before(tok.until) && tok.value != refused {
		return tok.value
	}
	return ""
}

// maxTokens bounds the tokens of a client and the names that point at
// them. Past it all go, and the next request of a name asks the realm
// again.
const maxTokens = 512

func (t *tokens) keep(c challenge, name string, tok token) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, held := t.byScope[c.key()]; !held && len(t.byScope) >= maxTokens {
		clear(t.byScope)
	}
	t.byScope[c.key()] = tok
	t.name(c, name)
}

func (t *tokens) named(c challenge, name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.name(c, name)
}

// name points name at the scope of c; t.mu is held.
func (t *tokens) name(c challenge, name string) {
	if name == "" {
		return
	}
	if _, held := t.byName[name]; !held && len(t.byName) >= maxTokens {
		clear(t.byName)
	}
	t.byName[name] = c.key()
}

// imageName is the name of an image a request path of the registry protocol
// names: v2/<name>/manifests/..., v2/<name>/blobs/... or v2/<name>/tags/list;
// empty for another path.
func imageName(path string) string {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(path, "/"), "v2/")
	if !ok {
		return ""
	}
	if name, ok := strings.CutSuffix(rest, "/tags/list"); ok {
		return name
	}
	for _, mark := range []string{"/manifests/", "/blobs/"} {
		if i := strings.LastIndex(rest, mark); i > 0 {
			return rest[:i]
		}
	}
	return ""
}

// mayCarry reports whether the credentials of the repository may go to the
// realm of a challenge of its origin (ruling P5-10): a realm over https, or
// one of the origin of the repository.
func (c *Client) mayCarry(realm *url.URL) bool {
	return c.signs() && c.auth.Type == repos.AuthBasic && (strings.EqualFold(realm.Scheme, "https") || originOf(realm) == c.origin)
}

// token is the token for a challenge of the origin: the one the client
// keeps while it lasts, else one the realm gives now. refused is the token
// the remote refused with this challenge, which is not used again.
func (c *Client) token(ctx context.Context, ch challenge, name, refused string) (string, error) {
	if value := c.tokens.forChallenge(ch, c.now(), refused); value != "" {
		c.tokens.named(ch, name)
		return value, nil
	}
	realm, err := url.Parse(ch.realm)
	if err != nil || realm.Scheme != "http" && realm.Scheme != "https" || realm.Hostname() == "" || realm.User != nil {
		return "", fmt.Errorf("repository %q: %w: the challenge names a realm that is no http or https address", c.repo, ErrToken)
	}
	realm.Fragment, realm.RawFragment = "", ""
	params := url.Values{}
	if ch.service != "" {
		params.Set("service", ch.service)
	}
	if ch.scope != "" {
		params.Set("scope", ch.scope)
	}
	if encoded := params.Encode(); encoded != "" {
		if realm.RawQuery != "" {
			realm.RawQuery += "&"
		}
		realm.RawQuery += encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", fmt.Errorf("repository %q: %w: %v", c.repo, ErrToken, err)
	}
	req.Header.Set("User-Agent", c.agent)
	carried := c.mayCarry(realm)
	if carried {
		req.Header.Set("Authorization", c.credentials())
	}
	resp, err := c.tokenHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("repository %q: the token request failed: %w", c.repo, withoutUserPart(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized && carried:
		return "", errTokenRefused
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", errScopeRefused
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("repository %q: the realm answered %d to the token request", c.repo, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenAnswer+1))
	if err != nil {
		return "", fmt.Errorf("repository %q: the answer of the realm broke off: %w", c.repo, err)
	}
	var answer struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		IssuedAt    string `json:"issued_at"`
	}
	// the error of the decoder may quote the answer, which holds the token: it is not passed on
	if len(data) > maxTokenAnswer || json.Unmarshal(data, &answer) != nil {
		return "", fmt.Errorf("repository %q: %w: the answer of the realm is no token document", c.repo, ErrToken)
	}
	value := answer.AccessToken
	if value == "" {
		value = answer.Token
	}
	if value == "" || strings.ContainsFunc(value, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return "", fmt.Errorf("repository %q: %w: the answer of the realm holds no token that a header can carry", c.repo, ErrToken)
	}
	life := defaultTokenLife
	if answer.ExpiresIn > 0 {
		life = time.Duration(min(answer.ExpiresIn, int64(24*time.Hour/time.Second))) * time.Second
	}
	start := c.now()
	if issued, err := time.Parse(time.RFC3339, answer.IssuedAt); err == nil && issued.Before(start) {
		start = issued
	}
	c.tokens.keep(ch, name, token{value: value, until: start.Add(life)})
	return value, nil
}
