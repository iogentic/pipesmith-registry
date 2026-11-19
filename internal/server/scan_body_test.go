package server

import (
	"strings"
	"testing"
)

func TestScanBodyIsBounded(t *testing.T) {
	a, _ := scanApp(t, 3)
	h := []string{"X-Artifact-Repository", "1", "Host", a.localhost()}
	padded := func(size int) []byte {
		head := `{"store": "nope", "padding": "`
		return []byte(head + strings.Repeat("a", size-len(head)-2) + `"}`)
	}
	if r := a.post(padded(maxScanBody), h...); r.status != 404 {
		t.Errorf("a body of %d bytes: %d %s", maxScanBody, r.status, r.body)
	}
	for _, body := range [][]byte{padded(maxScanBody + 1), padded(8 << 20), []byte(strings.Repeat("[", 1_000_000))} {
		r := a.post(body, h...)
		if r.status != 413 || string(r.body) != `{"error":"the body is larger than 65536 bytes"}` ||
			r.header.Get("Content-Type") != "application/json" {
			t.Errorf("a body of %d bytes: %d %v %s", len(body), r.status, r.header, r.body)
		}
	}
	if r := a.post([]byte(`{"store": "s1"}`), h...); r.status != 202 && r.status != 409 {
		t.Errorf("a scan after the refusals: %d %s", r.status, r.body)
	}
}
