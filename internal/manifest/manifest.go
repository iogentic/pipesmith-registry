// Package manifest reads the blob manifest of a file blob store as metadata
// source for a directory store.
//
// A manifest is a file or a directory of part-*.tsv, part-*.tsv.gz or
// part-*.tsv.zst files. Each line describes one blob in nine tab separated
// columns: store, blob_rel, repo, path, size, created_ms, deleted, sha1,
// content_type. Backslash, tab, newline and carriage return inside a value
// are written as \\, \t, \n and \r. A .tsv.zst part is read through the
// zstd program.
//
// Lookup copies the live rows into a SQLite table below a cache directory
// once per state of the manifest, in the file name and format of the Python
// server, so one data directory works with both. Nothing here writes next to
// the manifest.
package manifest

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/pystr"
)

// Layout of a manifest.
const (
	Columns    = 9
	PartPrefix = "part-"
	// LookupVersion is part of the state of a lookup table and its meta row.
	LookupVersion = 1
	// ReadBuffer is the buffer of a plain part and of the zstd output.
	ReadBuffer = 1 << 20
)

// PartSuffixes are the names a part file of a manifest directory ends with.
var PartSuffixes = []string{".tsv", ".tsv.gz", ".tsv.zst"}

const loggerName = "pipesmith_registry.manifest"

// Row is one live row of a manifest.
type Row struct {
	Repo, Path        string
	Size              int64
	SHA1, ContentType *string // nil where the Python server has None
	CreatedMS         int64
	// Overflow is true when size or created_ms has more digits than an
	// int64 holds. Python reads such a row, and its lookup fails when it
	// stores the row; the field that overflowed is 0.
	Overflow bool
}

// Error is an exception of the Python module, named by Class:
// FileNotFoundError, OSError for its subclasses, ValueError, OverflowError,
// UnicodeEncodeError, or "error" for zlib.error. A FileNotFoundError
// matches fs.ErrNotExist.
type Error struct {
	Class, Message string
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Is(target error) bool {
	return e.Class == "FileNotFoundError" && target == fs.ErrNotExist
}

func newError(class, format string, args ...any) *Error {
	return &Error{Class: class, Message: fmt.Sprintf(format, args...)}
}

type part struct {
	path, name string
}

// Parts returns the files of a manifest: the file itself, or the part
// files of a directory in name order.
func Parts(source string) ([]string, error) {
	found, err := parts(source)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(found))
	for i, p := range found {
		paths[i] = p.path
	}
	return paths, nil
}

func parts(source string) ([]part, error) {
	source = pystr.Path(source)
	ok, err := isDir(source)
	if err != nil {
		return nil, err
	}
	if ok {
		names, err := listDir(source)
		if err != nil {
			return nil, err
		}
		var found []part
		for _, name := range names {
			if !strings.HasPrefix(name, PartPrefix) || !slices.ContainsFunc(PartSuffixes, func(suffix string) bool {
				return strings.HasSuffix(name, suffix)
			}) {
				continue
			}
			path := pystr.Join(source, name)
			ok, err := isFile(path)
			if err != nil {
				return nil, err
			}
			if ok {
				found = append(found, part{path: path, name: name})
			}
		}
		slices.SortFunc(found, func(a, b part) int { return pystr.Compare(fsDecode(a.name), fsDecode(b.name)) })
		return found, nil
	}
	ok, err = isFile(source)
	if err != nil {
		return nil, err
	}
	if ok {
		return []part{{path: source, name: baseName(source)}}, nil
	}
	return nil, newError("FileNotFoundError", "manifest %s does not exist", source)
}

func listDir(path string) ([]string, error) {
	f, err := os.Open(path) // #nosec G304 -- the manifest directory the operator configured, or the lookup cache of the data directory.
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// ReadRows calls yield with the live rows of a manifest, part by part and
// line by line. Deleted rows and empty lines are skipped. A malformed line
// is skipped and counted, and the count is logged per part. An error of
// yield, or the ValueError of a line, ends the read and is returned.
func ReadRows(source string, yield func(Row) error) error {
	found, err := parts(source)
	if err != nil {
		return err
	}
	for _, p := range found {
		malformed := 0
		err := readLines(p, func(line []byte) error {
			row, bad, err := parseLine(line)
			switch {
			case err != nil:
				return err
			case row != nil:
				return yield(*row)
			case bad:
				malformed++
			}
			return nil
		})
		if err != nil {
			return err
		}
		if malformed > 0 {
			slog.Warn(fmt.Sprintf("manifest part %s: %d malformed lines skipped", p.path, malformed), "logger", loggerName)
		}
	}
	return nil
}

// readLines calls fn with every line of one part, decompressed by the
// suffix of its name. A line keeps its newline, as a Python line does.
func readLines(p part, fn func([]byte) error) error {
	switch {
	case strings.HasSuffix(p.name, ".zst"):
		return zstdLines(p.path, fn)
	case strings.HasSuffix(p.name, ".gz"):
		f, err := os.Open(p.path)
		if err != nil {
			return err
		}
		defer f.Close()
		return eachLine(bufio.NewReaderSize(newGzipReader(f, p.path), 64*1024), fn)
	}
	f, err := os.Open(p.path)
	if err != nil {
		return err
	}
	defer f.Close()
	return eachLine(bufio.NewReaderSize(f, ReadBuffer), fn)
}

// eachLine splits at b"\n" as iterating a binary file in Python does: the
// last line may lack it, and a line cut by a read error is not passed on.
func eachLine(r *bufio.Reader, fn func([]byte) error) error {
	var long []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			long = append(long, chunk...)
			continue
		}
		line := chunk
		if long != nil {
			line = append(long, chunk...)
			long = nil
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			if len(line) > 0 {
				return fn(line)
			}
			return nil
		default:
			return err
		}
		if err := fn(line); err != nil {
			return err
		}
	}
}

