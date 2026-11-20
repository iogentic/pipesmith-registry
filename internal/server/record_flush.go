package server

import "net/http"

// Flush passes a flush to the writer of net/http, for the assertion of
// http.Flusher in streamHeaders. A flush before any write sends the head
// with status 200.
func (w *countingWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
