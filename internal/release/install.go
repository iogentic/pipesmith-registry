package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Refusals and failures of Update and Rollback.
var (
	ErrOwner   = errors.New("not the owner of the install root")
	ErrLocked  = errors.New("another update or rollback runs")
	ErrInstall = errors.New("install failed")
	ErrRestart = errors.New("restart failed")
	ErrHealth  = errors.New("health check failed")
)

// Defaults of the time limits of an Updater.
const (
	DefaultHealthTimeout  = 30 * time.Second
	DefaultHealthInterval = 500 * time.Millisecond
	DefaultRequestTimeout = 2 * time.Second
	DefaultRestartTimeout = 2 * time.Minute
	DefaultVersionTimeout = 10 * time.Second
)

// maxAnswer bounds what is read of an answer of the server or of
// `<binary> version`.
const maxAnswer = 64 << 10

// ServiceManager restarts and stops a service unit.
type ServiceManager interface {
	Restart(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
}

// ActiveReporter is a ServiceManager that says whether a unit is active.
// The health wait asks it when the ServiceManager has it; Systemctl has it.
type ActiveReporter interface {
	Active(ctx context.Context, unit string) (bool, error)
}

// Systemctl restarts and stops a unit with systemctl, without a shell.
type Systemctl struct{}

// Restart runs systemctl reset-failed -- unit, then systemctl restart --
// unit. The reset clears the start limit that earlier restarts or a release
// that failed its start left, so the restart of an update or of its way back
// is not refused with start-limit-hit. A stopped unit that has not failed
// is not loaded and reset-failed fails for it, so its result is not used.
func (Systemctl) Restart(ctx context.Context, unit string) error {
	_ = systemctl(ctx, "reset-failed", unit) // best effort, see above
	return systemctl(ctx, "restart", unit)
}

// Stop runs systemctl stop -- unit.
func (Systemctl) Stop(ctx context.Context, unit string) error { return systemctl(ctx, "stop", unit) }

// Active runs systemctl is-active --quiet -- unit: exit code 0 is active,
// another exit code is not.
func (Systemctl) Active(ctx context.Context, unit string) (bool, error) {
	if !ValidUnit(unit) {
		return false, fmt.Errorf("unit %q is not a plain service unit name", unit)
	}
	var out capped
	cmd := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "--", unit) // #nosec G204 -- a fixed program, fixed arguments and a checked unit name, no shell.
	cmd.Stdout, cmd.Stderr = &out, &out
	ownGroup(cmd)
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit):
		return false, nil
	}
	return false, fmt.Errorf("systemctl is-active %s: %v: %s", unit, err, strings.TrimSpace(out.String()))
}

