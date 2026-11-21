package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/channel"
	"github.com/iogentic/pipesmith-registry/internal/release"
)

// The release check of serve: it reads the releases of the channel of
// update.json (the GitHub releases of channel.DefaultRepository without the
// key) and keeps them in <data>/updates/releases.json with the time of the
// check, so the screen Updates shows the newest release and its notes
// without a request per page view. serve checks by itself every
// check_hours hours while it runs from an install root, and on the button of
// the screen; a rate limit of the source waits for its reset time. It
// installs nothing and never touches the trusted key.

// States of the release check.
const (
	checkNever       = "never"
	checkOK          = "ok"
	checkRateLimited = "rate_limited"
	checkUnreachable = "unreachable"
	checkError       = "error"
	checkUnsupported = "unsupported"
)

// Bounds of the cache.
const (
	maxCachedReleases = 50
	maxCachedBody     = 64 << 10
	maxCacheFile      = 8 << 20
	// checkCooldown spaces two checks: GitHub allows 60 requests per hour
	// without a token, and the timer of the update shares them.
	checkCooldown = 5 * time.Minute
	// checkTimeout bounds one check of serve, every page included.
	checkTimeout = 2 * time.Minute
	// minCheckWait bounds how often the background loop wakes.
	minCheckWait = time.Minute
)

// cachedRelease is one release in the cache and in GET /api/updates.
type cachedRelease struct {
	Version     string `json:"version"`
	Tag         string `json:"tag"`
	Name        string `json:"name"`
	PublishedAt string `json:"published_at"`
	URL         string `json:"url"`
	Body        string `json:"body"`
	Truncated   bool   `json:"truncated,omitempty"`
	Prerelease  bool   `json:"prerelease"`
	Complete    bool   `json:"complete"`
}

// releaseCache is <data>/updates/releases.json.
type releaseCache struct {
	Source     string          `json:"source"` // owner/repo of the check
	CheckedAt  string          `json:"checked_at"`
	State      string          `json:"state"`
	Message    string          `json:"message"`
	RetryAt    string          `json:"retry_at"`
	Releases   []cachedRelease `json:"releases"`
	releasesAt string          // CheckedAt of the last check that ended ok
}

// releaseLister reads the releases of a channel; tests replace it.
type releaseLister func(ctx context.Context, set *channel.Settings) ([]channel.Release, error)

func listReleases(ctx context.Context, set *channel.Settings) ([]channel.Release, error) {
	return channel.Releases(ctx, set, channel.ListOptions{})
}

// releaseChecker holds the cache in memory and on disk.
type releaseChecker struct {
	mu      sync.Mutex
	path    string // <data>/updates/releases.json
	list    releaseLister
	now     func() time.Time
	cache   *releaseCache
	lastTry time.Time
	running bool // a check sends its requests now
}

func newReleaseChecker(dataDir string, list releaseLister) *releaseChecker {
	if list == nil {
		list = listReleases
	}
	return &releaseChecker{path: filepath.Join(dataDir, "updates", "releases.json"), list: list, now: time.Now}
}

// load reads the cache file once; a missing or broken file is the state
// never.
func (c *releaseChecker) load() *releaseCache {
	if c.cache != nil {
		return c.cache
	}
	c.cache = &releaseCache{State: checkNever}
	f, err := os.Open(c.path) // #nosec G304 -- a fixed file below the data directory.
	if err != nil {
		return c.cache
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCacheFile+1))
	if err != nil || len(data) > maxCacheFile {
		return c.cache
	}
	var cached releaseCache
	if json.Unmarshal(data, &cached) == nil && cached.State != "" {
		c.cache = &cached
	}
	return c.cache
}

// state is a copy of the cache.
func (c *releaseChecker) state() releaseCache {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.load()
}

// nextAt is the time the background loop checks next with an interval of
// hours: the last check plus the interval, not before a reset time.
func (c *releaseChecker) nextAt(hours int) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	cache := c.load()
	next := time.Time{}
	if t, err := time.Parse(time.RFC3339, cache.CheckedAt); err == nil {
		next = t.Add(time.Duration(hours) * time.Hour)
	}
	if t, err := time.Parse(time.RFC3339, cache.RetryAt); err == nil && t.After(next) {
		next = t
	}
	return next
}

