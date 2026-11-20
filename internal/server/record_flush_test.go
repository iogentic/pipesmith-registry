package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// With the request sink set, streamHeaders still flushes the head of an
// archive through the counting writer, so net/http frames the body as a
// stream (finding 7a-2). The record keeps the status 200.
func TestStreamHeadersFlushWithSink(t *testing.T) {
	rs := &recordSink{}
	s := &Server{requests: rs.add}
	recorded := httptest.NewRecorder()
	w, r := http.ResponseWriter(recorded), httptest.NewRequest("GET", "/api/export/demo-releases", nil)
	done := s.track(&w, &r)
	if _, ok := w.(*countingWriter); !ok {
		t.Fatalf("the writer is %T, not the counting writer", w)
	}
	streamHeaders(&recorder{ResponseWriter: w, s: s, r: r}, "application/zip", "demo.zip")
	if !recorded.Flushed || recorded.Code != http.StatusOK {
		t.Fatalf("flushed %v, status %d", recorded.Flushed, recorded.Code)
	}
	done()
	if rec := rs.next(t); rec.Status != http.StatusOK {
		t.Fatalf("%+v", rec)
	}
}