// zstdLines reads `zstd -dc -- <part>` line by line. The standard error of
// the program goes to the standard error of this process.
func zstdLines(path string, fn func([]byte) error) error {
	cmd := exec.Command("zstd", "-dc", "--", path) // #nosec G204 -- a fixed program with an argument list and no shell; the part of the operator's manifest comes after "--".
	if errors.Is(cmd.Err, exec.ErrDot) {
		cmd.Err = nil // Python's Popen runs a zstd found through "." in PATH
	}
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return newError("FileNotFoundError", "the zstd program is needed to read %s; install zstd", path)
		}
		return err
	}
	if err := eachLine(bufio.NewReaderSize(stdout, ReadBuffer), fn); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			code = -int(status.Signal()) // Python's returncode for a signal
		}
		return newError("OSError", "zstd -dc %s ended with code %d", path, code)
	}
	return err
}

var escapes = map[byte]byte{'\\': '\\', 't': '\t', 'n': '\n', 'r': '\r'}

// parseLine is the row of one line, or nil; bad is true for a malformed
// line. The error is the ValueError of int() for a number past 4300 digits.
func parseLine(line []byte) (*Row, bool, error) {
	line = trimNewlines(line)
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if len(line) == 0 {
		return nil, false, nil
	}
	fields := strings.Split(string(line), "\t")
	if len(fields) != Columns {
		return nil, true, nil
	}
	for _, field := range fields {
		if !utf8.ValidString(field) {
			return nil, true, nil
		}
	}
	repo, path, sizeText, createdText, deleted, sha1, contentType :=
		fields[2], fields[3], fields[4], fields[5], fields[6], fields[7], fields[8]
	if !digits(sizeText) || !digits(createdText) {
		return nil, true, nil
	}
	if !live(deleted) {
		return nil, false, nil
	}
	repo, path = unescape(repo), unescape(path)
	path = strings.TrimPrefix(path, "/")
	if repo == "" || path == "" {
		return nil, true, nil
	}
	// int() of Python reads at most pystr.MaxIntDigits digits
	for _, text := range []string{sizeText, createdText} {
		if len(text) > pystr.MaxIntDigits {
			_, err := pystr.ParseInt(text)
			return nil, false, newError("ValueError", "%s", err.Error())
		}
	}
	row := &Row{Repo: repo, Path: path}
	var err error
	if row.Size, err = strconv.ParseInt(sizeText, 10, 64); err != nil {
		row.Size, row.Overflow = 0, true
	}
	if row.CreatedMS, err = strconv.ParseInt(createdText, 10, 64); err != nil {
		row.CreatedMS, row.Overflow = 0, true
	}
	if sum := asciiLower(pystr.Strip(sha1)); len(sum) == 40 && isHex(sum) {
		row.SHA1 = &sum
	}
	if contentType = unescape(contentType); contentType != "" {
		row.ContentType = &contentType
	}
	return row, false, nil
}

// trimNewlines is bytes.rstrip(b"\n").
func trimNewlines(line []byte) []byte {
	for len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	return line
}

// digits is str.isascii() and str.isdigit(): one or more of 0 to 9.
func digits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// live is deleted.strip().lower() in ("", "0", "false"). No character
// outside ASCII lowers to one of these letters in Python, so lowering ASCII
// is enough; strings.EqualFold would also take the long s.
func live(deleted string) bool {
	switch asciiLower(pystr.Strip(deleted)) {
	case "", "0", "false":
		return true
	}
	return false
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}

// unescape turns \\, \t, \n and \r into their characters; any other
// backslash stays.
func unescape(text string) string {
	if !strings.Contains(text, `\`) {
		return text
	}
	out := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) {
			if c, ok := escapes[text[i+1]]; ok {
				out = append(out, c)
				i++
				continue
			}
		}
		out = append(out, text[i])
	}
	return string(out)
}
