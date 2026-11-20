package logship

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// An Adapter sends batches of lines to one target in the protocol of its
// kind. One sender goroutine calls it, never two calls at a time.
type Adapter interface {
	// Send makes one attempt to deliver batch and returns once ctx ends at
	// the latest. A nil error means the receiver took the batch, apart from
	// what Result names. An error says what failed, as a *Failure.
	Send(ctx context.Context, batch []logs.Line) (Result, error)
	// Close releases the connections of the adapter; the sender calls it
	// once, after the last Send.
	Close() error
}

// Result is what became of the lines of one attempt.
type Result struct {
	Rejected int         // lines the receiver refused inside an accepted answer; not sent again
	Failed   int         // lines that can never be sent, such as a message above the limit of the protocol
	Cut      int         // lines sent shortened, as syslog over UDP does
	Retry    []logs.Line // lines to send again in the next attempt; with an error, nil or empty stands for the whole batch
}

// Failure is the error of a failed attempt.
type Failure struct {
	Class      string        // what failed: dial, timeout, tls, network, write, status 503; never an address, a secret or a line
	Final      bool          // the lines are dropped at once; else they are sent again
	RetryAfter time.Duration // the pause the receiver asked for, 0 for none
}

func (f *Failure) Error() string { return f.Class }

// classOf is the class of the failure of an attempt.
func classOf(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Class
	}
	return "error"
}

// final reports whether err drops the lines at once.
func final(err error) bool {
	var f *Failure
	return errors.As(err, &f) && f.Final
}

// env is what an adapter gets from the manager besides its target.
type env struct {
	host    string // the host name of the machine
	version string // the version of the build, for User-Agent
	pid     int    // the process id
	clock   clock
}

// opener makes the adapter of one target. It reads files, such as ca_file,
// but does not touch the network: the first Send connects.
type opener func(t Target, secret string, e env) (Adapter, error)

// openers holds the adapter of each kind; each adapter file adds its own
// from init.
var openers = map[string]opener{}

func register(kind string, open opener) { openers[kind] = open }

// openAdapter makes the adapter of t.
func openAdapter(t Target, secret string, e env) (Adapter, error) {
	open := openers[t.Kind]
	if open == nil {
		return nil, fmt.Errorf("kind %s has no adapter in this build", t.Kind)
	}
	return open(t, secret, e)
}
