// Package channel is the fetch stage of the update in place: it lists the
// versions of the package pipesmith-registry in the generic package registry
// of the forge, picks the highest complete version above the running one,
// downloads its release files into releases/<version>.partial/ of the
// install root, runs every check and renames the directory to
// releases/<version>/. It restarts nothing. The files, names and checks are
// those of docs/RELEASE.md of the ci repository and of section Channel of
// docs/plans/phase-7-8-logging-updates.md.
package channel

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// Package is the name of the package in the registry of the forge.
const Package = "pipesmith-registry"

// The bill of materials of a release.
const BillFile = "sbom.cdx.json"

// Refusals and failures of the fetch stage.
var (
	ErrSettings   = errors.New("channel settings")
	ErrList       = errors.New("version list")
	ErrIncomplete = errors.New("incomplete version")
	ErrDownload   = errors.New("download")
	ErrStage      = errors.New("stage")
	ErrAnswer     = errors.New("version answer")
)

// Options of a fetch stage.
type Options struct {
	// Version names the candidate, X.Y.Z with or without the leading v;
	// empty picks the highest complete version above the running one.
	Version string
	// AllowDowngrade lets Version be the running version or a lower one.
	AllowDowngrade bool
}

// Fetcher runs the fetch stage on an install root.
type Fetcher struct {
	Root string
	// Out receives one line per finished step; nil discards them.
	Out io.Writer

	// DataDir gives the data directory whose secrets/ holds the token file;
	// nil takes data_dir of update.json.
	DataDir func(*release.UpdateConfig) (string, error)

	dial   dialFunc    // nil dials with the dial timeout
	tls    *tls.Config // nil verifies with the roots of the system
	goos   string
	goarch string
}

// Result is the outcome of a fetch stage without an error.
type Result struct {
	Nothing bool   // no complete version above the running one
	Version string // X.Y.Z of the candidate
	Dir     string // releases/<version> of the root
	Existed bool   // the directory was staged before; nothing was fetched
}

func (f *Fetcher) step(format string, args ...any) {
	if f.Out != nil {
		fmt.Fprintf(f.Out, format+"\n", args...)
	}
}

