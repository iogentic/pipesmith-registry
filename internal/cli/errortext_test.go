package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// A data directory whose index.sqlite is not a database: Python leaves the
// sqlite3.DatabaseError uncaught, and str() of it is the message of SQLite
// alone, "file is not a database", without the result code the Go driver
// appends (decision 10: 'error: <text>' with exit code 1).
func TestBrokenIndexPrintsTheSQLiteMessage(t *testing.T) {
	dir := dataDir(t)
	tree := demoTree(t, filepath.Dir(dir))
	if code, _, errOut := run(t, "store", "add", "tree", "--kind", "directory", "--root", tree); code != 0 {
		t.Fatalf("store add: %d %q", code, errOut)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.sqlite"), []byte(strings.Repeat("this is not a database\n", 200)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"store", "list"}, {"repos"}, {"scan"}, {"docker-check", "demo-docker"}, {"store", "remove", "tree"}} {
		code, out, errOut := run(t, args...)
		if code != 1 || out != "" || errOut != "error: file is not a database\n" {
			t.Errorf("%v: %d %q %q", args, code, out, errOut)
		}
	}
}

// str() of a sqlite3 error is sqlite3_errmsg: the driver's "<errstr>: <errmsg> (<code>)"
// prints as <errmsg>.
func TestSQLiteErrorText(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec("SELECT * FROM nope")
	if err == nil {
		t.Fatal("no error")
	}
	if got := errorText(err); got != "no such table: nope" {
		t.Fatalf("errorText(%q) = %q", err, got)
	}
	if got := errorText(os.ErrClosed); got != os.ErrClosed.Error() {
		t.Fatalf("errorText of a non SQLite error = %q", got)
	}
}
