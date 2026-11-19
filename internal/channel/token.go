package channel

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// maxToken bounds the size of a token file.
const maxToken = 4096

// token reads the token of the channel from <data directory>/secrets/<name>,
// where secrets/ is a directory and no symlink: a regular file that neither group nor others can read, its content
// without the final newline, printable ASCII without spaces. No message
// holds the token.
func (f *Fetcher) token(cfg *release.UpdateConfig, set *Settings) (string, error) {
	if set.TokenFile == "" {
		return "", nil
	}
	dataDir := cfg.DataDir
	if f.DataDir != nil {
		var err error
		if dataDir, err = f.DataDir(cfg); err != nil {
			return "", fmt.Errorf("%w: data directory: %v", ErrToken, err)
		}
	}
	if dataDir == "" {
		return "", fmt.Errorf("%w: no data directory for secrets/%s; set data_dir in %s", ErrToken, set.TokenFile, cfg.Path)
	}
	dir := filepath.Join(dataDir, "secrets")
	path := filepath.Join(dir, set.TokenFile)
	if info, err := os.Lstat(dir); err == nil && !info.IsDir() {
		return "", fmt.Errorf("%w: %s is a symlink or no directory (mode %v); secrets/ must be a directory of the data directory", ErrToken, dir, info.Mode())
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrToken, path, err)
	}
	defer root.Close()
	info, err := root.Lstat(set.TokenFile)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrToken, path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s is not a regular file", ErrToken, path)
	}
	if info.Mode().Perm()&0o044 != 0 {
		return "", fmt.Errorf("%w: group or others can read %s (mode %v); allow the owner only, chmod 600", ErrToken, path, info.Mode().Perm())
	}
	file, err := root.Open(set.TokenFile)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrToken, path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxToken+1))
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrToken, path, err)
	}
	if len(data) > maxToken {
		return "", fmt.Errorf("%w: %s is larger than %d bytes", ErrToken, path, maxToken)
	}
	data = bytes.TrimSuffix(data, []byte("\n"))
	if len(data) == 0 {
		return "", fmt.Errorf("%w: %s is empty", ErrToken, path)
	}
	for _, b := range data {
		if b < 0x21 || b > 0x7e {
			return "", fmt.Errorf("%w: %s holds characters other than printable ASCII without spaces", ErrToken, path)
		}
	}
	return string(data), nil
}