func systemctl(ctx context.Context, verb, unit string) error {
	if !ValidUnit(unit) {
		return fmt.Errorf("unit %q is not a plain service unit name", unit)
	}
	var out capped
	cmd := exec.CommandContext(ctx, "systemctl", verb, "--", unit) // #nosec G204 -- a fixed program, a fixed verb and a checked unit name, no shell.
	cmd.Stdout, cmd.Stderr = &out, &out
	ownGroup(cmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl %s %s: %v: %s", verb, unit, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// Updater runs update and rollback on an install root.
type Updater struct {
	Root    string
	Service ServiceManager
	// Address gives the base URL of the health wait, such as
	// http://127.0.0.1:14193, from the configuration.
	Address func(*UpdateConfig) (string, error)
	// HealthCert gives the PEM file of the certificate the server presents,
	// tls_cert of config.json, or "" for plain HTTP; nil means "".
	HealthCert func(*UpdateConfig) (string, error)
	// Out receives one line per finished step; nil discards them.
	Out io.Writer
	// Auto marks the history lines as those of a scheduled run.
	Auto bool

	// Zero values take the defaults above.
	HealthTimeout  time.Duration
	HealthInterval time.Duration
	RequestTimeout time.Duration
	RestartTimeout time.Duration
	VersionTimeout time.Duration

	euid    func() int
	ownerOf func(string) (int, error)
	statOf  func(string) (fileStat, error)
	hook    func(point string) error
	now     func() time.Time // the clock; nil is time.Now
	began   bool             // the last update or rollback wrote the record of its switch
	backOK  bool             // the way back of the last switch ended with the release before answering healthy
	// freeSpace replaces migrate.FreeSpace in tests.
	freeSpace func(dir string) (uint64, error)
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (u *Updater) step(format string, args ...any) {
	if u.Out != nil {
		fmt.Fprintf(u.Out, format+"\n", args...)
	}
}

// at is a point where a test injects a failure.
func (u *Updater) at(point string) error {
	if u.hook == nil {
		return nil
	}
	return u.hook(point)
}

// session is the state an update or rollback starts from.
type session struct {
	l           Layout
	cfg         *UpdateConfig
	base        string
	pin         *healthPin // the certificate of the health wait over TLS; nil for plain HTTP
	current     string     // release name current points to
	prevTarget  string     // target of previous as written, when prevExisted
	prevExisted bool
	unlock      func()
	owner       int // uid of the root
	statOf      func(string) (fileStat, error)
	dataDir     string         // data_dir of update.json
	plan        *schemaPlan    // the files the new release migrates
	halted      bool           // the way back stopped before its end; the record of the switch stays
	action      string         // update or rollback, for the record of the switch
	signals     chan os.Signal // SIGINT, SIGHUP and SIGTERM held from the first record on
}

// begin checks the owner and the owners and modes of the root, takes the
// lock, reads update.json and the links, and resolves the address of the
// health wait. withKey checks the trusted key as well. It changes nothing.
func (u *Updater) begin(withKey bool) (*session, error) {
	u.began = false
	u.backOK = false
	l := Layout{Root: u.Root}
	euid, ownerOf, statOf := os.Geteuid, fileOwner, lstatFile
	if u.euid != nil {
		euid = u.euid
	}
	if u.ownerOf != nil {
		ownerOf = u.ownerOf
	}
	if u.statOf != nil {
		statOf = u.statOf
	}
	owner, err := ownerRule(l.Root, euid, ownerOf)
	if err != nil {
		return nil, err
	}
	if err := rootRule(l.Root, owner, statOf); err != nil {
		return nil, err
	}
	unlock, err := lockRoot(l.Root)
	if err != nil {
		return nil, err
	}
	s := &session{l: l, owner: owner, statOf: statOf}
	s.unlock = func() {
		u.releaseSignals(s)
		unlock()
	}
	if err := u.read(s, withKey, statOf); err != nil {
		unlock()
		return nil, err
	}
	return s, nil
}

func (u *Updater) read(s *session, withKey bool, statOf func(string) (fileStat, error)) error {
	u.step("owner: uid of %s, lock taken", s.l.Root)
	cfg, err := LoadUpdateConfig(s.l.Root)
	if err != nil {
		return err
	}
	s.cfg = cfg
	if withKey {
		if err := keyRule(cfg.TrustedKey, s.owner, statOf); err != nil {
			return err
		}
	}
	if s.dataDir, err = RequireDataDir(cfg); err != nil {
		return err
	}
	current, err := s.l.ReadLink(CurrentLink)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s does not exist; the first release is installed by hand", ErrLayout, s.l.path(CurrentLink))
	}
	if err != nil {
		return fmt.Errorf("%w; the first release is installed by hand", err)
	}
	s.current = current
	target, err := s.l.rawLink(PreviousLink)
	switch {
	case err == nil:
		s.prevTarget, s.prevExisted = target, true
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	base, cert, key, err := u.HealthTarget(cfg)
	if err != nil {
		return err
	}
	s.base = base
	if cert != "" {
		if s.pin, err = loadHealthPin(cert, key); err != nil {
			return err
		}
	}
	if err := u.unitRule(s); err != nil {
		return err
	}
	u.step("current: %s/%s", ReleasesDir, current)
	u.listCopies(s.dataDir)
	return nil
}

// HealthTarget gives the base URL of the health wait and, for a wait over
// TLS, the certificate file and the key that named it: health_tls_cert of
// update.json, else the file of HealthCert.
func (u *Updater) HealthTarget(cfg *UpdateConfig) (base, cert, key string, err error) {
	switch {
	case cfg.HealthTLSCert != "":
		cert, key = cfg.HealthTLSCert, "health_tls_cert of "+ConfigFile
	case u.HealthCert != nil:
		if cert, err = u.HealthCert(cfg); err != nil {
			return "", "", "", err
		}
		key = "tls_cert of config.json"
	}
	switch {
	case cfg.HealthAddress != "":
		base = "http://" + cfg.HealthAddress
	case u.Address == nil:
		return "", "", "", fmt.Errorf("%w: no address for the health wait", ErrConfig)
	default:
		if base, err = u.Address(cfg); err != nil {
			return "", "", "", err
		}
	}
	if cert != "" {
		base = "https://" + strings.TrimPrefix(base, "http://")
	}
	return base, cert, key, nil
}

// restore puts current and previous back to what they were at begin.
func (u *Updater) restore(s *session) error {
	if err := s.l.SwapLink(CurrentLink, s.current); err != nil {
		return err
	}
	return s.l.restoreLink(PreviousLink, s.prevTarget, s.prevExisted)
}

// UpdateOptions are the flags of update.
type UpdateOptions struct {
	AllowDowngrade  bool // switch to a version not higher than the running one, or without a schema plan
	NoCatalogBackup bool // migrate catalog.sqlite without a copy when the space for one is missing
}

// Update installs the release in dir and switches to it. See the steps in
// docs/plans/step-5-release.md and work/plans/exec/session-phase8.md.
func (u *Updater) Update(dir string, allowDowngrade bool) error {
	return u.UpdateWith(dir, UpdateOptions{AllowDowngrade: allowDowngrade})
}

// Switch is UpdateWith on releases/<version>/ of the root, the directory the
// fetch stage stages. version is X.Y.Z, with or without a leading v.
func (u *Updater) Switch(version string, opts UpdateOptions) error {
	v, err := ParseVersion(strings.TrimPrefix(version, "v"))
	if err != nil {
		return err
	}
	return u.UpdateWith(Layout{Root: u.Root}.Release(v.String()), opts)
}

// UpdateWith is Update with every flag.
func (u *Updater) UpdateWith(dir string, opts UpdateOptions) (err error) {
	allowDowngrade := opts.AllowDowngrade
	s, err := u.begin(true)
	if err != nil {
		return err
	}
	defer s.unlock()
	rec := &recorder{u: u, s: s, action: "update"}
	defer func() { rec.finish(err) }()
	if err := u.endPending(s, rec); err != nil {
		return err
	}
	if err := u.removeLeftovers(s); err != nil {
		return err
	}

	rel, err := Check(dir, Options{TrustedKey: s.cfg.TrustedKey})
	if err != nil {
		return err
	}
	u.step("checked: signature of %s and SHA-256 of %s", SumsFile, rel.Name)

	staging, err := os.MkdirTemp(s.l.Releases(), ".staging-")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInstall, err)
	}
	defer os.RemoveAll(staging)
	bin := filepath.Join(staging, InstalledBinary)
	if err := u.at("copy"); err != nil {
		return fmt.Errorf("%w: copy of %s: %v", ErrInstall, rel.Name, err)
	}
	if err := copyBinary(rel, bin); err != nil {
		return fmt.Errorf("%w: copy of %s: %v", ErrInstall, rel.Name, err)
	}
	if err := u.at("copied"); err != nil {
		return fmt.Errorf("%w: copy of %s: %v", ErrInstall, rel.Name, err)
	}
	if err := rel.VerifyFile(bin); err != nil {
		return err
	}

	v, err := u.runVersion(bin)
	if err != nil {
		return err
	}
	rec.to = v.String()
	if err := versionRule(v, s.current, allowDowngrade); err != nil {
		return err
	}
	u.step("version: %s, current runs %s", v, s.current)
	if s.plan, err = u.planSchema(s, bin, v, opts); err != nil {
		return err
	}
	if err := u.checkSpace(s.plan, opts.NoCatalogBackup); err != nil {
		return err
	}
	if err := u.writeCheck(s.plan); err != nil {
		return err
	}
	if err := u.requestCopies(s.plan); err != nil {
		return err
	}
	defer u.dropRequests(s.plan)

	created, err := u.install(s, rel, staging, v.String())
	if err != nil {
		return err
	}
	if v.String() == s.current {
		rec.inPlace = true
		return u.restartInPlace(s, created)
	}
	if err := u.writePending(s, "update", v.String()); err != nil {
		removeCreated(created)
		return fmt.Errorf("%w: the record %s: %v", ErrInstall, PendingFile, err)
	}
	if err := u.switchTo(s, v.String()); err != nil {
		removeCreated(created)
		u.clearPending(s)
		return fmt.Errorf("%w: switch to %s/%s: %v", ErrInstall, ReleasesDir, v, err)
	}
	u.step("switched: current -> %s/%s, previous -> %s/%s", ReleasesDir, v, ReleasesDir, s.current)
	rec.back = true
	if err := u.at("kill-after-switch"); err != nil {
		return err
	}
	if err := u.restartAndWait(s, v.String()); err != nil {
		return u.wayBack(s, v.String(), created, err)
	}
	if err := u.at("kill-after-restart"); err != nil {
		return err
	}
	u.clearPending(s)
	return nil
}

// removeLeftovers removes the releases/.staging-* directories that a run
// which was killed left; the lock of the root is held, so no update uses them.
func (u *Updater) removeLeftovers(s *session) error {
	entries, err := os.ReadDir(s.l.Releases())
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInstall, err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".staging-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.l.Releases(), e.Name())); err != nil {
			return fmt.Errorf("%w: a leftover of a run that stopped: %v", ErrInstall, err)
		}
		u.step("removed: %s/%s, left by a run that stopped", ReleasesDir, e.Name())
	}
	return nil
}

