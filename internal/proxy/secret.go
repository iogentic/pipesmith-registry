package proxy

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// The secret half of the sign in of a proxy to its remote (decision 5.13 of
// the phase 5 plan): the password of Basic or the bearer token, in one file
// per repository, secrets/remote/<name> of the data directory, mode 0600,
// below folders of mode 0700. Type and user name stay in the attributes of
// the repository. The command repos remote-secret writes the file; a proxy
// reads it when it makes its client, and again when the file changes.

// MaxSecretBytes is the largest secret a file holds.
const MaxSecretBytes = 64 << 10

// ErrNoSecret is the answer of ReadSecret for a missing or empty file.
var ErrNoSecret = errors.New("no secret file")

// SecretFolder is the folder of the secret files of a data directory.
func SecretFolder(dataDir string) string { return filepath.Join(dataDir, "secrets", "remote") }

// checkSecretName refuses a name that is no single segment of a path.
func checkSecretName(repo string) error {
	if repo == "" || repo == "." || repo == ".." || strings.ContainsAny(repo, "/\\\x00") {
		return fmt.Errorf("%q names no secret file of a repository", repo)
	}
	return nil
}

// resolveSecret resolves the path of the secret file of repo and checks
// that it lies directly in the resolved folder (section Hardening of
// docs/plans/phases.md: a path is resolved first and then checked).
func resolveSecret(folder, repo string) (string, error) {
	root, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, repo))
	if err != nil {
		return "", err
	}
	if filepath.Dir(resolved) != root {
		return "", fmt.Errorf("the secret file of %s lies outside %s", repo, folder)
	}
	return resolved, nil
}

// ReadSecret is the secret of the remote of a proxy, ErrNoSecret without a
// file or for an empty one. Line breaks at its end are taken off.
func ReadSecret(dataDir, repo string) (string, error) {
	secret, _, err := readSecret(dataDir, repo)
	return secret, err
}

func readSecret(dataDir, repo string) (string, fileKey, error) {
	if err := checkSecretName(repo); err != nil {
		return "", fileKey{}, err
	}
	path, err := resolveSecret(SecretFolder(dataDir), repo)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fileKey{}, ErrNoSecret
	}
	if err != nil {
		return "", fileKey{}, err
	}
	f, err := os.Open(path) // #nosec G304 -- the secret file of a proxy, resolved and checked against its folder above.
	if err != nil {
		return "", fileKey{}, err
	}
	defer f.Close()
	var key fileKey
	if info, err := f.Stat(); err == nil {
		key = keyOf(info)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxSecretBytes+1))
	switch {
	case err != nil:
		return "", fileKey{}, err
	case len(data) > MaxSecretBytes:
		return "", fileKey{}, fmt.Errorf("the secret file of %s holds more than %d bytes", repo, MaxSecretBytes)
	}
	secret := strings.TrimRight(string(data), "\r\n")
	if secret == "" {
		return "", key, ErrNoSecret
	}
	return secret, key, nil
}

// WriteSecret writes the secret of the remote of a proxy into its file with
// mode 0600, through a temporary file of the folder and a rename, so a link
// at the place of the file is replaced and never followed. The folders get
// mode 0700. It returns the path of the file. A secret with a control
// character is refused; no error names the secret.
func WriteSecret(dataDir, repo, secret string) (string, error) {
	if err := checkSecretName(repo); err != nil {
		return "", err
	}
	switch {
	case secret == "":
		return "", errors.New("the secret is empty")
	case len(secret) > MaxSecretBytes:
		return "", fmt.Errorf("the secret has more than %d bytes", MaxSecretBytes)
	case strings.ContainsFunc(secret, isControl):
		return "", errors.New("the secret holds a control character")
	}
	folder := SecretFolder(dataDir)
	for _, dir := range []string{filepath.Dir(folder), folder} {
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a folder needs its search bit; 0700 is owner only.
			return "", err
		}
	}
	root, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(root, ".secret-*") // mode 0600
	if err != nil {
		return "", err
	}
	_, err = temp.WriteString(secret)
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	target := filepath.Join(root, repo)
	if err == nil {
		err = os.Rename(temp.Name(), target)
	}
	if err != nil {
		_ = os.Remove(temp.Name()) // the write error is the one returned
		return "", err
	}
	resolved, err := resolveSecret(folder, repo)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// RemoveSecret removes the secret file of the remote of a proxy (ruling
// ST9 of leg 6-stores). A missing file or folder is no error; a link at the
// place of the file is removed, never followed.
func RemoveSecret(dataDir, repo string) error {
	if err := checkSecretName(repo); err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(SecretFolder(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(root, repo)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// fileKey is what changes when a secret file is written again: inode,
// size and modification time; zero for no file.
type fileKey struct {
	ok    bool
	ino   uint64
	size  int64
	mtime int64
}

func keyOf(info fs.FileInfo) fileKey {
	key := fileKey{ok: true, size: info.Size(), mtime: info.ModTime().UnixNano()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		key.ino = uint64(st.Ino)
	}
	return key
}

// secretKey is the key of the secret file of repo now, zero without one.
func secretKey(dataDir, repo string) fileKey {
	if checkSecretName(repo) != nil {
		return fileKey{}
	}
	path, err := resolveSecret(SecretFolder(dataDir), repo)
	if err != nil {
		return fileKey{}
	}
	info, err := os.Stat(path)
	if err != nil {
		return fileKey{}
	}
	return keyOf(info)
}

// secretOf is the secret a client signs in with, and why it cannot sign in:
// a type this server does not build (NTLM, decision 5.12), or no secret
// file. A proxy without a type of sign in needs nothing.
func (p *Proxies) secretOf(name string, auth repos.RemoteAuth) (secret, need string) {
	switch {
	case auth.Type == "":
		return "", ""
	case !auth.Built():
		return "", fmt.Sprintf("the authentication type %s is not built in this server", auth.Type)
	}
	secret, err := ReadSecret(p.o.DataDir, name)
	switch {
	case errors.Is(err, ErrNoSecret):
		return "", fmt.Sprintf("no secret in secrets/remote/%s; write it with repos remote-secret %s", name, name)
	case err != nil:
		return "", fmt.Sprintf("the secret file cannot be read: %v", err)
	}
	return secret, ""
}

// needs is why the remote of the proxy name cannot sign in now, empty when
// it can.
func (p *Proxies) needs(name string, r *remote) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case r.need != "":
		return r.need
	case r.refused:
		return refusedSecret(name)
	}
	return ""
}

func refusedSecret(name string) string {
	return fmt.Sprintf("the remote refused the credentials; write the secret again with repos remote-secret %s", name)
}

// refuse puts a proxy whose credentials the remote answered with 401 into
// the state needs secret, until its secret file or its settings change or
// the server starts again.
func (p *Proxies) refuse(name string, r *remote) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !r.refused {
		p.warn("proxy %s needs its secret and asks no remote: %s", name, refusedSecret(name))
	}
	r.refused = true
}
