package channel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// The GitHub form of the channel reads the releases API: GET
// {api}/repos/{owner}/{repo}/releases with per_page and page, newest first,
// the header Link naming rel="next" while a page follows; an asset is read
// with GET {asset url} and Accept: application/octet-stream, which answers a
// redirect to release-assets.githubusercontent.com (measured on
// api.github.com for iogentic/pipesmith-registry, 2026-11-21). Without a
// token GitHub allows 60 requests per hour and answers 403 or 429 past
// that, with x-ratelimit-reset or retry-after.
const (
	gitHubPageSize  = 50
	gitHubMaxPages  = 10
	gitHubListBytes = 8 << 20
	gitHubVersion   = "2022-11-28"
)

// gitHubAssetHosts are the hosts api.github.com redirects an asset to.
var gitHubAssetHosts = []string{"release-assets.githubusercontent.com", "objects.githubusercontent.com",
	"github-releases.githubusercontent.com"}

// ErrRateLimit is the refusal of a source past its request limit.
var ErrRateLimit = errors.New("rate limit")

// RateLimitError names the time from which the source answers again; Reset
// is zero when the answer named none.
type RateLimitError struct {
	Code  int
	Reset time.Time
}

func (e *RateLimitError) Error() string {
	if e.Reset.IsZero() {
		return fmt.Sprintf("rate limit: the source answers %d and names no time to try again", e.Code)
	}
	return fmt.Sprintf("rate limit: the source answers %d until %s", e.Code, e.Reset.UTC().Format(time.RFC3339))
}

func (e *RateLimitError) Unwrap() error { return ErrRateLimit }

// rateLimit reads a 403 or 429 answer of GitHub: a rate limit when
// x-ratelimit-remaining is 0 or retry-after is given, else nil.
func rateLimit(resp *http.Response, now time.Time) error {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	h := resp.Header
	e := &RateLimitError{Code: resp.StatusCode}
	if after := h.Get("Retry-After"); after != "" {
		if n, err := strconv.ParseInt(after, 10, 64); err == nil && n >= 0 && n < 7*24*3600 {
			e.Reset = now.Add(time.Duration(n) * time.Second)
		}
		return e
	}
	if h.Get("X-Ratelimit-Remaining") != "0" && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	if n, err := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64); err == nil && n > 0 {
		e.Reset = time.Unix(n, 0)
	}
	return e
}

// Release is one release of the GitHub form.
type Release struct {
	Tag        string
	Name       string
	Body       string // the release notes, Markdown as written
	URL        string // the page of the release
	Published  time.Time
	Draft      bool
	Prerelease bool
	// Version is the tag without the leading v; ok is false for a tag that
	// is no version X.Y.Z.
	Version release.Version
	OK      bool
	Assets  map[string]string // name to the API address of the asset
}

// Complete reports whether the release holds the files the fetch stage
// downloads for goos and goarch.
func (r Release) Complete(goos, goarch string) bool {
	for _, name := range requiredAssets(goos, goarch) {
		if _, ok := r.Assets[name]; !ok {
			return false
		}
	}
	return true
}

func requiredAssets(goos, goarch string) []string {
	return []string{release.PublicKeyFile, release.SumsFile, release.BundleFile, BillFile, release.BinaryName(goos, goarch)}
}

type gitHubAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type gitHubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Body        string        `json:"body"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	HTMLURL     string        `json:"html_url"`
	PublishedAt string        `json:"published_at"`
	Assets      []gitHubAsset `json:"assets"`
}

func (set *Settings) apiURL(path string) string {
	u := *set.Server
	u.Path += path
	return u.String()
}

// assetPrefix is the start every asset address of the listing must have.
func (set *Settings) assetPrefix() string {
	return set.apiURL("/repos/" + url.PathEscape(set.GitHub.Owner) + "/" + url.PathEscape(set.GitHub.Name) + "/releases/assets/")
}

// inside reports whether target is the API address of an asset of the
// repository: the scheme, host and port of the API, and below its path
// /repos/<owner>/<repo>/releases/assets/<id>, owner and repo in any case, as
// GitHub names them in their own spelling.
func inside(target string, set *Settings) bool {
	u, err := url.Parse(target)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || origin(u) != origin(set.Server) {
		return false
	}
	rest, ok := strings.CutPrefix(u.Path, set.Server.Path+"/repos/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 5 || !strings.EqualFold(parts[0], set.GitHub.Owner) || !strings.EqualFold(parts[1], set.GitHub.Name) ||
		parts[2] != "releases" || parts[3] != "assets" {
		return false
	}
	_, err = strconv.ParseUint(parts[4], 10, 64)
	return err == nil
}

// gitHubReleases reads every page of the releases of the repository.
func (c *client) gitHubReleases(set *Settings) ([]Release, int, error) {
	var out []Release
	prefix := set.assetPrefix()
	pages := 0
	for page := 1; ; page++ {
		if page > gitHubMaxPages {
			return nil, pages, fmt.Errorf("%w: more than %d pages of releases at %s", ErrList, gitHubMaxPages, set.Server.Redacted())
		}
		q := url.Values{"per_page": {strconv.Itoa(gitHubPageSize)}, "page": {strconv.Itoa(page)}}
		target := set.apiURL("/repos/"+url.PathEscape(set.GitHub.Owner)+"/"+url.PathEscape(set.GitHub.Name)+"/releases") + "?" + q.Encode()
		body, header, err := c.fetch(target, gitHubListBytes)
		if err != nil {
			return nil, pages, fmt.Errorf("%w: page %d: %w", ErrList, page, err)
		}
		pages++
		var entries []gitHubRelease
		if err := json.Unmarshal(body, &entries); err != nil {
			return nil, pages, fmt.Errorf("%w: page %d is no JSON list of releases", ErrList, page)
		}
		for _, e := range entries {
			r := Release{Tag: e.TagName, Name: e.Name, Body: e.Body, URL: e.HTMLURL, Draft: e.Draft, Prerelease: e.Prerelease,
				Assets: map[string]string{}}
			if t, err := time.Parse(time.RFC3339, e.PublishedAt); err == nil {
				r.Published = t
			}
			if text, ok := strings.CutPrefix(e.TagName, "v"); ok {
				if v, err := release.ParseVersion(text); err == nil {
					r.Version, r.OK = v, true
				}
			}
			for _, a := range e.Assets {
				if !inside(a.URL, set) {
					return nil, pages, fmt.Errorf("%w: release %s names the asset %s at %s, outside %s",
						ErrList, bounded(e.TagName), bounded(a.Name), bounded(a.URL), prefix)
				}
				r.Assets[a.Name] = a.URL
			}
			out = append(out, r)
		}
		if len(entries) == 0 || !hasNext(header) {
			break
		}
	}
	return out, pages, nil
}

// listGitHub is list of the fetch stage for the GitHub form: the versions
// of the releases that are no draft and no prerelease, highest first.
func (f *Fetcher) listGitHub(s *stage) ([]release.Version, error) {
	all, pages, err := s.c.gitHubReleases(s.set)
	if err != nil {
		return nil, err
	}
	s.assets = map[string]map[string]string{}
	var out []release.Version
	for _, r := range all {
		if !r.OK || r.Draft || r.Prerelease {
			continue
		}
		name := r.Version.String()
		if _, seen := s.assets[name]; seen {
			continue
		}
		s.assets[name] = r.Assets
		out = append(out, r.Version)
	}
	slices.SortFunc(out, func(a, b release.Version) int { return b.Compare(a) })
	f.step("listed: %d versions of %s at %s on %d pages", len(out), s.set.GitHub, s.set.Server.Redacted(), pages)
	return out, nil
}

// completeGitHub reports whether the release of version name holds every
// file of the stage; no request is sent.
func (f *Fetcher) completeGitHub(s *stage, name string) (bool, string) {
	goos, goarch := f.platform()
	for _, file := range requiredAssets(goos, goarch) {
		if _, ok := s.assets[name][file]; !ok {
			return false, file
		}
	}
	return true, ""
}

// ListOptions change how Releases connects; the zero value verifies TLS
// with the roots of the system.
type ListOptions struct {
	TLS *tls.Config
}

// Releases lists every release of the GitHub channel set, newest first as
// the API answers, drafts and prereleases included. A rate limit is a
// *RateLimitError.
func Releases(ctx context.Context, set *Settings, opt ListOptions) ([]Release, error) {
	if set.GitHub == nil {
		return nil, fmt.Errorf("%w: the channel is a forge; only the GitHub form lists releases with notes", ErrSettings)
	}
	c := newClient(set, "", nil, opt.TLS)
	defer c.http.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, set.Stage)
	defer cancel()
	c.stage, c.stageLimit = ctx, set.Stage
	out, _, err := c.gitHubReleases(set)
	return out, err
}

// Platform is the GOOS and GOARCH of the running binary.
func Platform() (string, string) { return runtime.GOOS, runtime.GOARCH }
