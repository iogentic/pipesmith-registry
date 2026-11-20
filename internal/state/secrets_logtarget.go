package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LogTargetSecretPath is the secret file of the log target name:
// secrets/log-target-<name>, every byte of the name outside A-Z, a-z, 0-9,
// '.', '_' and '-' written as %XX, a leading '.' as well.
func LogTargetSecretPath(dataDir, name string) string {
	return filepath.Join(dataDir, SecretsDir, LogTargetSecretName(name))
}

// LogTargetSecretName is the file name of LogTargetSecretPath.
func LogTargetSecretName(name string) string {
	var b strings.Builder
	b.WriteString("log-target-")
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '.' && i == 0:
			fmt.Fprintf(&b, "%%%02X", c)
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// RemoveSecret deletes the secret file at path. A missing file is no error.
func RemoveSecret(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
