package channel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/release"
	"github.com/iogentic/pipesmith-registry/internal/release/releasetest"
)

// linkService runs the release current points to and answers its version.
type linkService struct {
	mu       sync.Mutex
	root     string
	running  string
	restarts int
}

func (s *linkService) Restart(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, err := os.Readlink(filepath.Join(s.root, release.CurrentLink))
	if err != nil {
		return err
	}
	s.running, s.restarts = strings.TrimPrefix(target, "releases/"), s.restarts+1
	return nil
}

func (s *linkService) Stop(context.Context, string) error { return nil }

func (s *linkService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if r.URL.Path == "/api/health" {
		fmt.Fprint(w, `{"status":"ok"}`)
		return
	}
	fmt.Fprintf(w, `{"version":%q}`, running)
}

// copyStub puts the published folder of version into releases/<version>/.
func (r *rig) copyStub(version string) {
	r.t.Helper()
	dir := filepath.Join(r.root, release.ReleasesDir, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(r.stubDir, version))
	if err != nil {
		r.t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(r.stubDir, version, e.Name()))
		if err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o755); err != nil {
			r.t.Fatal(err)
		}
	}
}

// heldFetch starts a fetch of v0.2.0 whose download of the binary waits
// until release is called, and returns once the stub holds that answer.
func (r *rig) heldFetch() (release func(), done <-chan error) {
	r.t.Helper()
	started, open := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.wrap = onFile(platformBinary(), func(w http.ResponseWriter, req *http.Request, next http.Handler) {
		if strings.Contains(req.URL.Path, "/v0.2.0/") {
			once.Do(func() { close(started) })
			select {
			case <-open:
			case <-time.After(30 * time.Second):
			}
		}
		next.ServeHTTP(w, req)
	})
	r.publish(published{version: "0.2.0"})
	r.publish(published{version: "0.3.0"})
	r.start()
	result := make(chan error, 1)
	go func() {
		_, err := r.f.Fetch(Options{Version: "0.2.0"})
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		r.t.Fatal("the fetch did not reach the download of the binary")
	}
	var closeOnce sync.Once
	return func() { closeOnce.Do(func() { close(open) }) }, result
}

// While a fetch waits on the forge, rollback and the switch to another
// staged version run; a second fetch is refused; the fetch then ends as it
// would alone.
func TestFetchHoldsNoLockOfTheRoot(t *testing.T) {
	r := newRig(t)
	if err := os.MkdirAll(filepath.Join(r.root, release.ReleasesDir, "0.0.9"), 0o755); err != nil {
		t.Fatal(err)
	}
	for v, path := range map[string]string{"0.0.9": "0.0.9", "0.1.0": "0.1.0"} {
		bin := filepath.Join(r.root, release.ReleasesDir, path, release.InstalledBinary)
		if err := os.WriteFile(bin, releasetest.Answering(t, v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/0.0.9", filepath.Join(r.root, release.PreviousLink)); err != nil {
		t.Fatal(err)
	}
	unblock, done := r.heldFetch()
	defer unblock()
	r.copyStub("0.3.0")

	svc := &linkService{root: r.root, running: "0.1.0"}
	srv := httptest.NewServer(svc)
	defer srv.Close()
	u := &release.Updater{Root: r.root, Service: svc, Out: io.Discard, HealthTimeout: 5 * time.Second,
		Address: func(*release.UpdateConfig) (string, error) { return srv.URL, nil }}

	second := &Fetcher{Root: r.root, Out: io.Discard, tls: r.f.tls}
	if _, err := second.Fetch(Options{Version: "0.2.0"}); err == nil || !strings.Contains(err.Error(), "another fetch stage runs") {
		t.Fatalf("a second fetch: %v", err)
	}
	if err := u.Rollback(); err != nil {
		t.Fatalf("rollback during a fetch: %v", err)
	}
	if err := u.Switch("0.3.0", release.UpdateOptions{}); err != nil {
		t.Fatalf("switch during a fetch: %v", err)
	}
	if target, _ := os.Readlink(filepath.Join(r.root, release.CurrentLink)); target != "releases/0.3.0" || svc.restarts != 2 {
		t.Fatalf("current %s, %d restarts", target, svc.restarts)
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the fetch: %v\n%s", err, r.out.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the fetch did not end")
	}
	if !strings.HasSuffix(r.out.String(), "staged: releases/0.2.0\n") {
		t.Fatalf("output:\n%s", r.out.String())
	}
	if _, err := release.Check(filepath.Join(r.root, release.ReleasesDir, "0.2.0"), release.Options{TrustedKey: r.trusted.Pub}); err != nil {
		t.Fatal(err)
	}
}

// A releases/<version>/ that appears while the fetch downloads stays as it
// is; the fetch reports it and removes its partial directory.
func TestFetchTargetAppears(t *testing.T) {
	r := newRig(t)
	unblock, done := r.heldFetch()
	defer unblock()
	r.copyStub("0.2.0")
	before, err := os.ReadFile(filepath.Join(r.root, release.ReleasesDir, "0.2.0", release.SumsFile))
	if err != nil {
		t.Fatal(err)
	}
	unblock()
	var ferr error
	select {
	case ferr = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the fetch did not end")
	}
	if !errors.Is(ferr, ErrStage) || !strings.Contains(fmt.Sprint(ferr), "appeared") {
		t.Fatalf("error %v", ferr)
	}
	if exists(filepath.Join(r.root, release.ReleasesDir, "0.2.0.partial")) {
		t.Fatal("the partial directory stayed")
	}
	after, err := os.ReadFile(filepath.Join(r.root, release.ReleasesDir, "0.2.0", release.SumsFile))
	if err != nil || string(after) != string(before) {
		t.Fatalf("releases/0.2.0 changed: %v", err)
	}
}
