package release

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// checkBefore checks releases/<prev>/ before rollback runs its binary as the
// owner of the root: the directory, pipesmith-registry and SHA256SUMS belong
// to the owner or uid 0 and neither group nor others may write them, and the
// binary has the SHA-256 that SHA256SUMS lists for the platform. A directory
// without SHA256SUMS, as a first release installed by hand, is not compared.
func (u *Updater) checkBefore(s *session, prev string) error {
	dir := s.l.Release(prev)
	bin := filepath.Join(dir, InstalledBinary)
	sums := filepath.Join(dir, SumsFile)
	if err := u.trustRelease(s, prev); err != nil {
		return fmt.Errorf("%w; rollback changed nothing", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("%w: %v; rollback changed nothing", ErrNoBinary, err)
	}
	defer root.Close()
	data, err := readLimited(root, SumsFile)
	if errors.Is(err, fs.ErrNotExist) {
		u.step("checked: %s/%s holds no %s; the binary is not compared", ReleasesDir, prev, SumsFile)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %v; rollback changed nothing", ErrSums, err)
	}
	lines, err := ParseSums(data)
	if err != nil {
		return fmt.Errorf("%w; rollback changed nothing", err)
	}
	name := BinaryName(runtime.GOOS, runtime.GOARCH)
	rel := &Release{Dir: dir, Name: name}
	for _, l := range lines {
		if l.Name == name {
			rel.SHA256 = l.SHA256
		}
	}
	if rel.SHA256 == "" {
		return fmt.Errorf("%w: %s of %s/%s has no line for %s; rollback changed nothing", ErrNotListed, SumsFile, ReleasesDir, prev, name)
	}
	if err := rel.VerifyFile(bin); err != nil {
		return fmt.Errorf("%w; %s lists it for %s; rollback changed nothing", err, sums, name)
	}
	u.step("checked: %s/%s/%s has the SHA-256 that %s lists for %s", ReleasesDir, prev, InstalledBinary, SumsFile, name)
	return nil
}

// trustRelease is the owner and mode check of releases/<name>/ before a run
// of its binary as the owner of the root: the directory, pipesmith-registry
// and, when it exists, SHA256SUMS belong to the owner or uid 0, and neither
// group nor others may write them (findings A-2, R3-3).
func (u *Updater) trustRelease(s *session, name string) error {
	dir := s.l.Release(name)
	bin := filepath.Join(dir, InstalledBinary)
	sums := filepath.Join(dir, SumsFile)
	for _, p := range []string{dir, bin, sums} {
		st, err := s.statOf(p)
		if p == sums && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrPermissions, err)
		}
		if p == dir && !st.mode.IsDir() {
			return fmt.Errorf("%w: %s is no directory (uid %d, mode %v)", ErrPermissions, p, st.uid, st.mode)
		}
		if p != dir && !st.mode.IsRegular() {
			return fmt.Errorf("%w: %s is no regular file (uid %d, mode %v)", ErrPermissions, p, st.uid, st.mode)
		}
		if err := ownedSafe(p, st, s.owner, true, false); err != nil {
			return err
		}
	}
	return nil
}
