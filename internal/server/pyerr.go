package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"syscall"

	"github.com/mirosso25/pipesmith-registry/internal/blobprops"
	"github.com/mirosso25/pipesmith-registry/internal/docker"
	"github.com/mirosso25/pipesmith-registry/internal/listeners"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/pystr"
	"github.com/mirosso25/pipesmith-registry/internal/stores"
)

// Logger names, as the Python modules name theirs.
const (
	serverLogger    = "pipesmith_registry.server"
	listenersLogger = "pipesmith_registry.listeners"
)

func logAt(level slog.Level, logger, format string, args ...any) {
	slog.Default().Log(context.Background(), level, fmt.Sprintf(format, args...), "logger", logger)
}

// PyClass names the Python exception class an error of the ported packages
// stands for: "OSError", "FileNotFoundError", "ValueError",
// "JSONDecodeError", "UnicodeDecodeError", "OverflowError", "RuntimeError",
// "RecursionError", ..., and "Exception" for an error Python has no class for.
func PyClass(err error) string {
	var syntax *pyjson.SyntaxError
	var pyErr *pyjson.Error
	var digits *blobprops.ValueError
	var storeErr *stores.Error
	var notFound *stores.NotFoundError
	var pathErr *docker.PathError
	var listenErr *listeners.ListenError
	var fsErr *fs.PathError
	var linkErr *os.LinkError
	var sysErr *os.SyscallError
	var errno syscall.Errno
	switch {
	case err == nil:
		return ""
	case errors.As(err, &syntax):
		return "JSONDecodeError"
	case errors.As(err, &pathErr), errors.As(err, &digits):
		return "ValueError"
	case errors.As(err, &pyErr):
		return pyErr.Class
	case errors.As(err, &storeErr):
		return storeErr.Class
	case errors.As(err, &notFound):
		return "FileNotFoundError"
	case errors.As(err, &listenErr), errors.Is(err, stores.ErrNoManifestLookup):
		return "OSError"
	case errors.As(err, &fsErr), errors.As(err, &linkErr), errors.As(err, &sysErr), errors.As(err, &errno):
		if errors.Is(err, fs.ErrNotExist) {
			return "FileNotFoundError"
		}
		return "OSError"
	case isOverflow(err):
		return "OverflowError"
	}
	return "Exception"
}

// Caught reports whether Python catches the error with
// `except (OSError, ValueError)`.
func Caught(err error) bool {
	switch PyClass(err) {
	case "OSError", "FileNotFoundError", "ValueError", "JSONDecodeError", "UnicodeDecodeError", "UnicodeEncodeError":
		return true
	}
	return false
}

// PyText is str() of the Python exception: an OS error of a path reads
// "[Errno 2] No such file or directory: '/x'".
func PyText(err error) string {
	if pathErr, ok := err.(*fs.PathError); ok {
		var errno syscall.Errno
		if errors.As(pathErr.Err, &errno) {
			return fmt.Sprintf("[Errno %d] %s: %s", int(errno), strerror(errno), pystr.Repr(pathErr.Path))
		}
	}
	return err.Error()
}

// strerror is the C text of an errno, which starts with a capital letter.
func strerror(errno syscall.Errno) string {
	text := errno.Error()
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// strerrorOf is `exc.strerror or exc` of an OSError.
func strerrorOf(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return strerror(errno)
	}
	return PyText(err)
}
