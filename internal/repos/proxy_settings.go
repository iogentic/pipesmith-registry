package repos

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// The typed settings of a proxy and of a group (section Settings of the
// phase 5 plan). The stored keys are those of the old product, as the
// fixture export holds them: proxy.remoteUrl, proxy.contentMaxAge,
// proxy.metadataMaxAge, negativeCache.enabled, negativeCache.timeToLive,
// httpclient.blocked, httpclient.autoBlock, httpclient.connection.retries,
// .timeout and .userAgentSuffix, httpclient.authentication.type, .username
// and .preemptive, group.memberNames and group.groupWriteMember. The stored
// section is httpclient; httpClient, the spelling of the interface, is read
// when the stored one is missing. A stored integer may carry a zero fraction
// (1440.0). testdata/vectors/proxy_settings.json cites the documentation of
// the defaults and the ranges.

// The ages of decision 5.4, in minutes.
const (
	AgeNever  int64 = -1 // never check the remote again
	AgeAlways int64 = 0  // check the remote on every request
)

// The defaults where an attribute or its whole section is missing: decision
// 5.4 for the ages and the not found cache, the documented values of a new
// proxy for the connection (auto block on and blocked off besides).
const (
	DefaultContentMaxAge  int64 = 1440 // minutes
	DefaultMetadataMaxAge int64 = 1440 // minutes
	DefaultNotFoundTTL    int64 = 1440 // minutes
	DefaultRetries              = 2
	DefaultTimeoutSeconds       = 20
)

// The ranges the old product allows for the connection, and for the ages
// its Java int.
const (
	MaxRetries        = 10
	MaxTimeoutSeconds = 3600
	maxMinutes        = math.MaxInt32
)

// The authentication types this server signs in with (decision 5.12): Basic
// with a user name and a secret, and a fixed bearer token.
const (
	AuthBasic  = "username"
	AuthBearer = "bearerToken"
)

// RemoteAuth is httpclient.authentication without its secret half, which
// lives in a file of its own (decision 5.13).
type RemoteAuth struct {
	// Type is the stored type: AuthBasic, AuthBearer, any other value of the
	// old product as it is (ntlm among them), or empty for none.
	Type string
	// Username is the user name of Basic.
	Username string
	// Preemptive sends the credentials with the first request; else only
	// after a challenge of the remote.
	Preemptive bool
}

// Built reports whether this server signs in with the type itself. A proxy
// whose authentication has another type waits in the state needs secret
// (decision 5.12); that is no settings error.
func (a RemoteAuth) Built() bool { return a.Type == AuthBasic || a.Type == AuthBearer }

// ProxySettings are the typed attributes of a proxy repository.
type ProxySettings struct {
	// RemoteURL is the remote address: remote_url of repositories.json,
	// overridden by proxy.remoteUrl. http or https, with a host, without a
	// user part and without a fragment.
	RemoteURL string
	// ContentMaxAge and MetadataMaxAge are minutes; AgeNever and AgeAlways.
	ContentMaxAge  int64
	MetadataMaxAge int64
	// NotFoundCache keeps a 404 of the remote for NotFoundTTL minutes.
	NotFoundCache bool
	NotFoundTTL   int64
	// Blocked is the state blocked; AutoBlock switches decision 5.3 on.
	Blocked   bool
	AutoBlock bool
	Auth      RemoteAuth
	// Retries, TimeoutSeconds and UserAgentSuffix are the connection.
	Retries         int
	TimeoutSeconds  int
	UserAgentSuffix string
	// RoutingRule is the name of the routing rule of the repository, empty
	// for none (Repo.RoutingRule).
	RoutingRule string
	// PublicOnly is the attribute PublicOnlyKey: the client dials no closed
	// address, not even the remote address (ruling ST11).
	PublicOnly bool
}

// PublicOnlyKey is the server owned attribute of a proxy whose remote a
// subject without the full permission set set or changed (ruling ST11 of
// leg 6-stores). No request body sets it.
const PublicOnlyKey = "remote_public_only"

// GroupSettings are the typed attributes of a group repository.
type GroupSettings struct {
	// Members are group.memberNames, else the members of repositories.json.
	Members []string
	// WritableMember is group.groupWriteMember (decision 5.16): a hosted
	// member of the format of the group that the group lists itself. Empty
	// for none, which keeps the refusal of a write to a group.
	WritableMember string
}

// SettingsError is an attribute document a typed reader refuses. It names
// the repository and the key and never the value, so no user part of an
// address reaches a log line.
type SettingsError struct {
	Repository string
	// Key is the stored key, or "the remote address" for a problem of the
	// address whichever field it came from; empty for a problem of the
	// repository itself.
	Key     string
	Problem string
	// Err is ErrFraction, ErrNoNumber or ErrRange of a number, else nil.
	Err error
}