// Rollback switches current to the release previous points to.
func (u *Updater) Rollback() (err error) {
	s, err := u.begin(false)
	if err != nil {
		return err
	}
	defer s.unlock()
	rec := &recorder{u: u, s: s, action: "rollback"}
	defer func() { rec.finish(err) }()
	if err := u.endPending(s, rec); err != nil {
		return err
	}
	prev, err := s.l.ReadLink(PreviousLink)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s does not exist; there is no release to go back to", ErrLayout, s.l.path(PreviousLink))
	}
	if err != nil {
		return err
	}
	if prev == s.current {
		return fmt.Errorf("%w: previous and current both point to %s/%s", ErrLayout, ReleasesDir, prev)
	}
	if info, err := os.Lstat(filepath.Join(s.l.Release(prev), InstalledBinary)); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s/%s holds no %s", ErrLayout, ReleasesDir, prev, InstalledBinary)
	}
	rec.to = prev
	u.step("previous: %s/%s", ReleasesDir, prev)
	if err := u.checkBefore(s, prev); err != nil {
		return err
	}
	if err := u.rollbackSchema(s, prev); err != nil {
		return err
	}
	if err := u.writePending(s, "rollback", prev); err != nil {
		return fmt.Errorf("%w: the record %s: %v", ErrInstall, PendingFile, err)
	}
	if err := u.switchTo(s, prev); err != nil {
		u.clearPending(s)
		return fmt.Errorf("%w: switch to %s/%s: %v", ErrInstall, ReleasesDir, prev, err)
	}
	u.step("switched: current -> %s/%s, previous -> %s/%s", ReleasesDir, prev, ReleasesDir, s.current)
	rec.back = true
	if err := u.restartAndWait(s, u.versionOf(s, prev)); err != nil {
		return u.wayBack(s, prev, "", err)
	}
	u.clearPending(s)
	return nil
}

