package server

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// logLines logs a message whose format holds line breaks of its own, as a
// traceback head: standard error writes the format with its values escaped
// (logs.Lines), server.log the message as logAt writes it.
func logLines(level slog.Level, logger, format string, args ...any) {
	slog.Default().Log(context.Background(), level, fmt.Sprintf(format, args...), "logger", logger, logs.Lines(format, args...))
}
