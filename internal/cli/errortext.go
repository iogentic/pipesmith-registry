package cli

import (
	"errors"
	"regexp"
	"strings"

	"modernc.org/sqlite"

	"github.com/mirosso25/pipesmith-registry/internal/server"
)

// sqliteCode is the result code the driver appends to a message.
var sqliteCode = regexp.MustCompile(`^(.*) \(\d+\)( \(SQLITE_BUSY\))?$`)

// errorText is str() of the Python exception behind err. For a SQLite error
// that is sqlite3_errmsg, as the sqlite3 module of Python reports it: the
// driver writes "<errstr> (<code>)" or "<errstr>: <errmsg> (<code>)", and no
// errstr of SQLite holds ": ".
func errorText(err error) string {
	text := server.PyText(err)
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return text
	}
	match := sqliteCode.FindStringSubmatch(e.Error())
	if match == nil {
		return text
	}
	message := match[1]
	if _, errmsg, ok := strings.Cut(message, ": "); ok {
		message = errmsg
	}
	return strings.Replace(text, e.Error(), message, 1)
}
