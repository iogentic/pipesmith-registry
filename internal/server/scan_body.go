package server

import (
	"errors"
	"io"
	"net/http"
	"strconv"
)

// maxScanBody is the largest body POST /api/scan reads; {"store": <name>}
// needs a few bytes.
const maxScanBody = 64 << 10

// maxScanDrain is how much of a larger body is read and dropped before the
// 413 answer, so that a client that sends it whole reads the answer and
// not a reset connection. Past it the connection closes.
const maxScanDrain = 16 << 20

var scanBodyTooLarge = "the body is larger than " + strconv.Itoa(maxScanBody) + " bytes"

// tooLarge is the error of an http.MaxBytesReader past its limit.
func tooLarge(err error) bool {
	var limit *http.MaxBytesError
	return errors.As(err, &limit)
}

// refuseScanBody answers a scan body past maxScanBody with 413.
func refuseScanBody(w *recorder, req *request) error {
	_, _ = io.CopyN(io.Discard, req.r.Body, maxScanDrain)
	return jsonError(w, req, http.StatusRequestEntityTooLarge, scanBodyTooLarge)
}
