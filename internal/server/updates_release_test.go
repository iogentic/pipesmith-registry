package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/channel"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/release"
	"github.com/iogentic/pipesmith-registry/internal/version"
)

// fakeSource answers the release list of the tests and counts the calls.
type fakeSource struct {
	mu    sync.Mutex
	calls int
	sets  []*channel.Settings
	list  []channel.Release
	err   error
}

func (f *fakeSource) lister(_ context.Context, set *channel.Settings) ([]channel.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.sets = append(f.sets, set)
	return f.list, f.err
}

func (f *fakeSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func rel(t *testing.T, tag, body string, pre bool) channel.Release {
	t.Helper()
	v, err := release.ParseVersion(strings.TrimPrefix(tag, "v"))
	goos, goarch := channel.Platform()
	assets := map[string]string{}
	for _, name := range []string{release.PublicKeyFile, release.SumsFile, release.BundleFile, channel.BillFile, release.BinaryName(goos, goarch)} {
		assets[name] = "https://api.github.com/x/" + name
	}
	return channel.Release{Tag: tag, Name: tag, Body: body, URL: "https://github.com/iogentic/pipesmith-registry/releases/tag/" + tag,
		Published: time.Date(2026, 11, 20, 19, 41, 52, 0, time.UTC), Prerelease: pre, Version: v, OK: err == nil, Assets: assets}
}

// startReleases starts a server with security whose install root is root
// ("" for none) with the release source src and the timer answer timer.
func startReleases(t *testing.T, root string, src *fakeSource, timer string) *secureApp {
	t.Helper()
	sec := newFakeSecurity()
	base := t.TempDir()
	stores := []config.StoreConfig{blobStore(t, base, "store-a", spec("demo-raw", "docs/readme.txt", []byte("readme"), "text/plain"))}
	opts := appOptions{defs: []*pyjson.Object{def("demo-raw", "raw", "hosted")}, security: sec,
		installRoot: func() (string, string) { return root, filepath.Join(base, "bin", "pipesmith-registry") }, trustRule: modeRule,
		releaseList: src.lister,
		timerShow: func(context.Context, string) (string, error) {
			if timer == "" {
				return "", errors.New("exec: \"systemctl\": executable file not found in $PATH")
			}
			return timer, nil
		}}
	return &secureApp{app: startApp(t, base, prepare(t, base, stores, opts), opts), sec: sec}
}

func runningVersion(t *testing.T, v string) {
	t.Helper()
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

func checkNow(t *testing.T, a *secureApp, who string) map[string]any {
	t.Helper()
	r := a.send("POST", "/api/updates/check", "basic", who)
	status(t, r, 200)
	return r.object(t)
}

func TestUpdatesCheckReadsTheReleases(t *testing.T) {
	runningVersion(t, "0.2.0")
	src := &fakeSource{list: []channel.Release{rel(t, "v0.4.0", "pre", true), rel(t, "v0.3.0", "## Fixes\n\n- one", false), rel(t, "v0.2.0", "old", false)}}
	a := startReleases(t, "", src, "")
	got := updatesOf(t, a, asAdmin)
	if got["version"] != "0.2.0" || answerField(t, got, "source.state") != checkNever || got["status"] != statusUnknown || src.count() != 0 {
		t.Fatalf("before a check: %v, %d calls", got, src.count())
	}
	if answerField(t, got, "repository.url") != "https://github.com/iogentic/pipesmith-registry" ||
		answerField(t, got, "repository.releases_url") != "https://github.com/iogentic/pipesmith-registry/releases" {
		t.Fatalf("repository %v", got["repository"])
	}
	if answerField(t, got, "source.background") != false || got["auto"] != nil || got["history"] != nil {
		t.Fatalf("a server outside an install root: %v", got)
	}
	got = checkNow(t, a, asAdmin)
	if src.count() != 1 || src.sets[0].GitHub.String() != channel.DefaultRepository {
		t.Fatalf("%d calls, settings %+v", src.count(), src.sets)
	}
	if got["status"] != statusAvailable || answerField(t, got, "latest.version") != "0.3.0" ||
		answerField(t, got, "latest.body") != "## Fixes\n\n- one" || answerField(t, got, "source.state") != checkOK {
		t.Fatalf("after the check: %v", got)
	}
	if list, _ := got["releases"].([]any); len(list) != 3 {
		t.Fatalf("releases %v", got["releases"])
	}
	if _, err := os.Stat(filepath.Join(a.app.cfg.DataDir, "updates", "releases.json")); err != nil {
		t.Fatalf("no cache: %v", err)
	}
	again := checkNow(t, a, asAdmin)
	if src.count() != 1 || !strings.Contains(answerField(t, again, "source.skipped").(string), "at least 300 s apart") {
		t.Fatalf("a second press: %d calls, %v", src.count(), again["source"])
	}
	if got := updatesOf(t, a, asAdmin); src.count() != 1 || answerField(t, got, "latest.version") != "0.3.0" {
		t.Fatalf("GET after the check: %d calls, %v", src.count(), got["latest"])
	}
}

func TestUpdatesCheckUpToDate(t *testing.T) {
	runningVersion(t, "0.3.0")
	src := &fakeSource{list: []channel.Release{rel(t, "v0.3.0", "", false), rel(t, "v0.2.0", "", false)}}
	got := checkNow(t, startReleases(t, "", src, ""), asAdmin)
	if got["status"] != statusUpToDate {
		t.Fatalf("status %v", got["status"])
	}
}

func TestUpdatesCheckNeedsThePermission(t *testing.T) {
	src := &fakeSource{}
	a := startReleases(t, "", src, "")
	status(t, a.send("POST", "/api/updates/check"), 401)
	status(t, a.send("POST", "/api/updates/check", "basic", asNobody), 403)
	status(t, a.send("GET", "/api/updates/check", "basic", asAdmin), 405)
	if src.count() != 0 {
		t.Fatalf("%d calls without the permission", src.count())
	}
}

func TestReleaseCheckStates(t *testing.T) {
	dir := t.TempDir()
	src := &fakeSource{list: []channel.Release{rel(t, "v0.3.0", "x", false)}}
	c := newReleaseChecker(dir, src.lister)
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	set, _, _, err := releaseSettings("")
	if err != nil {
		t.Fatal(err)
	}
	if st, skipped := c.check(context.Background(), set, "a/b"); st.State != checkOK || skipped != "" || len(st.Releases) != 1 {
		t.Fatalf("ok: %+v %q", st, skipped)
	}
	reset := now.Add(30 * time.Minute)
	src.list, src.err = nil, &channel.RateLimitError{Code: 403, Reset: reset}
	now = now.Add(6 * time.Minute)
	st, _ := c.check(context.Background(), set, "a/b")
	if st.State != checkRateLimited || st.RetryAt != reset.Format(time.RFC3339) || len(st.Releases) != 1 {
		t.Fatalf("rate limit keeps the releases and names the reset: %+v", st)
	}
	now = now.Add(5 * time.Minute)
	if _, skipped := c.check(context.Background(), set, "a/b"); !strings.Contains(skipped, "until") || src.count() != 2 {
		t.Fatalf("within the reset: %q, %d calls", skipped, src.count())
	}
	if next := c.nextAt(6); !next.Equal(now.Add(-5 * time.Minute).Add(6 * time.Hour)) {
		t.Fatalf("next %v", next)
	}
	now = reset.Add(time.Minute)
	src.err = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: network is unreachable")}
	if st, _ := c.check(context.Background(), set, "a/b"); st.State != checkUnreachable || len(st.Releases) != 1 {
		t.Fatalf("no network: %+v", st)
	}
	now = now.Add(6 * time.Minute)
	src.err = errors.New("version list: page 1 is no JSON list of releases")
	if st, _ := c.check(context.Background(), set, "a/b"); st.State != checkError {
		t.Fatalf("a broken answer: %+v", st)
	}
	// the cache survives a new checker, as a restart of serve
	c2 := newReleaseChecker(dir, src.lister)
	if st := c2.state(); st.State != checkError || len(st.Releases) != 1 || st.Source != "a/b" {
		t.Fatalf("after a restart: %+v", st)
	}
}

func TestReleaseCacheCutsBodies(t *testing.T) {
	r := rel(t, "v0.3.0", strings.Repeat("x", maxCachedBody+10), false)
	d := rel(t, "v0.4.0", "draft", false)
	d.Draft = true
	got := cached([]channel.Release{d, r})
	if len(got) != 1 || len(got[0].Body) != maxCachedBody || !got[0].Truncated || !got[0].Complete {
		t.Fatalf("%d releases, body %d, truncated %v", len(got), len(got[0].Body), got[0].Truncated)
	}
}

const timerAnswer = "LoadState=loaded\nActiveState=active\nUnitFileState=enabled\n" +
	"NextElapseUSecRealtime=Sat 2026-11-21 14:15:00 UTC\nLastTriggerUSec=Sat 2026-11-21 13:15:02 UTC\n"

func TestUpdatesShowsTheScheduledUpdate(t *testing.T) {
	root, _, _ := installLayout(t)
	writeFile(t, filepath.Join(root, "update.json"), []byte(`{"trusted_key": "trusted.pub", "data_dir": "/var/lib/pipesmith-registry", `+
		`"scheduled_switch": true, "window": "02:00-05:00", "check_hours": 12}`), 0o644)
	history := `{"id":"1","time":"2026-11-20T02:10:00Z","action":"update","result":"ok","from":"0.1.0","to":"0.2.0","reason":"healthy","auto":true}` + "\n" +
		`{"id":"2","time":"2026-11-21T02:10:00Z","action":"update","result":"back","from":"0.2.0","to":"0.2.1","reason":"health wait failed","auto":true}` + "\n"
	writeFile(t, filepath.Join(root, "releases", release.HistoryFile), []byte(history), 0o644)
	if err := release.WriteLastRun(root, release.LastRun{Time: "2026-11-21T02:10:30Z", Result: release.RunBack, Version: "0.2.1", Message: "health wait failed"}); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{}
	got := updatesOf(t, startReleases(t, root, src, timerAnswer), asAdmin)
	want := map[string]any{
		"auto.enabled":          true,
		"auto.window":           "02:00-05:00",
		"auto.error":            "",
		"auto.timer.readable":   true,
		"auto.timer.active":     "active",
		"auto.timer.enabled":    "enabled",
		"auto.timer.next":       "2026-11-21T14:15:00Z",
		"auto.timer.last":       "2026-11-21T13:15:02Z",
		"auto.last_run.result":  "back",
		"auto.last_run.version": "0.2.1",
		"last_update.to":        "0.2.0",
		"last_update.auto":      true,
		"source.background":     true,
	}
	for path, value := range want {
		if v := answerField(t, got, path); v != value {
			t.Errorf("%s: %v, want %v", path, v, value)
		}
	}
	if v := answerField(t, got, "source.interval_hours"); fmt.Sprint(v) != "12" {
		t.Errorf("source.interval_hours: %v", v)
	}
	h, _ := got["history"].([]any)
	if len(h) != 2 || h[0].(map[string]any)["result"] != "back" || h[0].(map[string]any)["auto"] != true {
		t.Fatalf("history %v", got["history"])
	}
}

func TestUpdatesTimerNotReadable(t *testing.T) {
	root, _, _ := installLayout(t)
	got := updatesOf(t, startReleases(t, root, &fakeSource{}, ""), asAdmin)
	if answerField(t, got, "auto.timer.readable") != false || !strings.Contains(answerField(t, got, "auto.timer.message").(string), "systemctl") ||
		answerField(t, got, "auto.enabled") != false {
		t.Fatalf("auto %v", got["auto"])
	}
	got = updatesOf(t, startReleases(t, root, &fakeSource{}, "LoadState=not-found\nActiveState=inactive\n"), asAdmin)
	if answerField(t, got, "auto.timer.readable") != true || !strings.Contains(answerField(t, got, "auto.timer.message").(string), "not installed") {
		t.Fatalf("auto %v", got["auto"])
	}
}

func TestBackgroundCheckRunsInAnInstallRoot(t *testing.T) {
	root, _, _ := installLayout(t)
	src := &fakeSource{list: []channel.Release{rel(t, "v0.3.0", "", false)}}
	startReleases(t, root, src, "")
	deadline := time.Now().Add(5 * time.Second)
	for src.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if src.count() != 1 {
		t.Fatalf("%d background checks, want 1 at the start", src.count())
	}
	time.Sleep(100 * time.Millisecond)
	if src.count() != 1 {
		t.Fatalf("%d background checks within 100 ms", src.count())
	}
}

func TestBackgroundCheckOff(t *testing.T) {
	root, _, _ := installLayout(t)
	writeFile(t, filepath.Join(root, "update.json"), []byte(`{"trusted_key": "trusted.pub", "check_hours": 0}`), 0o644)
	src := &fakeSource{}
	a := startReleases(t, root, src, "")
	time.Sleep(300 * time.Millisecond)
	if src.count() != 0 {
		t.Fatalf("%d background checks with check_hours 0", src.count())
	}
	if got := updatesOf(t, a, asAdmin); answerField(t, got, "source.background") != false {
		t.Fatalf("source %v", got["source"])
	}
}

func TestNoBackgroundCheckOutsideAnInstallRoot(t *testing.T) {
	src := &fakeSource{}
	startReleases(t, "", src, "")
	time.Sleep(300 * time.Millisecond)
	if src.count() != 0 {
		t.Fatalf("%d background checks without an install root", src.count())
	}
}

func TestSystemdTime(t *testing.T) {
	for in, want := range map[string]string{"Sat 2026-11-21 14:15:00 UTC": "2026-11-21T14:15:00Z", "n/a": "", "": "", "Sat 2026-11-21 14:15:00 CET": ""} {
		if got := systemdTime(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// blockingSource answers once release is closed; started is closed when a
// call began.
type blockingSource struct {
	started, release chan struct{}
	calls            int
}

func (b *blockingSource) lister(ctx context.Context, _ *channel.Settings) ([]channel.Release, error) {
	b.calls++
	close(b.started)
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, nil
}

func TestReleaseCheckHoldsNoLockWhileItRuns(t *testing.T) {
	src := &blockingSource{started: make(chan struct{}), release: make(chan struct{})}
	c := newReleaseChecker(t.TempDir(), src.lister)
	set, _, _, _ := releaseSettings("")
	done := make(chan releaseCache, 1)
	go func() {
		st, _ := c.check(context.Background(), set, "a/b")
		done <- st
	}()
	<-src.started
	read := make(chan string, 1)
	go func() { read <- c.state().State }()
	select {
	case st := <-read:
		if st != checkNever {
			t.Fatalf("state while the check runs: %s", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("state() waits for the running check")
	}
	if _, skipped := c.check(context.Background(), set, "a/b"); !strings.Contains(skipped, "runs already") {
		t.Fatalf("a second check: %q", skipped)
	}
	close(src.release)
	if st := <-done; st.State != checkOK || src.calls != 1 {
		t.Fatalf("%+v, %d calls", st, src.calls)
	}
}

func TestCanceledReleaseCheckChangesNothing(t *testing.T) {
	src := &blockingSource{started: make(chan struct{}), release: make(chan struct{})}
	c := newReleaseChecker(t.TempDir(), src.lister)
	set, _, _, _ := releaseSettings("")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-src.started; cancel() }()
	st, skipped := c.check(ctx, set, "a/b")
	if st.State != checkNever || !strings.Contains(skipped, "canceled") {
		t.Fatalf("%+v %q", st, skipped)
	}
	if _, err := os.Stat(c.path); err == nil {
		t.Fatal("a canceled check wrote the cache")
	}
}

func TestReleasePageAddressMustBeHTTPS(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/a/b/releases/tag/v1": "https://github.com/a/b/releases/tag/v1",
		"javascript:alert(1)":                    "",
		"http://github.com/a":                    "",
		"https://user@github.com/a":              "",
		"":                                       "",
	} {
		if got := webURL(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}
