package state

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Secret files hold what the server needs and state.sqlite must not: the
// bind password of an LDAP connection (decision P9 of docs/plans/phases.md).
// They lie in the folder secrets/ of the data directory, one file per
// secret. A file is read only while it grants nothing to the group and to
// others and belongs to the user of the process, and its folder belongs to
// that user and is writable by nobody else (hardening line 11 of the phase 3
// plan).

// SecretsDir is the folder of the secret files in a data directory.
const SecretsDir = "secrets"

// MaxSecretBytes bounds a secret file.
const MaxSecretBytes = 4096

// ErrNoSecret is returned by ReadSecret when the file does not exist.
var ErrNoSecret = errors.New("no secret file")

// processUID is the user a secret file must belong to; tests replace it.
var processUID = os.Geteuid

// LDAPSecretPath is the file of the bind password of the LDAP connection id:
// secrets/ldap-<id>, every byte of the id outside A-Z, a-z, 0-9, '.', '_'
// and '-' written as %XX, a leading '.' as well, so that two ids never share
// a file and no id leaves the folder.
func LDAPSecretPath(dataDir, id string) string {
	var b strings.Builder
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c == '.' && i == 0:
			fmt.Fprintf(&b, "%%%02X", c)
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return filepath.Join(dataDir, SecretsDir, "ldap-"+b.String())
}

// checkFolder refuses a secrets folder that is no folder, belongs to another
// user or that the group or others may write.
func checkFolder(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a folder", dir)
	}
	if owner, ok := ownerOf(info); !ok || owner != processUID() {
		return fmt.Errorf("the folder %s belongs to user %d, not to the user %d of this process", dir, owner, processUID())
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("the folder %s has mode %04o, so others may change its files; chmod 700 %s", dir, info.Mode().Perm(), dir)
	}
	return nil
}

// WriteSecret writes value as the secret file at path: the folder with mode
// 0700 when it is new, the file with mode 0600, through a new file in the
// same folder that is renamed into place, so a reader sees the old value or
// the new one. A folder that checkFolder refuses gets no file.
func WriteSecret(path string, value []byte) (err error) {
	if len(value) > MaxSecretBytes {
		return fmt.Errorf("a secret has at most %d bytes", MaxSecretBytes)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := checkFolder(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".new-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name()) // the new file is half written; the error of the write counts
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close() // the chmod error counts
		return err
	}
	if _, err := f.Write(value); err != nil {
		_ = f.Close() // the write error counts
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close() // the sync error counts
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// DeleteSecret removes the secret file at path. A file that does not exist
// is no error. A folder that checkFolder refuses keeps its file.
func DeleteSecret(path string) error {
	dir := filepath.Dir(path)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := checkFolder(dir); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ReadSecret reads the secret file at path: a regular file, no symbolic
// link, that belongs to the user of the process and has no right for the
// group or others, in a folder that checkFolder takes. ErrNoSecret when the
// file does not exist. No error holds a byte of the file.
func ReadSecret(path string) ([]byte, error) {
	f, err := openNoFollow(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoSecret
	}
	if err != nil {
		if isLoop(err) {
			return nil, fmt.Errorf("the secret file %s is not a regular file", path)
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("the secret file %s is not a regular file", path)
	}
	if err := checkFolder(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if owner, ok := ownerOf(info); !ok || owner != processUID() {
		return nil, fmt.Errorf("the secret file %s belongs to user %d, not to the user %d of this process", path, owner, processUID())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("the secret file %s has mode %04o, so others may read it; chmod 600 %s", path, perm, path)
	}
	if info.Size() > MaxSecretBytes {
		return nil, fmt.Errorf("the secret file %s is larger than %d bytes", path, MaxSecretBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxSecretBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSecretBytes {
		return nil, fmt.Errorf("the secret file %s is larger than %d bytes", path, MaxSecretBytes)
	}
	return data, nil
}

// InstallKeyPath is the file of the install key of the imported keys:
// secrets/imported-keys (decision K4 of docs/plans/secrets-import.md).
func InstallKeyPath(dataDir string) string {
	return filepath.Join(dataDir, SecretsDir, "imported-keys")
}

// InstallKeyBytes is the length of the install key.
const InstallKeyBytes = 32

// ReadInstallKey is the install key of a data directory; ErrNoSecret when
// no import made one.
func ReadInstallKey(dataDir string) ([]byte, error) {
	key, err := ReadSecret(InstallKeyPath(dataDir))
	if err != nil {
		return nil, err
	}
	if len(key) != InstallKeyBytes {
		return nil, fmt.Errorf("the install key %s has %d bytes, not %d", InstallKeyPath(dataDir), len(key), InstallKeyBytes)
	}
	return key, nil
}

// MakeInstallKey is the install key of a data directory, made of
// crypto/rand when there is none yet.
func MakeInstallKey(dataDir string) ([]byte, error) {
	key, err := ReadInstallKey(dataDir)
	if !errors.Is(err, ErrNoSecret) {
		return key, err
	}
	key = make([]byte, InstallKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := WriteSecret(InstallKeyPath(dataDir), key); err != nil {
		return nil, err
	}
	return key, nil
}

// ReadTrustFile reads a file this server trusts but need not keep secret,
// such as the CA file of an LDAP connection: a regular file of at most max
// bytes, no symbolic link at the last part of the path, that belongs to the
// user of the process or to root and that neither the group nor others may
// write. Others may read it.
func ReadTrustFile(path string, max int64) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		if isLoop(err) {
			return nil, fmt.Errorf("%s is a symbolic link; name the file it points to", path)
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > max {
		return nil, fmt.Errorf("%s is no regular file of at most %d bytes", path, max)
	}
	if owner, ok := ownerOf(info); !ok || (owner != processUID() && owner != 0) {
		return nil, fmt.Errorf("%s belongs to user %d, neither to the user %d of this process nor to root", path, owner, processUID())
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return nil, fmt.Errorf("%s has mode %04o, so the group or others may write it; chmod go-w %s", path, perm, path)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s is no regular file of at most %d bytes", path, max)
	}
	return data, nil
}