// switchTo points previous at the release current points to, then current at
// name. A failure puts both back.
func (u *Updater) switchTo(s *session, name string) error {
	if err := u.at("swap-previous"); err != nil {
		return err
	}
	if err := s.l.SwapLink(PreviousLink, s.current); err != nil {
		return errors.Join(err, s.l.restoreLink(PreviousLink, s.prevTarget, s.prevExisted))
	}
	err := u.at("swap-current")
	if err == nil {
		err = s.l.SwapLink(CurrentLink, name)
	}
	if err != nil {
		return errors.Join(err, u.restore(s))
	}
	return nil
}

// wayBack returns to the release that ran before, after a failed restart or
// health wait of name.
func (u *Updater) wayBack(s *session, name, created string, cause error) error {
	failed := fmt.Errorf("switch to %s/%s failed: %w", ReleasesDir, name, cause)
	if err := u.markBack(s, name); err != nil {
		return fmt.Errorf("%w; the way back did not start, current points to %s/%s: the record %s: %v", failed, ReleasesDir, name, PendingFile, err)
	}
	if err := u.restore(s); err != nil {
		return fmt.Errorf("%w; the way back failed, current may still point to %s/%s: %v", failed, ReleasesDir, name, err)
	}
	u.step("switched back: current -> %s/%s", ReleasesDir, s.current)
	if s.plan != nil && len(s.plan.files) > 0 {
		err := u.wayBackSchema(s, created, failed)
		if !s.halted {
			u.clearPending(s)
		}
		return err
	}
	err := u.restartAndWait(s, u.versionOf(s, s.current))
	removeCreated(created)
	u.clearPending(s)
	if err != nil {
		return fmt.Errorf("%w; went back to %s/%s, which does not answer either: %v", failed, ReleasesDir, s.current, err)
	}
	u.backOK = true
	return fmt.Errorf("%w; went back to %s/%s, which answers again", failed, ReleasesDir, s.current)
}

