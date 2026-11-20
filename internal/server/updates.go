package server

import (
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

	"github.com/iogentic/pipesmith-registry/internal/release"
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
	body, err := json.Marshal(s.updatesState())
	if err != nil {
		return err
	}
	writeBody(w, req, http.StatusOK, [][2]string{{"Content-Type", jsonType}}, body)
	return nil
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
