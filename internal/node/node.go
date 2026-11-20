// Package node keeps the id of this server: a random UUID written at the
// first start into the file node-id of the data directory (rule R9 of the
// phase 1c plan) and copied into the row node_id of state.settings when the
// data directory has state.sqlite (rule I20 of the phase 2 plan). Once the
// row exists it is the source; the file is written again from the row when
// it is missing.
package node

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/state"
)

const (
	// FileName is the file of the id in the data directory.
	FileName = "node-id"
	// SettingKey is the row of the id in state.settings.
	SettingKey = "node_id"
)

// ID is the node id of dataDir. A file or row that holds no lower case
// UUID is refused and left as it is.
func ID(dataDir string) (string, error) {
	st, err := state.Open(dataDir)
	switch {
	case errors.Is(err, state.ErrAbsent):
		st = nil
	case err != nil:
		return "", err
	default:
		defer st.Close()
	}
	path := filepath.Join(dataDir, FileName)
	if st != nil {
		id, found, err := fromRow(st)
		if err != nil {
			return "", err
		}
		if found {
			return id, restore(path, id)
		}
	}
	id, err := fromFile(path)
	if err != nil {
		return "", err
	}
	if id == "" {
		if id, err = writeNew(path, newUUID()); err != nil {
			return "", err
		}
	}
	if st != nil {
		if err := st.PutSetting(SettingKey, id); err != nil {
			return "", fmt.Errorf("copy the node id into state.sqlite: %w", err)
		}
	}
	return id, nil
}

// fromRow reads the row node_id of state.settings.
func fromRow(st *state.Store) (string, bool, error) {
	value, found, err := st.Setting(SettingKey)
	if err != nil || !found {
		return "", false, err
	}
	var id string
	if err := json.Unmarshal(value, &id); err != nil || !valid(id) {
		return "", false, fmt.Errorf("the row %s of state.settings holds no node id (a lower case UUID)", SettingKey)
	}
	return id, true, nil
}

// fromFile reads the file node-id; a missing file gives "".
func fromFile(path string) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the file node-id of the data directory the operator configured.
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if !valid(id) {
		return "", fmt.Errorf("%s holds no node id (a lower case UUID); correct it or remove it to get a new id", path)
	}
	return id, nil
}

// restore writes the id of the row into the file when the file is missing.
// An existing file is not read: the row is the source (I20).
func restore(path, id string) error {
	_, err := os.Lstat(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	_, err = writeNew(path, id)
	return err
}

// writeNew writes id into a new file at path with mode 0600. When another
// start wrote the file first, its id is the one returned.
func writeNew(path, id string) (string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the file node-id of the data directory the operator configured.
	if errors.Is(err, fs.ErrExist) {
		if stored, err := fromFile(path); err != nil || stored != "" {
			return stored, err
		}
		return id, nil
	}
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(id + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path) // cleanup of the partial file; the write error is returned
		return "", err
	}
	return id, nil
}

// newUUID is a random lower case UUID of version 4.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read does not fail
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// valid reports whether id is a lower case UUID of any version.
func valid(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}