// restartInPlace restarts the release current runs already, installed again:
// the links stay as they are, also when the restart or the health wait fails.
func (u *Updater) restartInPlace(s *session, created string) error {
	u.step("links kept: current runs %s already", s.current)
	if err := u.restartAndWait(s, s.current); err != nil {
		removeCreated(created)
		return fmt.Errorf("restart of %s/%s failed: %w; current and previous are unchanged", ReleasesDir, s.current, err)
	}
	return nil
}

// versionOf is the version the binary of releases/<name>/ answers, for a
// directory installed by hand under another name than its version; name
// when the binary gives no version, or when the directory fails the owner
// and mode check, so its binary does not run as the owner of the root.
func (u *Updater) versionOf(s *session, name string) string {
	if _, err := ParseVersion(name); err == nil {
		return name
	}
	if err := u.trustRelease(s, name); err != nil {
		u.step("not run: %s/%s/%s, for its version: %v; the health wait asks for the version %s", ReleasesDir, name, InstalledBinary, err, name)
		return name
	}
	v, err := u.runVersion(filepath.Join(s.l.Release(name), InstalledBinary))
	if err != nil {
		return name
	}
	return v.String()
}

func removeCreated(path string) {
	if path != "" {
		os.RemoveAll(path)
	}
}

func versionRule(v Version, current string, allowDowngrade bool) error {
	if allowDowngrade {
		return nil
	}
	cur, err := ParseVersion(current)
	if err != nil {
		return fmt.Errorf("%w: current points to %s/%s, which is not a version X.Y.Z; --allow-downgrade switches anyway", ErrVersion, ReleasesDir, current)
	}
	switch v.Compare(cur) {
	case 0:
		return fmt.Errorf("%w: %s is the version current runs; --allow-downgrade installs it again", ErrVersion, v)
	case -1:
		return fmt.Errorf("%w: %s is lower than %s, the version current runs; --allow-downgrade switches anyway", ErrVersion, v, cur)
	}
	return nil
}