func (e *SettingsError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("repository %q %s", e.Repository, e.Problem)
	}
	return fmt.Sprintf("repository %q: %s %s", e.Repository, e.Key, e.Problem)
}

func (e *SettingsError) Unwrap() error { return e.Err }

// attributes reads the document of one repository; the first error wins.
type attributes struct {
	repo string
	err  error
}

func (a *attributes) fail(key, problem string, err error) {
	if a.err == nil {
		a.err = &SettingsError{Repository: a.repo, Key: key, Problem: problem, Err: err}
	}
}

// object is the section key of m, nil when it is missing or null.
func (a *attributes) object(m map[string]any, key, name string) map[string]any {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		a.fail(name, "is not an object", nil)
	}
	return obj
}

func (a *attributes) flag(m map[string]any, key, name string, fallback bool) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return fallback
	}
	b, ok := v.(bool)
	if !ok {
		a.fail(name, "is not true or false", nil)
		return fallback
	}
	return b
}

func (a *attributes) text(m map[string]any, key, name string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		a.fail(name, "is not a text", nil)
	}
	return s
}

func (a *attributes) whole(m map[string]any, key, name string, fallback, low, high int64) int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return fallback
	}
	n, err := wholeOf(v)
	switch {
	case err != nil:
		a.fail(name, err.Error(), err)
	case n < low:
		a.fail(name, fmt.Sprintf("is below %d", low), nil)
	case n > high:
		a.fail(name, fmt.Sprintf("is above %d", high), nil)
	default:
		return n
	}
	return fallback
}

// wholeOf is WholeNumber for every form a decoded document holds a number
// in: json.Number as internal/state decodes it, and the float64 and int of
// a document built in memory.
func wholeOf(v any) (int64, error) {
	switch x := v.(type) {
	case json.Number:
		return WholeNumber(x)
	case float64:
		switch {
		case math.IsNaN(x) || math.IsInf(x, 0):
			return 0, ErrNoNumber
		case x != math.Trunc(x):
			return 0, ErrFraction
		case x < math.MinInt64 || x >= math.MaxInt64:
			return 0, ErrRange
		}
		return int64(x), nil
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	}
	return 0, ErrNoNumber
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// ProxySettings reads the proxy settings of the repository: the defaults
// where an attribute or a section is missing, and an error that names
// repository and key for a value it refuses. Without state.sqlite the
// attributes are nil and every default applies.
func (r Repo) ProxySettings() (ProxySettings, error) {
	if r.Type != "proxy" {
		return ProxySettings{}, &SettingsError{Repository: r.Name, Problem: fmt.Sprintf("is %s, not a proxy", r.Type)}
	}
	a := &attributes{repo: r.Name}
	s := ProxySettings{RoutingRule: r.RoutingRule}
	proxy := a.object(r.Attributes, "proxy", "proxy")
	if r.RemoteURL != nil {
		s.RemoteURL = *r.RemoteURL
	}
	if remote := a.text(proxy, "remoteUrl", "proxy.remoteUrl"); remote != "" {
		s.RemoteURL = remote
	}
	s.ContentMaxAge = a.whole(proxy, "contentMaxAge", "proxy.contentMaxAge", DefaultContentMaxAge, AgeNever, maxMinutes)
	s.MetadataMaxAge = a.whole(proxy, "metadataMaxAge", "proxy.metadataMaxAge", DefaultMetadataMaxAge, AgeNever, maxMinutes)
	negative := a.object(r.Attributes, "negativeCache", "negativeCache")
	s.NotFoundCache = a.flag(negative, "enabled", "negativeCache.enabled", true)
	s.NotFoundTTL = a.whole(negative, "timeToLive", "negativeCache.timeToLive", DefaultNotFoundTTL, 0, maxMinutes)
	section := "httpclient"
	if stored, ok := r.Attributes[section]; !ok || stored == nil {
		if _, camel := r.Attributes["httpClient"]; camel {
			section = "httpClient"
		}
	}
	client := a.object(r.Attributes, section, section)
	s.Blocked = a.flag(client, "blocked", section+".blocked", false)
	s.AutoBlock = a.flag(client, "autoBlock", section+".autoBlock", true)
	connection := a.object(client, "connection", section+".connection")
	s.Retries = int(a.whole(connection, "retries", section+".connection.retries", DefaultRetries, 0, MaxRetries))
	s.TimeoutSeconds = int(a.whole(connection, "timeout", section+".connection.timeout", DefaultTimeoutSeconds, 1,
		MaxTimeoutSeconds))
	suffix := a.text(connection, "userAgentSuffix", section+".connection.userAgentSuffix")
	if strings.ContainsFunc(suffix, isControl) {
		a.fail(section+".connection.userAgentSuffix", "has a control character", nil)
	} else {
		s.UserAgentSuffix = suffix
	}
	s.Auth = a.auth(client, section+".authentication")
	s.PublicOnly = r.Attributes[PublicOnlyKey] == true
	if a.err != nil {
		return ProxySettings{}, a.err
	}
	if err := checkRemote(r.Name, s.RemoteURL); err != nil {
		return ProxySettings{}, err
	}
	return s, nil
}

// auth reads the authentication section, name being its stored key.
func (a *attributes) auth(client map[string]any, name string) RemoteAuth {
	m := a.object(client, "authentication", name)
	if m == nil {
		return RemoteAuth{}
	}
	auth := RemoteAuth{Type: a.text(m, "type", name+".type"), Username: a.text(m, "username", name+".username"),
		Preemptive: a.flag(m, "preemptive", name+".preemptive", false)}
	switch {
	case auth.Type == "":
		a.fail(name+".type", "is missing", nil)
	case auth.Type == AuthBasic && auth.Username == "":
		a.fail(name+".username", "is missing for the type username", nil)
	}
	return auth
}

// checkRemote refuses a remote address that is not http or https, has no
// host, a user part, a fragment or a port outside 1 to 65535 (decision
// 5.11). The value is never part of the error.
func checkRemote(repo, raw string) error {
	if raw == "" {
		return &SettingsError{Repository: repo, Problem: "has no remote address: neither remote_url nor proxy.remoteUrl"}
	}
	fail := func(problem string) error {
		return &SettingsError{Repository: repo, Key: "the remote address", Problem: problem}
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fail("cannot be read as an address")
	case u.Scheme == "":
		return fail("has no scheme")
	case u.Scheme != "http" && u.Scheme != "https":
		return fail(fmt.Sprintf("has the scheme %s, not http or https", u.Scheme))
	case u.User != nil:
		return fail("has a user part")
	case u.Hostname() == "":
		return fail("has no host")
	case strings.Contains(raw, "#"):
		return fail("has a fragment")
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fail("has a port outside 1 to 65535")
		}
	}
	return nil
}

