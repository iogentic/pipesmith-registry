package channel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// Settings are the keys of the fetch stage in update.json.
type Settings struct {
	Server    *url.URL // scheme, host, optional port and path prefix, no trailing slash
	Owner     string
	TokenFile string // a plain file name below <data directory>/secrets/; empty without a token
	// GitHub is the repository of the GitHub form; Server is then the
	// address of the API. Nil is the forge form.
	GitHub   *Repo
	Limits   Limits
	Timeouts Timeouts
	// Stage bounds the whole fetch stage, every request included
	// (timeouts.stage_seconds).
	Stage time.Duration
	// Candidates bounds the versions above the running one whose
	// completeness the stage asks for (limits.candidates).
	Candidates int
}

// Limits are the size caps of the downloaded files (U12).
type Limits struct{ Sums, Bundle, Binary, Bill int64 }

// Timeouts apply to each request; Total covers one file.
type Timeouts struct{ Dial, Header, Total time.Duration }

var (
	defaultLimits   = Limits{Sums: 1 << 20, Bundle: 1 << 20, Binary: 512 << 20, Bill: 64 << 20}
	defaultTimeouts = Timeouts{Dial: 10 * time.Second, Header: 30 * time.Second, Total: 1800 * time.Second}
)

// Defaults and bound of the stage settings.
const (
	defaultStage      = time.Hour
	defaultCandidates = 10
	maxCandidates     = 1000
)

// Bounds of a setting: a cap of at most 16 GiB, a timeout of at most a day.
const (
	maxCap     = 16 << 30
	maxSeconds = 24 * 60 * 60
)

// checkedCap is the size of SHA256SUMS and of the bundle that the release
// check accepts, maxFile of internal/release; their caps go no higher.
const checkedCap = 1 << 20

var tokenFileName = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)

var ownerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// DefaultRepository is the GitHub repository of the releases, the channel
// when update.json has no key channel.
const DefaultRepository = "iogentic/pipesmith-registry"

// DefaultAPI is the address of the GitHub API.
const DefaultAPI = "https://api.github.com"

// Repo is owner and name of a GitHub repository.
type Repo struct{ Owner, Name string }

func (r Repo) String() string { return r.Owner + "/" + r.Name }

var repoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}/[A-Za-z0-9._-]{1,100}$`)

// ParseSettings reads the key channel of update.json. Without it the channel
// is the GitHub repository DefaultRepository. The key holds either the forge
// form {"server", "owner", "token_file"} or the GitHub form {"github":
// "owner/repo", "api"}.
func ParseSettings(cfg *release.UpdateConfig) (*Settings, error) {
	raw, ok := cfg.Member("channel")
	var s *Settings
	var err error
	switch {
	case !ok:
		s, err = gitHubSettings(cfg, DefaultRepository, nil)
	case bytes.Equal(bytes.TrimSpace(raw), []byte("null")):
		return nil, fmt.Errorf("%w: %s: channel is null; leave the key out for the GitHub releases of %s, or name a forge or a repository", ErrSettings, cfg.Path, DefaultRepository)
	default:
		s, err = channelSettings(cfg, raw)
	}
	if err != nil {
		return nil, err
	}
	if err := s.bounds(cfg); err != nil {
		return nil, err
	}
	return s, nil
}

// gitHubSettings is the GitHub form: repo owner/name, api the address of
// the API (nil is DefaultAPI).
func gitHubSettings(cfg *release.UpdateConfig, repo string, api *string) (*Settings, error) {
	if !repoName.MatchString(repo) || strings.Contains(repo, "..") {
		return nil, fmt.Errorf("%w: %s: channel.github %s is not owner/repo", ErrSettings, cfg.Path, bounded(repo))
	}
	text := DefaultAPI
	if api != nil {
		text = *api
	}
	server, err := parseServer(text)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: channel.api: %v", ErrSettings, cfg.Path, err)
	}
	if server.Scheme == "http" && !Loopback(server.Hostname()) {
		return nil, fmt.Errorf("%w: %s: channel.api %s: plain http only to a loopback address", ErrSettings, cfg.Path, server.Redacted())
	}
	owner, name, _ := strings.Cut(repo, "/")
	return &Settings{Server: server, GitHub: &Repo{Owner: owner, Name: name}, Limits: defaultLimits, Timeouts: defaultTimeouts,
		Stage: defaultStage, Candidates: defaultCandidates}, nil
}

func channelSettings(cfg *release.UpdateConfig, raw json.RawMessage) (*Settings, error) {
	var doc struct {
		Server    *string `json:"server"`
		Owner     *string `json:"owner"`
		TokenFile *string `json:"token_file"`
		GitHub    *string `json:"github"`
		API       *string `json:"api"`
	}
	if err := strictDecode(raw, &doc); err != nil {
		return nil, fmt.Errorf("%w: %s: channel: %v", ErrSettings, cfg.Path, err)
	}
	if doc.GitHub != nil {
		switch {
		case doc.Server != nil || doc.Owner != nil:
			return nil, fmt.Errorf("%w: %s: channel names github together with server or owner; give one form", ErrSettings, cfg.Path)
		case doc.TokenFile != nil:
			return nil, fmt.Errorf("%w: %s: channel.token_file: the GitHub releases are read without a token", ErrToken, cfg.Path)
		}
		return gitHubSettings(cfg, *doc.GitHub, doc.API)
	}
	if doc.API != nil {
		return nil, fmt.Errorf("%w: %s: channel.api belongs to the form with github", ErrSettings, cfg.Path)
	}
	if doc.Server == nil || *doc.Server == "" {
		return nil, fmt.Errorf("%w: %s: channel has no server and no github", ErrSettings, cfg.Path)
	}
	if doc.Owner == nil || !ownerName.MatchString(*doc.Owner) {
		return nil, fmt.Errorf("%w: %s: channel.owner is missing or not a plain owner name", ErrSettings, cfg.Path)
	}
	server, err := parseServer(*doc.Server)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: channel.server: %v", ErrSettings, cfg.Path, err)
	}
	s := &Settings{Server: server, Owner: *doc.Owner, Limits: defaultLimits, Timeouts: defaultTimeouts,
		Stage: defaultStage, Candidates: defaultCandidates}
	if doc.TokenFile != nil {
		s.TokenFile = *doc.TokenFile
		if !tokenFileName.MatchString(s.TokenFile) || s.TokenFile == "." || s.TokenFile == ".." {
			return nil, fmt.Errorf("%w: %s: channel.token_file %q is not a plain file name below secrets/", ErrToken, cfg.Path, s.TokenFile)
		}
	}
	if s.TokenFile != "" && !tokenAllowed(server) {
		return nil, fmt.Errorf("%w: %s: the token goes only over https or to a loopback address, and %s is plain http to %s; nothing was sent",
			ErrToken, cfg.Path, server.Redacted(), server.Hostname())
	}
	if server.Scheme == "http" && !Loopback(server.Hostname()) {
		return nil, fmt.Errorf("%w: %s: channel.server %s: plain http only to a loopback address", ErrSettings, cfg.Path, server.Redacted())
	}
	return s, nil
}

// bounds reads the keys limits and timeouts of update.json into s.
func (s *Settings) bounds(cfg *release.UpdateConfig) error {
	limits := map[string]*int64{"sums_bytes": &s.Limits.Sums, "bundle_bytes": &s.Limits.Bundle,
		"binary_bytes": &s.Limits.Binary, "bill_bytes": &s.Limits.Bill}
	candidates := int64(defaultCandidates)
	if err := numbers(cfg, "limits", maxCap, func(key string, n int64) bool {
		if key == "candidates" {
			candidates = n
			return true
		}
		p, ok := limits[key]
		if ok {
			*p = n
		}
		return ok
	}); err != nil {
		return err
	}
	if candidates > maxCandidates {
		return fmt.Errorf("%w: %s: limits.candidates is %d, want a whole number from 1 to %d", ErrSettings, cfg.Path, candidates, maxCandidates)
	}
	s.Candidates = int(candidates)
	for key, n := range map[string]int64{"sums_bytes": s.Limits.Sums, "bundle_bytes": s.Limits.Bundle} {
		if n > checkedCap {
			return fmt.Errorf("%w: %s: limits.%s is %d; the release check accepts at most %d bytes", ErrSettings, cfg.Path, key, n, checkedCap)
		}
	}
	timeouts := map[string]*time.Duration{"dial_seconds": &s.Timeouts.Dial, "header_seconds": &s.Timeouts.Header,
		"total_seconds": &s.Timeouts.Total, "stage_seconds": &s.Stage}
	if err := numbers(cfg, "timeouts", maxSeconds, func(key string, n int64) bool {
		p, ok := timeouts[key]
		if ok {
			*p = time.Duration(n) * time.Second
		}
		return ok
	}); err != nil {
		return err
	}
	return nil
}

// numbers reads the object key of update.json whose members are whole
// numbers from 1 to most; set refuses an unknown member.
func numbers(cfg *release.UpdateConfig, key string, most int64, set func(string, int64) bool) error {
	raw, ok := cfg.Member(key)
	if !ok {
		return nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return fmt.Errorf("%w: %s: %s is not an object of numbers", ErrSettings, cfg.Path, key)
	}
	for name, value := range doc {
		n, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil || n < 1 || n > most {
			return fmt.Errorf("%w: %s: %s.%s is %s, want a whole number from 1 to %d", ErrSettings, cfg.Path, key, name, value, most)
		}
		if !set(name, n) {
			return fmt.Errorf("%w: %s: %s has no member %q", ErrSettings, cfg.Path, key, name)
		}
	}
	return nil
}

// strictDecode decodes one JSON object into dest; a member of an unknown
// name is refused, so a misspelt key is seen.
func strictDecode(raw json.RawMessage, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("text after the object")
	}
	return nil
}

// parseServer accepts https://host[:port][/prefix] and http:// of the same
// form for a loopback host. No refusal repeats the text as written: it names
// the address without user information, query and fragment, as shown does.
func parseServer(text string) (*url.URL, error) {
	u, err := url.Parse(text)
	if err != nil {
		return nil, fmt.Errorf("the value is no address")
	}
	switch {
	case u.Scheme != "https" && u.Scheme != "http":
		return nil, fmt.Errorf("%s: scheme %q, want https (or http to loopback)", shown(u), u.Scheme)
	case u.User != nil:
		return nil, fmt.Errorf("%s holds user information; a token goes into token_file", shown(u))
	case u.Host == "" || u.Hostname() == "":
		return nil, fmt.Errorf("%s has no host", shown(u))
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "":
		return nil, fmt.Errorf("%s holds a query or a fragment", shown(u))
	case strings.Contains(u.Path, "/api/"):
		return nil, fmt.Errorf("%s holds an API path; give the address of the forge", shown(u))
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%s: port %q", shown(u), p)
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

// shown is u without user information, query, fragment and opaque part.
func shown(u *url.URL) string {
	c := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
	return strconv.Quote(c.String())
}

// Loopback reports whether host is localhost or a loopback IP address.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