func (f *Fetcher) platform() (string, string) {
	goos, goarch := f.goos, f.goarch
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

// stage is the state of one run.
type stage struct {
	hold    *release.Hold
	set     *Settings
	key     string
	running release.Version
	c       *client
}

// Fetch runs the fetch stage.
func (f *Fetcher) Fetch(opt Options) (*Result, error) {
	hold, err := release.HoldReleases(f.Root)
	if err != nil {
		return nil, err
	}
	defer hold.Release()
	f.step("current: %s/%s", release.ReleasesDir, hold.Current)
	s := &stage{hold: hold, key: hold.Config.TrustedKey}
	if s.key == "" {
		return nil, fmt.Errorf("%w (trusted_key of %s); the fetch stage sends no request without it", release.ErrNoTrustedKey, hold.Config.Path)
	}
	if _, err := release.LoadTrustedKey(s.key); err != nil {
		return nil, err
	}
	if s.set, err = ParseSettings(hold.Config); err != nil {
		return nil, err
	}
	if s.running, err = release.ParseVersion(hold.Current); err != nil {
		return nil, fmt.Errorf("%w: current points to %s/%s, which is not a version X.Y.Z", release.ErrVersion, release.ReleasesDir, hold.Current)
	}
	var named release.Version
	if opt.Version != "" {
		if named, err = release.ParseVersion(strings.TrimPrefix(opt.Version, "v")); err != nil {
			return nil, err
		}
		if named.Compare(s.running) <= 0 && !opt.AllowDowngrade {
			return nil, fmt.Errorf("%w: v%s is not higher than %s, the version current runs; --allow-downgrade fetches it anyway", release.ErrVersion, named, s.running)
		}
	}
	token, err := f.token(hold.Config, s.set)
	if err != nil {
		return nil, err
	}
	s.c = newClient(s.set, token, f.dial, f.tls)

	versions, err := f.list(s)
	if err != nil {
		return nil, err
	}
	if opt.Version != "" {
		return f.named(s, versions, named)
	}
	for _, v := range versions {
		if v.Compare(s.running) <= 0 {
			break
		}
		name := v.String()
		if done, err := f.staged(s, name); err != nil || done != nil {
			return done, err
		}
		complete, err := f.complete(s, name)
		if err != nil {
			return nil, err
		}
		if !complete {
			f.step("skipped: v%s is incomplete, %s answers 404", name, release.PublicKeyFile)
			continue
		}
		f.step("candidate: v%s", name)
		return f.fetch(s, name)
	}
	f.step("nothing to do: no complete version above %s at %s", s.running, s.set.Server.Redacted())
	return &Result{Nothing: true}, nil
}

// named fetches the version the options name.
func (f *Fetcher) named(s *stage, versions []release.Version, v release.Version) (*Result, error) {
	name := v.String()
	if !slices.Contains(versions, v) {
		return nil, fmt.Errorf("%w: v%s is not listed at %s", ErrList, name, s.set.Server.Redacted())
	}
	if done, err := f.staged(s, name); err != nil || done != nil {
		return done, err
	}
	complete, err := f.complete(s, name)
	if err != nil {
		return nil, err
	}
	if !complete {
		return nil, fmt.Errorf("%w: v%s, %s answers 404", ErrIncomplete, name, release.PublicKeyFile)
	}
	f.step("candidate: v%s", name)
	return f.fetch(s, name)
}

// staged returns a result when releases/<name>/ exists already and passes
// the release check; a directory that does not pass is an error and stays.
func (f *Fetcher) staged(s *stage, name string) (*Result, error) {
	dir := s.hold.Layout.Release(name)
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("%w: %v", ErrStage, err)
	case !info.IsDir():
		return nil, fmt.Errorf("%w: %s exists and is not a directory", ErrStage, dir)
	}
	goos, goarch := f.platform()
	if _, err := release.Check(dir, release.Options{TrustedKey: s.key, GOOS: goos, GOARCH: goarch}); err != nil {
		return nil, fmt.Errorf("%w: %s exists and does not pass the release check: %w; move it away, then fetch again", ErrStage, dir, err)
	}
	f.step("staged: %s/%s exists, nothing fetched", release.ReleasesDir, name)
	return &Result{Version: name, Dir: dir, Existed: true}, nil
}

// fetch downloads and checks version name and stages it.
func (f *Fetcher) fetch(s *stage, name string) (*Result, error) {
	l := s.hold.Layout
	target := l.Release(name)
	partial := target + ".partial"
	if err := os.RemoveAll(partial); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStage, err)
	}
	if err := os.Mkdir(partial, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStage, err)
	}
	if err := f.download(s, name, partial); err != nil {
		os.RemoveAll(partial)
		return nil, err
	}
	if err := modes(partial); err != nil {
		os.RemoveAll(partial)
		return nil, fmt.Errorf("%w: %v", ErrStage, err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		os.RemoveAll(partial)
		return nil, fmt.Errorf("%w: %s appeared while the fetch ran; it stays as it is and nothing was staged", ErrStage, target)
	}
	if err := os.Rename(partial, target); err != nil {
		os.RemoveAll(partial)
		return nil, fmt.Errorf("%w: %s appeared while the fetch ran, or the rename failed: %v; nothing was staged", ErrStage, target, err)
	}
	if err := syncDir(l.Releases()); err != nil {
		os.RemoveAll(target)
		return nil, fmt.Errorf("%w: %v", ErrStage, err)
	}
	f.step("staged: %s/%s", release.ReleasesDir, name)
	return &Result{Version: name, Dir: target}, nil
}

