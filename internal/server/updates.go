package server

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/release"
	"github.com/iogentic/pipesmith-registry/internal/version"
)

// GET /api/updates shows the update and signing state of the install root
// the server runs from, read only: update.json, the trusted key it names,
// the answer of the owner and mode rule, the fingerprint of the key and the
// release current points to. The server never writes the trusted key, so a
// web session cannot replace the root of trust.
//
// The install root is the one of the running binary: its path, links
// resolved, is <root>/releases/<version>/pipesmith-registry and <root>/current
// is a link. A server started anywhere else answers that it is not installed
// through an install root.

// updatesAnswer is the answer of GET /api/updates.
type updatesAnswer struct {
	Installed bool           `json:"installed"`
	Message   string         `json:"message,omitempty"`
	Binary    string         `json:"binary"`
	Root      string         `json:"root,omitempty"`
	Config    *updatesConfig `json:"config,omitempty"`
	Key       *updatesKey    `json:"key,omitempty"`
	Check     *updatesCheck  `json:"check,omitempty"`
	Release   *updatesRel    `json:"release,omitempty"`

	Version      string          `json:"version"`
	Repository   *updatesRepo    `json:"repository,omitempty"`
	Source       *updatesSource  `json:"source"`
	Status       string          `json:"status"`
	Latest       *cachedRelease  `json:"latest"`
	Releases     []cachedRelease `json:"releases"`
	Auto         *updatesAuto    `json:"auto,omitempty"`
	LastUpdate   *updatesEntry   `json:"last_update"`
	History      []updatesEntry  `json:"history,omitempty"`
	HistoryError string          `json:"history_error,omitempty"`
}

type updatesConfig struct {
	Path  string `json:"path"`
	Found bool   `json:"found"`
	Error string `json:"error"`
}

// updatesKey is the trusted key. State is good, missing, refused, or
// unchecked when the server has no trust rule.
type updatesKey struct {
	Path    string `json:"path"`
	Found   bool   `json:"found"`
	State   string `json:"state"`
	Problem string `json:"problem"`
	SHA256  string `json:"sha256"` // hex of the SHA-256 of the DER form
	Hint    string `json:"hint"`   // the same in base64, the key hint of cosign
}

type updatesCheck struct {
	Ran     bool   `json:"ran"`
	Passed  bool   `json:"passed"`
	Message string `json:"message"`
}

// updatesRel is the release the install root last verified. From names the
// source: current, the link, while no record of verified releases exists.
type updatesRel struct {
	Name string `json:"name"`
	From string `json:"from"`
	Note string `json:"note"`
}

// loadUpdateConfig reads update.json; tests replace it.
var loadUpdateConfig = release.LoadUpdateConfig

const notInstalled = "This server is not installed through an install root."

// installRootOf is the install root of the binary at exe and the path of
// the binary with its links resolved; the root is empty when the binary does
// not lie at <root>/releases/<version>/pipesmith-registry beside a link
// <root>/current.
func installRootOf(exe string) (string, string) {
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", exe
	}
	if real, err = filepath.Abs(real); err != nil {
		return "", exe
	}
	releases := filepath.Dir(filepath.Dir(real))
	if filepath.Base(real) != release.InstalledBinary || filepath.Base(releases) != release.ReleasesDir {
		return "", real
	}
	root := filepath.Dir(releases)
	if info, err := os.Lstat(filepath.Join(root, release.CurrentLink)); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		return "", real
	}
	return root, real
}

// runningRoot is installRootOf for the running executable.
func runningRoot() (string, string) {
	exe, err := os.Executable()
	if err != nil {
		return "", ""
	}
	return installRootOf(exe)
}

func (s *Server) apiUpdates(w *recorder, req *request, _ []string) error {
	body, err := json.Marshal(s.updatesAnswerOf(req.r.Context(), false))
	if err != nil {
		return err
	}
	writeBody(w, req, http.StatusOK, [][2]string{{"Content-Type", jsonType}}, body)
	return nil
}

// updatesAnswerOf is the answer of GET /api/updates; check asks the source
// for its releases first.
func (s *Server) updatesAnswerOf(ctx context.Context, check bool) updatesAnswer {
	a := s.updatesState()
	s.releasePart(ctx, &a, a.Root, check)
	if a.Installed {
		s.autoPart(ctx, &a, a.Root)
	}
	return a
}

