package logs

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"
)

// TestOpenFileFindsNoFileForAPlainName: a plain name that is no log file is a
// file this server does not have (the 404 of docs/ui/API.md, line 49); only
// a name with "..", a slash, a backslash or a control byte is refused as a
// name (line 551).
func TestOpenFileFindsNoFileForAPlainName(t *testing.T) {
	dir := t.TempDir()
	putFile(t, filepath.Join(dir, "notes.txt"), []byte("notes"), time.Now())
	putFile(t, filepath.Join(dir, "logs.lock"), nil, time.Now())
	for _, name := range []string{"notes.txt", "logs.lock", "demo-walk-unknown", "other.log", "server.log.gz"} {
		f, err := OpenFile(dir, name)
		if f != nil {
			f.Close()
		}
		if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrName) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"", ".", "..", "../server.log", "a..log", "/server.log", "sub/server.log",
		`..\server.log`, `sub\server.log`, "server.log\x00", "server\n.log", "server\x7f.log"} {
		f, err := OpenFile(dir, name)
		if f != nil {
			f.Close()
		}
		if !errors.Is(err, ErrName) {
			t.Errorf("%q: %v", name, err)
		}
	}
}