// check asks the source now, unless a reset time lies ahead, the last
// request is younger than checkCooldown or another check runs; skipped names
// that case. The lock is not held while the requests run, so the answer of
// GET /api/updates and the end of serve never wait for the source. A check
// that its caller cancels changes nothing.
func (c *releaseChecker) check(ctx context.Context, set *channel.Settings, source string) (state releaseCache, skipped string) {
	c.mu.Lock()
	cache := c.load()
	now := c.now()
	switch t, err := time.Parse(time.RFC3339, cache.RetryAt); {
	case err == nil && now.Before(t) && cache.Source == source:
		c.mu.Unlock()
		return *cache, "The source refused more requests until " + cache.RetryAt + "; no request was sent."
	case c.running:
		c.mu.Unlock()
		return *cache, "A check runs already; no second request was sent."
	case !c.lastTry.IsZero() && now.Sub(c.lastTry) < checkCooldown:
		c.mu.Unlock()
		return *cache, fmt.Sprintf("The last check ran %d s ago; checks are at least %d s apart, so no request was sent.",
			int(now.Sub(c.lastTry).Seconds()), int(checkCooldown.Seconds()))
	}
	c.running, c.lastTry = true, now
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	list, err := c.list(ctx, set)
	canceled := errors.Is(ctx.Err(), context.Canceled)
	cancel()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.running = false
	cache = c.load()
	if canceled {
		return *cache, "The check was canceled; the cache stays as it was."
	}
	next := &releaseCache{Source: source, CheckedAt: now.UTC().Format(time.RFC3339)}
	var rl *channel.RateLimitError
	switch {
	case err == nil:
		next.State = checkOK
		next.Releases = cached(list)
	case errors.As(err, &rl):
		next.State, next.Message = checkRateLimited, err.Error()
		if !rl.Reset.IsZero() {
			next.RetryAt = rl.Reset.UTC().Format(time.RFC3339)
		}
	case errors.Is(err, channel.ErrSettings) && set != nil && set.GitHub == nil:
		next.State, next.Message = checkUnsupported, err.Error()
	case isNetwork(err):
		next.State, next.Message = checkUnreachable, err.Error()
	default:
		next.State, next.Message = checkError, err.Error()
	}
	if next.State != checkOK && cache.Source == source {
		// a failed check keeps the releases of the last good one
		next.Releases = cache.Releases
	}
	c.cache = next
	if werr := c.write(next); werr != nil {
		next.Message = joinText(next.Message, "The cache "+c.path+" was not written: "+werr.Error())
	}
	return *next, ""
}

func joinText(a, b string) string {
	if a == "" {
		return b
	}
	return a + " " + b
}

// isNetwork reports an error of the connection rather than of an answer.
func isNetwork(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, channel.ErrDeadline) || errors.Is(err, context.DeadlineExceeded)
}

// cached keeps the newest maxCachedReleases releases that are no draft,
// each body cut at maxCachedBody.
func cached(list []channel.Release) []cachedRelease {
	goos, goarch := channel.Platform()
	out := []cachedRelease{}
	for _, r := range list {
		if r.Draft || !r.OK {
			continue
		}
		c := cachedRelease{Version: r.Version.String(), Tag: r.Tag, Name: r.Name, URL: webURL(r.URL), Body: r.Body,
			Prerelease: r.Prerelease, Complete: r.Complete(goos, goarch)}
		if !r.Published.IsZero() {
			c.PublishedAt = r.Published.UTC().Format(time.RFC3339)
		}
		if len(c.Body) > maxCachedBody {
			c.Body, c.Truncated = c.Body[:maxCachedBody], true
		}
		out = append(out, c)
		if len(out) == maxCachedReleases {
			break
		}
	}
	return out
}

// webURL is u when it is an https address, else "": the page links it.
func webURL(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "https" || p.Host == "" || p.User != nil {
		return ""
	}
	return u
}

func (c *releaseChecker) write(cache *releaseCache) error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	tmp := c.path + ".new"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// releaseSettings are the channel settings serve checks with: those of
// update.json of root, or the default GitHub channel when the server runs
// from no install root or cannot read the file; note says which.
func releaseSettings(root string) (set *channel.Settings, hours int, note string, err error) {
	hours = release.DefaultCheckHours
	if root == "" {
		set, err = channel.ParseSettings(&release.UpdateConfig{Path: release.ConfigFile})
		return set, hours, "This server runs from no install root, so it checks the default channel " + channel.DefaultRepository + ".", err
	}
	cfg, cerr := loadUpdateConfig(root)
	if cerr != nil {
		set, err = channel.ParseSettings(&release.UpdateConfig{Path: release.ConfigFile})
		return set, hours, "This server cannot read " + filepath.Join(root, release.ConfigFile) + ", so it checks the default channel " +
			channel.DefaultRepository + " and every " + fmt.Sprint(hours) + " hours.", err
	}
	if hours, err = cfg.CheckHours(); err != nil {
		return nil, release.DefaultCheckHours, "", err
	}
	set, err = channel.ParseSettings(cfg)
	return set, hours, "", err
}

// sourceName is owner/repo of a GitHub channel or the address of a forge.
func sourceName(set *channel.Settings) string {
	switch {
	case set == nil:
		return ""
	case set.GitHub != nil:
		return set.GitHub.String()
	}
	return set.Server.Redacted() + " (" + set.Owner + ")"
}

// runReleaseChecks is the background loop: while the server runs from an
// install root it checks whenever the interval of check_hours has passed.
// It ends with ctx.
func (s *Server) runReleaseChecks(ctx context.Context, root string) {
	for {
		set, hours, _, err := releaseSettings(root)
		wait := time.Hour
		if err == nil && hours > 0 {
			next := s.releases.nextAt(hours)
			wait = max(time.Until(next), minCheckWait)
			if !next.After(s.releases.now()) {
				s.releases.check(ctx, set, sourceName(set))
				wait = minCheckWait
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// startReleaseChecks starts the loop when the server runs from an install
// root; Close ends it.
func (s *Server) startReleaseChecks() {
	root, _ := s.installRoot()
	if root == "" || s.releases == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.releaseStop = func() {
		cancel()
		<-done
	}
	go func() {
		defer close(done)
		s.runReleaseChecks(ctx, root)
	}()
}
