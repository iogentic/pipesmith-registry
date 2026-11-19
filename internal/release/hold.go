package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// Hold is an install root under the owner rule and the lock of Update and
// Rollback, with update.json and the release current points to.
type Hold struct {
	Layout  Layout
	Config  *UpdateConfig
	Current string // release name current points to
	unlock  func()
}

// HoldRoot checks that this process runs as the owner of root and the
// owners and modes of the root, releases/, update.json and the trusted key,
// takes the lock without waiting, reads update.json and the link current.
// It changes nothing. Release ends the hold.
func HoldRoot(root string) (*Hold, error) {
	return holdRoot(root, os.Geteuid, fileOwner, lstatFile, func(l Layout) (func(), error) { return lockRoot(l.Root) })
}

// ErrFetching refuses a fetch stage while another one runs.
var ErrFetching = errors.New("another fetch stage runs")

// HoldReleases is HoldRoot with the lock of the fetch stage: an exclusive
// lock on releases/ of root without waiting, and no lock on the root, so
// update and rollback run while a fetch waits on the forge.
func HoldReleases(root string) (*Hold, error) {
	return holdRoot(root, os.Geteuid, fileOwner, lstatFile, func(l Layout) (func(), error) {
		return lockDir(l.Releases(), ErrFetching)
	})
}

func holdRoot(root string, euid func() int, ownerOf func(string) (int, error), statOf func(string) (fileStat, error),
	lock func(Layout) (func(), error)) (*Hold, error) {
	l := Layout{Root: root}
	owner, err := ownerRule(l.Root, euid, ownerOf)
	if err != nil {
		return nil, err
	}
	if err := rootRule(l.Root, owner, statOf); err != nil {
		return nil, err
	}
	unlock, err := lock(l)
	if err != nil {
		return nil, err
	}
	h := &Hold{Layout: l, unlock: unlock}
	if h.Config, err = LoadUpdateConfig(l.Root); err == nil {
		err = keyRule(h.Config.TrustedKey, owner, statOf)
	}
	if err != nil {
		unlock()
		return nil, err
	}
	h.Current, err = l.ReadLink(CurrentLink)
	if errors.Is(err, fs.ErrNotExist) {
		err = fmt.Errorf("%w: %s does not exist; the first release is installed by hand", ErrLayout, l.path(CurrentLink))
	}
	if err != nil {
		unlock()
		return nil, err
	}
	return h, nil
}

// Release ends the hold.
func (h *Hold) Release() { h.unlock() }

// ownerRule refuses a process that does not run as the owner of root, and
// returns that owner.
func ownerRule(root string, euid func() int, ownerOf func(string) (int, error)) (int, error) {
	owner, err := ownerOf(root)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrOwner, err)
	}
	if me := euid(); me < 0 || owner != me {
		return 0, fmt.Errorf("%w: %s belongs to uid %d and this command runs as uid %d; run it as the owner", ErrOwner, root, owner, me)
	}
	return owner, nil
}

// RunVersion runs `<bin> version` as Update does, without a shell, with an
// empty environment and no standard input, and reads the version from its
// answer. A timeout of zero takes DefaultVersionTimeout.
func RunVersion(bin string, timeout time.Duration) (Version, error) {
	u := &Updater{VersionTimeout: timeout}
	return u.runVersion(bin)
}

// Member returns the raw value of a key of update.json, for the keys of
// other readers of the file.
func (c *UpdateConfig) Member(key string) (json.RawMessage, bool) {
	raw, ok := c.members[key]
	return raw, ok
}