func (s *Server) updatesState() updatesAnswer {
	root, binary := s.installRoot()
	if root == "" {
		return updatesAnswer{Message: notInstalled + " Its binary " + binary + " does not lie at <root>/releases/<version>/" +
			release.InstalledBinary + " beside a link <root>/current, so there is no update.json and no trusted key to show.", Binary: binary}
	}
	a := updatesAnswer{Installed: true, Binary: binary, Root: root,
		Config: &updatesConfig{Path: filepath.Join(root, release.ConfigFile)}, Key: &updatesKey{}, Check: &updatesCheck{}}
	if _, err := os.Lstat(a.Config.Path); err == nil {
		a.Config.Found = true
		cfg, err := loadUpdateConfig(root)
		switch {
		case errors.Is(err, fs.ErrPermission):
			a.Config.Error = err.Error()
			a.Key.State, a.Key.Problem = "unreadable", fmt.Sprintf("This server, uid %d, cannot read %s, so it cannot show the key. "+
				"update and rollback read it as the owner of the install root and are not affected. To show the key here, make the file readable: "+
				"sudo chmod 0644 %s; the owner and mode rule allows that.", os.Geteuid(), a.Config.Path, a.Config.Path)
		case err != nil:
			a.Config.Error = err.Error()
		default:
			a.Key.Path = cfg.TrustedKey
		}
	}
	s.readKey(a.Key, a.Config)
	if s.trust != nil {
		a.Check.Ran = true
		if err := s.trust(root, a.Key.Path); err != nil {
			a.Check.Message = err.Error()
		} else {
			a.Check.Passed = true
		}
	}
	switch {
	case a.Key.State != "":
	case a.Check.Ran && !a.Check.Passed:
		a.Key.State, a.Key.Problem = "refused", a.Check.Message
	case !a.Check.Ran:
		a.Key.State, a.Key.Problem = "unchecked", "This server has no owner and mode rule to check the key with."
	default:
		a.Key.State = "good"
	}
	a.Release = lastRelease(root)
	return a
}

// readKey fills the path, presence and fingerprint of the key, and the
// state missing or refused when the key cannot be used whatever its owner.
func (s *Server) readKey(k *updatesKey, cfg *updatesConfig) {
	switch {
	case k.State != "":
		return
	case !cfg.Found:
		k.State, k.Problem = "missing", cfg.Path+" does not exist."
		return
	case cfg.Error != "":
		k.State, k.Problem = "refused", cfg.Error
		return
	case k.Path == "":
		k.State, k.Problem = "missing", cfg.Path+" names no trusted_key."
		return
	}
	if _, err := os.Stat(k.Path); errors.Is(err, fs.ErrNotExist) {
		k.State, k.Problem = "missing", k.Path+" does not exist."
		return
	}
	k.Found = true
	key, err := release.LoadTrustedKey(k.Path)
	if err != nil {
		k.State, k.Problem = "refused", err.Error()
		return
	}
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		k.State, k.Problem = "refused", err.Error()
		return
	}
	sum := sha256.Sum256(der)
	k.SHA256, k.Hint = hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(sum[:])
}

// lastRelease is the release of the last switch that ended healthy in
// releases/history.jsonl, and the release current points to when the file
// holds none.
func lastRelease(root string) *updatesRel {
	if e, err := release.LastSwitch(filepath.Join(root, release.ReleasesDir)); err == nil && e != nil {
		return &updatesRel{Name: e.To, From: "history",
			Note: "The last " + e.Action + " that ended healthy, at " + e.Time + ", from releases/" + release.HistoryFile + "."}
	}
	name, err := release.Layout{Root: root}.ReadLink(release.CurrentLink)
	if err != nil {
		return &updatesRel{From: release.CurrentLink, Note: err.Error()}
	}
	return &updatesRel{Name: name, From: release.CurrentLink,
		Note: "Read from the link current: releases/" + release.HistoryFile + " holds no update that ended healthy."}
}

// The release part of GET /api/updates: the running version, the releases
// of the channel with their notes, the scheduled update and the history.

type updatesRepo struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	ReleasesURL string `json:"releases_url"`
}

type updatesSource struct {
	Kind          string `json:"kind"` // github or forge
	Name          string `json:"name"`
	State         string `json:"state"`
	Message       string `json:"message"`
	Note          string `json:"note"`
	CheckedAt     string `json:"checked_at"`
	RetryAt       string `json:"retry_at"`
	NextCheckAt   string `json:"next_check_at"`
	IntervalHours int    `json:"interval_hours"`
	Background    bool   `json:"background"`
	Skipped       string `json:"skipped,omitempty"`
}

type updatesAuto struct {
	Enabled bool             `json:"enabled"`
	Window  string           `json:"window"`
	Error   string           `json:"error"`
	Timer   timerState       `json:"timer"`
	LastRun *release.LastRun `json:"last_run"`
}

