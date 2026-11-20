package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A request refused as invalid before its access line keeps no npm token
// of its path in the record (finding 7a-7): the token is masked as the
// access line masks it.
func TestInvalidRequestRecordMasksTheToken(t *testing.T) {
	rs := &recordSink{}
	s := &Server{requests: rs.add}
	recorded := httptest.NewRecorder()
	given := httptest.NewRequest("DELETE", "/repository/demo-npm/-/user/token/demo-token", nil)
	given.RequestURI = "/repository/demo-npm/-/user/token/demo-token\x01secret?a=1"
	w, r := http.ResponseWriter(recorded), given
	done := s.track(&w, &r)
	s.invalidRequest(w)
	done()
	rec := rs.next(t)
	if rec.Status != http.StatusBadRequest || strings.Contains(rec.Path+rec.Query, "demo-token") ||
		rec.Path != "/repository/demo-npm/-/user/token/-" {
		t.Fatalf("%+v", rec)
	}
}