var versionAnswer = regexp.MustCompile(`^pipesmith-registry ([^\n]*)\n$`)

// runVersion runs `<bin> version` without a shell, with an empty environment
// and no standard input, and reads the version from its answer.
func (u *Updater) runVersion(bin string) (Version, error) {
	for attempt := 0; ; attempt++ {
		answer, _, err := u.runOnce(bin, "version")
		if err != nil && textBusy(err) && attempt < 5 {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if err != nil {
			return Version{}, fmt.Errorf("%w: %s version failed: %v", ErrVersion, InstalledBinary, err)
		}
		m := versionAnswer.FindStringSubmatch(answer)
		if m == nil {
			return Version{}, fmt.Errorf("%w: %s version answered %q, want \"pipesmith-registry X.Y.Z\\n\"", ErrVersion, InstalledBinary, answer)
		}
		v, err := ParseVersion(m[1])
		if err != nil {
			return Version{}, fmt.Errorf("%w: %s version answered %q, want \"pipesmith-registry X.Y.Z\\n\"", ErrVersion, InstalledBinary, answer)
		}
		return v, nil
	}
}

// runOnce runs `<bin> command` and returns its standard output and its
// standard error, each capped at maxAnswer bytes.
func (u *Updater) runOnce(bin, command string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), orDefault(u.VersionTimeout, DefaultVersionTimeout))
	defer cancel()
	var out, errOut capped
	cmd := exec.CommandContext(ctx, bin, command) // #nosec G204 -- the checked copy of the release binary, no shell.
	cmd.Env = []string{}
	cmd.Dir = filepath.Dir(bin)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	cmd.WaitDelay = time.Second
	ownGroup(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", "", fmt.Errorf("no answer within %s", orDefault(u.VersionTimeout, DefaultVersionTimeout))
	}
	if out.over {
		return "", "", fmt.Errorf("answer longer than %d bytes", maxAnswer)
	}
	return out.String(), errOut.String(), err
}

// capped keeps the first maxAnswer bytes written to it.
type capped struct {
	buf  bytes.Buffer
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := maxAnswer - c.buf.Len(); len(p) > room {
		c.buf.Write(p[:room])
		c.over = true
	} else {
		c.buf.Write(p)
	}
	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }

func copyBinary(rel *Release, dst string) error {
	root, err := os.OpenRoot(rel.Dir)
	if err != nil {
		return err
	}
	defer root.Close()
	in, err := openRegular(root, rel.Name)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", rel.Name)
	}
	return writeSynced(dst, in, 0o700)
}

func writeSynced(dst string, src io.Reader, mode os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) // #nosec G304 -- a path in the private staging directory.
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copySums copies SHA256SUMS and its bundle into dir and checks the copies
// as Check checked the originals, so the installed files are the checked ones.
func copySums(rel *Release, trustedKey, dir string) error {
	root, err := os.OpenRoot(rel.Dir)
	if err != nil {
		return err
	}
	defer root.Close()
	sums, err := readLimited(root, SumsFile)
	if err != nil {
		return err
	}
	bundle, err := readLimited(root, BundleFile)
	if err != nil {
		return err
	}
	key, err := LoadTrustedKey(trustedKey)
	if err != nil {
		return err
	}
	if err := VerifyBundle(key, sums, bundle); err != nil {
		return err
	}
	lines, err := ParseSums(sums)
	if err != nil {
		return err
	}
	listed := false
	for _, s := range lines {
		listed = listed || (s.Name == rel.Name && sameHex(s.SHA256, rel.SHA256))
	}
	if !listed {
		return fmt.Errorf("%w: %s changed after the check", ErrSignature, SumsFile)
	}
	if err := writeSynced(filepath.Join(dir, SumsFile), bytes.NewReader(sums), 0o644); err != nil {
		return err
	}
	return writeSynced(filepath.Join(dir, BundleFile), bytes.NewReader(bundle), 0o644)
}