// download fetches the files of version name into partial and checks them.
func (f *Fetcher) download(s *stage, name, partial string) error {
	sums, err := f.getFile(s, name, release.SumsFile, partial, s.set.Limits.Sums, release.ErrNoSums)
	if err != nil {
		return err
	}
	bundle, err := f.getFile(s, name, release.BundleFile, partial, s.set.Limits.Bundle, release.ErrNoBundle)
	if err != nil {
		return err
	}
	if err := verifySignature(s.key, sums, bundle); err != nil {
		return err
	}
	f.step("signature: %s verified with the trusted key", release.SumsFile)
	lines, err := release.ParseSums(sums)
	if err != nil {
		return err
	}
	goos, goarch := f.platform()
	bin := release.BinaryName(goos, goarch)
	if lineOf(lines, bin) == "" {
		return fmt.Errorf("%w: %s, the binary of this platform", release.ErrNotListed, bin)
	}
	if err := f.get(s, name, bin, partial, s.set.Limits.Binary); err != nil {
		return err
	}
	if _, err := release.Check(partial, release.Options{TrustedKey: s.key, GOOS: goos, GOARCH: goarch}); err != nil {
		return err
	}
	f.step("checksum: SHA-256 of %s equals its line in %s", bin, release.SumsFile)

	want := lineOf(lines, BillFile)
	if want == "" {
		return fmt.Errorf("%w: %s has no line in %s", ErrBill, BillFile, release.SumsFile)
	}
	bill, err := f.getFile(s, name, BillFile, partial, s.set.Limits.Bill, ErrBill)
	if err != nil {
		return err
	}
	if got := hexSHA256(bill); got != want {
		return fmt.Errorf("%w: the SHA-256 of %s is %s, %s lists %s", ErrBill, BillFile, got, release.SumsFile, want)
	}
	n, err := checkBill(filepath.Join(partial, bin), bill)
	if err != nil {
		return err
	}
	f.step("bill: the %d modules of %s are named in %s", n, bin, BillFile)

	if err := os.Chmod(filepath.Join(partial, bin), 0o700); err != nil { // #nosec G302 -- the checked binary runs once for its version answer, for the owner only.
		return fmt.Errorf("%w: %v", ErrStage, err)
	}
	answer, err := release.RunVersion(filepath.Join(partial, bin), 0)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrAnswer, err)
	}
	if answer.String() != name {
		return fmt.Errorf("%w: %s answers version %s, and the forge names it v%s", ErrAnswer, bin, answer, name)
	}
	f.step("version: %s answers %s", bin, answer)
	return nil
}

func lineOf(lines []release.Sum, name string) string {
	for _, l := range lines {
		if l.Name == name {
			return l.SHA256
		}
	}
	return ""
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// getFile downloads one small file into dir and returns its content; a 404
// is the refusal missing.
func (f *Fetcher) getFile(s *stage, name, file, dir string, limit int64, missing error) ([]byte, error) {
	err := f.get(s, name, file, dir, limit)
	if errors.Is(err, errNotFound) {
		return nil, fmt.Errorf("%w: v%s at %s answers 404", missing, name, s.set.Server.Redacted())
	}
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, file)) // #nosec G304 -- a fixed release file name in the private partial directory.
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStage, err)
	}
	return data, nil
}

// get downloads one file of version name into dir.
func (f *Fetcher) get(s *stage, name, file, dir string, limit int64) error {
	out, err := os.OpenFile(filepath.Join(dir, file), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a fixed release file name in the private partial directory.
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStage, err)
	}
	n, err := s.c.download(s.fileURL(name, file), limit, out)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("%w of %s of v%s: %w", ErrDownload, file, name, err)
	}
	f.step("downloaded: %s (%d bytes)", file, n)
	return nil
}

// modes makes the staged files readable by the service user and the binary
// executable, and syncs the directory.
func modes(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		mode := os.FileMode(0o644)
		if strings.HasPrefix(e.Name(), "pipesmith-registry-") {
			mode = 0o755
		}
		if err := os.Chmod(filepath.Join(dir, e.Name()), mode); err != nil { // #nosec G302 -- release files are read by the service user.
			return err
		}
	}
	if err := os.Chmod(dir, 0o755); err != nil { // #nosec G302 -- a release directory is readable by the service user.
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- the releases directory of the install root.
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
