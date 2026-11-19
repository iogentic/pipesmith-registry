package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The install root holds releases/<version>/pipesmith-registry, the links
// current and previous with the relative target releases/<version>, and
// update.json.
const (
	DefaultRoot     = "/opt/pipesmith-registry"
	ReleasesDir     = "releases"
	CurrentLink     = "current"
	PreviousLink    = "previous"
	ConfigFile      = "update.json"
	InstalledBinary = "pipesmith-registry"
	DefaultUnit     = "pipesmith-registry.service"
)

// maxConfig bounds the size of update.json.
const maxConfig = 64 << 10

// Refusals of the install root, of update.json and of a version.
var (
	ErrLayout  = errors.New("install root")
	ErrConfig  = errors.New("update configuration")
	ErrVersion = errors.New("version")
)

// Version is a release version of three numbers, X.Y.Z.
type Version [3]uint64

var versionText = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

// ParseVersion parses X.Y.Z: three decimal numbers without a leading zero,
// each of at most nine digits, without a leading v.
func ParseVersion(text string) (Version, error) {
	m := versionText.FindStringSubmatch(text)
	if m == nil {
		return Version{}, fmt.Errorf("%w: %q is not X.Y.Z", ErrVersion, text)
	}
	var v Version
	for i := range v {
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("%w: %q: %v", ErrVersion, text, err)
		}
		v[i] = n
	}
	return v, nil
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// Compare returns -1, 0 or 1 as v is lower than, equal to or higher than o.
func (v Version) Compare(o Version) int {
	for i := range v {
		if v[i] < o[i] {
			return -1
		}
		if v[i] > o[i] {
			return 1
		}
	}
	return 0
}

// Layout is an install root.
type Layout struct{ Root string }

func (l Layout) path(name string) string { return filepath.Join(l.Root, name) }

// Releases is the directory of the installed releases.
func (l Layout) Releases() string { return l.path(ReleasesDir) }

// Release is the directory of the release name.
func (l Layout) Release(name string) string { return filepath.Join(l.Releases(), name) }

var releaseName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validRelease(name string) bool {
	return releaseName.MatchString(name) && !strings.HasSuffix(name, ".partial")
}

func validLink(link string) bool { return link == CurrentLink || link == PreviousLink }

// rawLink is the target of link as it is written, or an error that wraps
// fs.ErrNotExist when there is no link, or ErrLayout when the entry is not a
// symlink.
func (l Layout) rawLink(link string) (string, error) {
	p := l.path(link)
	info, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return "", fmt.Errorf("%w: %s is not a symlink", ErrLayout, p)
	}
	return os.Readlink(p)
}

// ReadLink returns the release name that link points to. The link must be a
// symlink with the relative target releases/<name>, and that directory must
// exist. A missing link gives an error that wraps fs.ErrNotExist.
func (l Layout) ReadLink(link string) (string, error) {
	target, err := l.rawLink(link)
	if err != nil {
		return "", err
	}
	p := l.path(link)
	name, ok := strings.CutPrefix(target, ReleasesDir+"/")
	if !ok || !validRelease(name) {
		return "", fmt.Errorf("%w: %s points to %q, not to %s/<version>", ErrLayout, p, target, ReleasesDir)
	}
	info, err := os.Lstat(l.Release(name))
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: %s points to %s, which is not an installed release directory", ErrLayout, p, target)
	}
	return name, nil
}

// SwapLink points link at releases/<name>: it writes a fresh link under a
// temporary name and renames it over link, so link is at every moment either
// the old or the new link.
func (l Layout) SwapLink(link, name string) error {
	if !validLink(link) || !validRelease(name) {
		return fmt.Errorf("%w: no link %q to release %q", ErrLayout, link, name)
	}
	return l.writeLink(link, ReleasesDir+"/"+name)
}

// writeLink points link at target by the rename of a fresh link.
func (l Layout) writeLink(link, target string) error {
	tmp := l.path("." + link + ".new")
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.path(link)); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(l.Root)
}

// restoreLink puts link back to a target read before with rawLink, or
// removes it when there was none.
func (l Layout) restoreLink(link, target string, existed bool) error {
	if !existed {
		err := os.Remove(l.path(link))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return syncDir(l.Root)
	}
	return l.writeLink(link, target)
}

func syncDir(dir string) error {
	f, err := os.Open(dir) // #nosec G304 -- a directory of the install root.
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// UpdateConfig is update.json of the install root.
type UpdateConfig struct {
	Path       string // the file it was read from
	TrustedKey string // PEM file of the trusted public key; relative to the root
	Unit       string // the service unit to restart
	DataDir    string // data directory of the health wait; empty means the default of every command
	// HealthAddress is host:port of the health wait; empty means the address
	// built from config.json of the data directory.
	HealthAddress string
	// HealthTLSCert is the PEM file of the certificate the server presents,
	// for a unit that gives --tls-cert; relative to the root.
	HealthTLSCert string
	// DrainSeconds is the drain time of serve that the unit sets.
	DrainSeconds int
	drainGiven   bool
	members      map[string]json.RawMessage
}

var unitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._-]*\.service$`)

// ValidUnit reports whether name is a plain service unit name.
func ValidUnit(name string) bool { return len(name) <= 255 && unitName.MatchString(name) }

// LoadUpdateConfig reads update.json of root: a JSON object whose keys
// trusted_key, unit and data_dir are strings when present. Unknown keys are
// ignored; a key given twice is refused.
func LoadUpdateConfig(root string) (*UpdateConfig, error) {
	p := filepath.Join(root, ConfigFile)
	f, err := os.Open(p) // #nosec G304 -- update.json of the install root.
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read %s: %v", ErrConfig, p, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfig+1))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read %s: %v", ErrConfig, p, err)
	}
	if len(data) > maxConfig {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrConfig, p, maxConfig)
	}
	values, err := objectOf(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrConfig, p, err)
	}
	cfg := &UpdateConfig{Path: p, Unit: DefaultUnit, members: values}
	for key, dest := range map[string]*string{"trusted_key": &cfg.TrustedKey, "unit": &cfg.Unit, "data_dir": &cfg.DataDir,
		"health_address": &cfg.HealthAddress, "health_tls_cert": &cfg.HealthTLSCert} {
		raw, ok := values[key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, dest); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, fmt.Errorf("%w: %s: %s is not a string", ErrConfig, p, key)
		}
	}
	if err := cfg.readDrain(values); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrConfig, p, err)
	}
	if err := cfg.checkHealthAddress(values); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrConfig, p, err)
	}
	if !ValidUnit(cfg.Unit) {
		return nil, fmt.Errorf("%w: %s: unit %q is not a plain service unit name", ErrConfig, p, cfg.Unit)
	}
	for _, dest := range []*string{&cfg.TrustedKey, &cfg.DataDir, &cfg.HealthTLSCert} {
		if *dest != "" && !filepath.IsAbs(*dest) {
			*dest = filepath.Join(root, *dest)
		}
	}
	return cfg, nil
}

// objectOf reads one JSON object and its members, refusing a member given
// twice and anything after the object.
func objectOf(data []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	values := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if _, ok := values[key]; ok {
			return nil, fmt.Errorf("key %q given twice", key)
		}
		values[key] = raw
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("text after the JSON object")
	}
	return values, nil
}