// install puts the checked binary at releases/<name>/pipesmith-registry and
// returns what it created, for removal on a later failure.
func (u *Updater) install(s *session, rel *Release, staging, name string) (string, error) {
	target := s.l.Release(name)
	bin := filepath.Join(staging, InstalledBinary)
	info, err := os.Lstat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return u.installNew(s, rel, staging, target)
	case err != nil:
		return "", fmt.Errorf("%w: %v", ErrInstall, err)
	case !info.IsDir():
		return "", fmt.Errorf("%w: %s exists and is not a directory", ErrInstall, target)
	}
	inst := filepath.Join(target, InstalledBinary)
	info, err = os.Lstat(inst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := u.at("install"); err != nil {
			return "", fmt.Errorf("%w: %v", ErrInstall, err)
		}
		if err := os.Chmod(bin, 0o755); err != nil { // #nosec G302 -- the installed binary is executable by the service user.
			return "", fmt.Errorf("%w: %v", ErrInstall, err)
		}
		if err := os.Rename(bin, inst); err != nil {
			return "", fmt.Errorf("%w: %v", ErrInstall, err)
		}
		if err := errors.Join(syncDir(target), rel.VerifyFile(inst)); err != nil {
			os.Remove(inst)
			return "", fmt.Errorf("%w: %v", ErrInstall, err)
		}
		u.step("installed: %s/%s/%s", ReleasesDir, name, InstalledBinary)
		return inst, nil
	case err != nil:
		return "", fmt.Errorf("%w: %v", ErrInstall, err)
	case !info.Mode().IsRegular():
		return "", fmt.Errorf("%w: %s is not a regular file", ErrInstall, inst)
	}
	if err := rel.VerifyFile(inst); err != nil {
		return "", fmt.Errorf("%w: %s holds a %s with another SHA-256 than the checked binary", ErrInstall, target, InstalledBinary)
	}
	u.step("kept: %s/%s holds the checked binary", ReleasesDir, name)
	return "", nil
}

func (u *Updater) installNew(s *session, rel *Release, staging, target string) (string, error) {
	partial := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".installing")
	if err := os.RemoveAll(partial); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInstall, err)
	}
	bin := filepath.Join(staging, InstalledBinary)
	err := copySums(rel, s.cfg.TrustedKey, staging)
	if err == nil {
		err = os.Chmod(bin, 0o755) // #nosec G302 -- the installed binary is executable by the service user.
	}
	if err == nil {
		err = linkOrCopy(bin, filepath.Join(staging, rel.Name))
	}
	if err == nil {
		err = os.Chmod(staging, 0o755) // #nosec G302 -- the release directory is readable by the service user.
	}
	if err == nil {
		err = syncDir(staging)
	}
	if err == nil {
		err = os.Rename(staging, partial)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInstall, err)
	}
	err = rel.VerifyFile(filepath.Join(partial, InstalledBinary))
	if err == nil {
		err = u.at("install")
	}
	if err == nil {
		err = os.Rename(partial, target)
	}
	if err != nil {
		os.RemoveAll(partial)
		return "", fmt.Errorf("%w: %v", ErrInstall, err)
	}
	if err := syncDir(s.l.Releases()); err != nil {
		os.RemoveAll(target)
		return "", fmt.Errorf("%w: %v", ErrInstall, err)
	}
	u.step("installed: %s/%s", ReleasesDir, filepath.Base(target))
	return target, nil
}

