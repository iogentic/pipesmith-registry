package cli

import (
	"log/slog"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// stderrMessage is the message of a record for standard error (decision
// P7-24): the text of its logs.Lines attribute, else the message through
// logs.LineText.
func stderrMessage(r slog.Record) string {
	text, found := "", false
	r.Attrs(func(a slog.Attr) bool {
		text, found = logs.LinesText(a)
		return !found
	})
	if found {
		return text
	}
	return logs.LineText(r.Message)
}
