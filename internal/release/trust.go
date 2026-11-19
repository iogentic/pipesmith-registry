package release

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// ErrPermissions refuses a file of the install root that another user than
// its owner could change.
var ErrPermissions = errors.New("owner or mode")

// fileStat is the owner and the mode of a file.
type fileStat struct {
	uid  int
	mode fs.FileMode
}

// rootRule refuses a root or releases/ that is not a directory of owner, or
// that group or others can write, and an update.json that is no regular file
// of owner or uid 0, or that group or others can write. A missing releases/
// or update.json is left to the steps that read them.
func rootRule(root string, owner int, statOf func(string) (fileStat, error)) error {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPermissions, err)
	}
	for _, p := range []string{real, filepath.Join(real, ReleasesDir), filepath.Join(real, ConfigFile)} {
		st, err := statOf(p)
		if errors.Is(err, fs.ErrNotExist) && p != real {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrPermissions, err)
		}
		isConfig := filepath.Base(p) == ConfigFile && p != real
		switch {
		case isConfig && !st.mode.IsRegular():
			return fmt.Errorf("%w: %s is no regular file (uid %d, mode %v); make it a plain file of uid %d", ErrPermissions, p, st.uid, st.mode, owner)
		case !isConfig && !st.mode.IsDir():
			return fmt.Errorf("%w: %s is no directory (uid %d, mode %v)", ErrPermissions, p, st.uid, st.mode)
		}
		if err := ownedSafe(p, st, owner, isConfig, false); err != nil {
			return err
		}
	}
	return nil
}

// keyRule refuses a trusted key that is, after its symlinks, no regular file
// of owner or uid 0 or that group or others can write, and a key in a
// directory of another uid or that group or others can write without the
// sticky bit.
func keyRule(key string, owner int, statOf func(string) (fileStat, error)) error {
	if key == "" {
		return nil
	}
	real, err := filepath.EvalSymlinks(key)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTrustedKey, err)
	}
	st, err := statOf(real)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPermissions, err)
	}
	if !st.mode.IsRegular() {
		return fmt.Errorf("%w: the trusted key %s is no regular file (uid %d, mode %v)", ErrPermissions, real, st.uid, st.mode)
	}
	if err := ownedSafe(real, st, owner, true, false); err != nil {
		return err
	}
	dir := filepath.Dir(real)
	if st, err = statOf(dir); err != nil {
		return fmt.Errorf("%w: %v", ErrPermissions, err)
	}
	return ownedSafe(dir, st, owner, true, true)
}

// ownedSafe refuses a file of another uid than owner (or uid 0 when rootToo)
// and a file that group or others can write (allowed for a directory with
// the sticky bit when sticky).
func ownedSafe(p string, st fileStat, owner int, rootToo, sticky bool) error {
	uidOK := st.uid == owner || (rootToo && st.uid == 0)
	modeOK := st.mode.Perm()&0o022 == 0 || (sticky && st.mode&fs.ModeSticky != 0)
	if uidOK && modeOK {
		return nil
	}
	who := fmt.Sprintf("uid %d, the owner of the root", owner)
	if rootToo && owner != 0 {
		who += ", or uid 0"
	}
	var fix []string
	if !uidOK {
		fix = append(fix, fmt.Sprintf("chown %d %s", owner, p))
	}
	if !modeOK {
		fix = append(fix, "chmod go-w "+p)
	}
	return fmt.Errorf("%w: %s belongs to uid %d and has mode %v; it must belong to %s, and neither group nor others may write it; %s",
		ErrPermissions, p, st.uid, st.mode, who, strings.Join(fix, "; "))
}