// linkOrCopy puts the file src at dst as a hard link, or as a copy when the
// link fails, so releases/<version>/ holds the binary under its platform name
// too and passes Check.
func linkOrCopy(src, dst string) error {
	if os.Link(src, dst) == nil {
		return nil
	}
	in, err := os.Open(src) // #nosec G304 -- the checked binary in the private staging directory.
	if err != nil {
		return err
	}
	defer in.Close()
	return writeSynced(dst, in, 0o755)
}

// restartAndWait restarts the unit and waits until version answers healthy.
func (u *Updater) restartAndWait(s *session, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), orDefault(u.RestartTimeout, s.cfg.restartTimeout()))
	defer cancel()
	if err := u.Service.Restart(ctx, s.cfg.Unit); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrRestart, s.cfg.Unit, err)
	}
	u.step("restarted: %s", s.cfg.Unit)
	if err := u.waitHealth(s.base, s.pin, version, u.activeCheck(ctx, s.cfg.Unit)); err != nil {
		return err
	}
	u.step("healthy: %s answers with version %s", s.base, version)
	return nil
}

// activeCheck is nil error when the service manager reports unit active or
// reports no state of units.
func (u *Updater) activeCheck(ctx context.Context, unit string) func() error {
	return func() error {
		reporter, ok := u.Service.(ActiveReporter)
		if !ok {
			return nil
		}
		on, err := reporter.Active(ctx, unit)
		switch {
		case err != nil:
			return err
		case !on:
			return fmt.Errorf("the unit %s is not active", unit)
		}
		return nil
	}
}

// waitHealth waits for a healthy answer with version while active passes, so
// another process on the address of a unit that failed is no success.
func (u *Updater) waitHealth(base string, pin *healthPin, version string, active func() error) error {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	if pin != nil {
		transport.TLSClientConfig = pin.tlsConfig()
	}
	client := &http.Client{
		Timeout:       orDefault(u.RequestTimeout, DefaultRequestTimeout),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     transport,
	}
	timeout := orDefault(u.HealthTimeout, DefaultHealthTimeout)
	interval := orDefault(u.HealthInterval, DefaultHealthInterval)
	deadline := time.Now().Add(timeout)
	for {
		err := probe(client, base, version)
		if err == nil {
			err = active()
		}
		if err == nil {
			return nil
		}
		if time.Now().Add(interval).After(deadline) {
			return fmt.Errorf("%w: no healthy answer with version %s at %s within %s; last: %v", ErrHealth, version, base, timeout, err)
		}
		time.Sleep(interval)
	}
}

func probe(client *http.Client, base, version string) error {
	var health map[string]any
	if err := getJSON(client, base+"/api/health", &health); err != nil {
		return err
	}
	if health["status"] != "ok" {
		return fmt.Errorf("/api/health answered status %s", quoted(health["status"]))
	}
	var status map[string]any
	if err := getJSON(client, base+"/api/status", &status); err != nil {
		return err
	}
	if status["version"] != version {
		return fmt.Errorf("/api/status answered version %s", quoted(status["version"]))
	}
	return nil
}

// quoted is a JSON value of the server as a quoted Go string of at most 200
// bytes, so no byte of the answer reaches the output raw.
func quoted(v any) string {
	text := fmt.Sprint(v)
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	return fmt.Sprintf("%q", text)
}

func getJSON(client *http.Client, url string, dest *map[string]any) error {
	resp, err := client.Get(url) // #nosec G107 -- the local address of the configuration.
	if err != nil {
		return explain(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return err
	}
	if len(data) > maxAnswer {
		return fmt.Errorf("%s answered more than %d bytes", url, maxAnswer)
	}
	if err := json.Unmarshal(data, dest); err != nil || *dest == nil {
		return fmt.Errorf("%s answered no JSON object", url)
	}
	return nil
}