// GroupSettings reads the settings of the group of that name. The writable
// member must be a hosted repository of the format of the group that the
// group lists itself; a member of a group inside the group does not count.
func (rs *Repositories) GroupSettings(name string) (GroupSettings, error) {
	r, ok := rs.Get(name)
	if !ok {
		return GroupSettings{}, &SettingsError{Repository: name, Problem: "does not exist"}
	}
	if r.Type != "group" {
		return GroupSettings{}, &SettingsError{Repository: name, Problem: fmt.Sprintf("is %s, not a group", r.Type)}
	}
	a := &attributes{repo: name}
	group := a.object(r.Attributes, "group", "group")
	g := GroupSettings{Members: slices.Clone(r.Members)}
	if v, ok := group["memberNames"]; ok && v != nil {
		list, isList := v.([]any)
		names := make([]string, 0, len(list))
		for _, item := range list {
			s, isText := item.(string)
			if !isText {
				isList = false
				break
			}
			names = append(names, s)
		}
		if isList {
			g.Members = names
		} else {
			a.fail("group.memberNames", "is not a list of names", nil)
		}
	}
	writable := a.text(group, "groupWriteMember", "group.groupWriteMember")
	if a.err != nil {
		return GroupSettings{}, a.err
	}
	if writable == "" {
		return g, nil
	}
	fail := func(format string, args ...any) error {
		return &SettingsError{Repository: name, Key: "group.groupWriteMember", Problem: fmt.Sprintf(format, args...)}
	}
	member, exists := rs.Get(writable)
	switch {
	case !slices.Contains(g.Members, writable):
		return GroupSettings{}, fail("names %s, which the group does not list", writable)
	case !exists:
		return GroupSettings{}, fail("names %s, which does not exist", writable)
	case member.Type != "hosted":
		return GroupSettings{}, fail("names %s, a %s, not a hosted repository", writable, member.Type)
	case member.Format != r.Format:
		return GroupSettings{}, fail("names %s of format %s, not %s", writable, member.Format, r.Format)
	}
	g.WritableMember = writable
	return g, nil
}

// WithRoutingRules returns the repositories, each with the name of the
// routing rule that links gives its name (decision P5-1 keeps the links in
// state.sqlite, read by internal/state/routing.go). A link without a
// repository of its name adds none; the repositories it is called on stay
// as they are.
func (rs *Repositories) WithRoutingRules(links map[string]string) *Repositories {
	out := &Repositories{byName: make(map[string]Repo, len(rs.byName)), sorted: make([]Repo, 0, len(rs.sorted))}
	for _, repo := range rs.sorted {
		repo.RoutingRule = links[repo.Name]
		out.byName[repo.Name] = repo
		out.sorted = append(out.sorted, repo)
	}
	return out
}