type updatesEntry struct {
	ID     string `json:"id"`
	Time   string `json:"time"`
	Action string `json:"action"`
	Result string `json:"result"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
	Auto   bool   `json:"auto"`
}

// Status of the running version against the newest release.
const (
	statusUpToDate  = "up_to_date"
	statusAvailable = "update_available"
	statusUnknown   = "unknown"
)

// maxHistoryShown bounds the history lines of the answer.
const maxHistoryShown = 50

// releasePart fills the release part of a; skipped is the note of a check
// the button asked for and that sent no request.
func (s *Server) releasePart(ctx context.Context, a *updatesAnswer, root string, check bool) {
	a.Version = version.Version
	set, hours, note, err := releaseSettings(root)
	a.Source = &updatesSource{Note: note, IntervalHours: hours, Background: root != "" && hours > 0}
	if err != nil {
		a.Source.State, a.Source.Message = checkError, err.Error()
		a.Status = statusUnknown
		return
	}
	name := sourceName(set)
	a.Source.Name = name
	a.Source.Kind = "forge"
	if set.GitHub != nil {
		a.Source.Kind = "github"
		a.Repository = &updatesRepo{Name: name, URL: "https://github.com/" + name, ReleasesURL: "https://github.com/" + name + "/releases"}
	}
	var st releaseCache
	if check {
		st, a.Source.Skipped = s.releases.check(ctx, set, name)
	} else {
		st = s.releases.state()
	}
	if st.Source != "" && st.Source != name {
		// the cache belongs to another channel than the one of update.json now
		st = releaseCache{State: checkNever}
	}
	a.Source.State, a.Source.Message, a.Source.CheckedAt, a.Source.RetryAt = st.State, st.Message, st.CheckedAt, st.RetryAt
	if a.Source.Background {
		if next := s.releases.nextAt(hours); !next.IsZero() {
			a.Source.NextCheckAt = next.UTC().Format(time.RFC3339)
		}
	}
	a.Releases = st.Releases
	if a.Releases == nil {
		a.Releases = []cachedRelease{}
	}
	a.Latest = newest(a.Releases)
	a.Status = statusOf(a.Version, a.Latest)
}

// newest is the release of the highest version that is no prerelease.
func newest(list []cachedRelease) *cachedRelease {
	var best *cachedRelease
	var bestV release.Version
	for i := range list {
		if list[i].Prerelease {
			continue
		}
		v, err := release.ParseVersion(list[i].Version)
		if err != nil {
			continue
		}
		if best == nil || v.Compare(bestV) > 0 {
			best, bestV = &list[i], v
		}
	}
	return best
}

func statusOf(running string, latest *cachedRelease) string {
	if latest == nil {
		return statusUnknown
	}
	r, err := release.ParseVersion(running)
	l, lerr := release.ParseVersion(latest.Version)
	switch {
	case err != nil || lerr != nil:
		return statusUnknown
	case l.Compare(r) > 0:
		return statusAvailable
	}
	return statusUpToDate
}

// autoPart fills the scheduled update and the history of an install root.
func (s *Server) autoPart(ctx context.Context, a *updatesAnswer, root string) {
	auto := &updatesAuto{Timer: readTimer(ctx, s.timer)}
	if cfg, err := loadUpdateConfig(root); err != nil {
		auto.Error = err.Error()
	} else {
		if auto.Enabled, err = cfg.ScheduledSwitch(); err != nil {
			auto.Error = err.Error()
		}
		if w, ok, err := cfg.Window(); err != nil {
			auto.Error = joinText(auto.Error, err.Error())
		} else if ok {
			auto.Window = w.Text
		}
	}
	if run, err := release.ReadLastRun(root); err != nil {
		auto.Error = joinText(auto.Error, err.Error())
	} else {
		auto.LastRun = run
	}
	a.Auto = auto
	a.History = []updatesEntry{}
	lines, err := release.History(filepath.Join(root, release.ReleasesDir), maxHistoryShown)
	if err != nil {
		a.HistoryError = err.Error()
	}
	for _, e := range lines {
		u := updatesEntry{ID: e.ID, Time: e.Time, Action: e.Action, Result: e.Result, From: e.From, To: e.To, Reason: e.Reason, Auto: e.Auto}
		a.History = append(a.History, u)
		if a.LastUpdate == nil && e.Result == release.ResultOK {
			last := u
			a.LastUpdate = &last
		}
	}
}

// apiUpdatesCheck is POST /api/updates/check: a check of the releases now,
// then the answer of GET /api/updates. It installs nothing.
func (s *Server) apiUpdatesCheck(w *recorder, req *request, _ []string) error {
	body, err := json.Marshal(s.updatesAnswerOf(req.r.Context(), true))
	if err != nil {
		return err
	}
	writeBody(w, req, http.StatusOK, [][2]string{{"Content-Type", jsonType}}, body)
	return nil
}
