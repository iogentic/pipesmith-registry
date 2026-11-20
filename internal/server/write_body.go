package server

import (
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// The body of a write as every write handler reads it (section Hardening of
// docs/plans/phases.md: uploads stream to disk, a read deadline per chunk):
// the file writes of raw and maven2, the npm and Docker writes, and the REST
// upload.

// defaultChunkTimeout is the longest wait for the next bytes of a body.
const defaultChunkTimeout = 30 * time.Second

// chunkTimeout replaces defaultChunkTimeout while it is above zero, in
// nanoseconds; tests shorten the wait with it.
var chunkTimeout atomic.Int64

// defaultBodyTimeout is the longest a whole body may take (N4): a client
// that sends a byte within each chunk timeout holds the request no longer.
// Four hours carry a body of 14 GiB at 1 MiB/s.
const defaultBodyTimeout = 4 * time.Hour

// bodyTimeout replaces defaultBodyTimeout while it is above zero, in
// nanoseconds; tests shorten it.
var bodyTimeout atomic.Int64

func bodyWait() time.Duration {
	if d := time.Duration(bodyTimeout.Load()); d > 0 {
		return d
	}
	return defaultBodyTimeout
}

func chunkWait() time.Duration {
	if d := time.Duration(chunkTimeout.Load()); d > 0 {
		return d
	}
	return defaultChunkTimeout
}

// timedBody is the body of a request as a write reads it: each read waits
// at most chunkWait for its bytes, the whole body at most bodyWait from the
// first read, and the first error of the connection is kept, so that the
// answer tells a client that left or stalled from a failure of the server.
type timedBody struct {
	body io.Reader
	rc   *http.ResponseController
	end  time.Time // the end of bodyWait, zero before the first read
	err  error
}

func newTimedBody(w *recorder, req *request) *timedBody {
	return &timedBody{body: req.r.Body, rc: http.NewResponseController(w.ResponseWriter)}
}

func (b *timedBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	now := time.Now()
	if b.end.IsZero() {
		b.end = now.Add(bodyWait())
	}
	deadline := now.Add(chunkWait())
	if b.end.Before(deadline) {
		deadline = b.end
	}
	if err := b.rc.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		b.err = err
		return 0, err
	}
	n, err := b.body.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.err = err
	}
	return n, err
}

// cut reports a body that did not arrive in full and the status of its
// answer: 408 when the client stalled, 400 when it left or sent less than
// it declared.
func (b *timedBody) cut() (int, bool) {
	if b == nil || b.err == nil {
		return 0, false
	}
	var timeout net.Error
	if errors.As(b.err, &timeout) && timeout.Timeout() {
		return http.StatusRequestTimeout, true
	}
	return http.StatusBadRequest, true
}

// peerAddress is the address of the client without its port, as a blob
// stores it in created-by-ip.
func peerAddress(w *recorder) string {
	if host, _, err := net.SplitHostPort(w.client); err == nil {
		return host
	}
	return w.client
}
