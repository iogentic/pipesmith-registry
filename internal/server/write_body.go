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

func chunkWait() time.Duration {
	if d := time.Duration(chunkTimeout.Load()); d > 0 {
		return d
	}
	return defaultChunkTimeout
}

// timedBody is the body of a request as a write reads it: each read waits
// at most chunkWait for its bytes, and the first error of the connection is
// kept, so that the answer tells a client that left or stalled from a
// failure of the server.
type timedBody struct {
	body io.Reader
	rc   *http.ResponseController
	err  error
}

func newTimedBody(w *recorder, req *request) *timedBody {
	return &timedBody{body: req.r.Body, rc: http.NewResponseController(w.ResponseWriter)}
}

func (b *timedBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if err := b.rc.SetReadDeadline(time.Now().Add(chunkWait())); err != nil && !errors.Is(err, http.ErrNotSupported) {
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
