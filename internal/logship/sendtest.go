package logship

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// testLogger names the line of SendTest.
const testLogger = "pipesmith_registry.log_target"

// SendTest sends one line of server.log with the message "log-target test"
// through the adapter of t, in one attempt bounded by the timeout of t, with
// no manager and no queue. A nil error means the receiver took the line;
// else the error is the class of the failure, as the sender records it.
func SendTest(t Target, secret, version string) error {
	host, _ := os.Hostname() // a machine without a name sends localhost
	if host == "" {
		host = "localhost"
	}
	a, err := openAdapter(t, secret, env{host: host, version: version, pid: os.Getpid(), clock: realClock{}})
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }() // the answer is in; what the adapter held is released
	ctx, cancel := context.WithTimeout(context.Background(), t.timeout())
	defer cancel()
	res, err := a.Send(ctx, []logs.Line{sendTestLine()})
	switch {
	case err != nil:
		return errors.New(classOf(err))
	case res.Rejected > 0:
		return errors.New("rejected")
	case res.Failed > 0:
		return errors.New("failed")
	case len(res.Retry) > 0:
		return errors.New("retry")
	}
	return nil
}

// sendTestLine is the line of SendTest, made by the log files while they are off.
func sendTestLine() logs.Line {
	var line logs.Line
	files := logs.Open(logs.Options{Ship: func(l logs.Line) { line = l }})
	slog.New(files.Handler()).Info("log-target test", slog.String("logger", testLogger))
	_ = files.Close() // the files are off; nothing is open
	return line
}
