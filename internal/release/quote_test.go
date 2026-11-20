package release

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Values of the health answer appear quoted in an error, so a line break or
// a control byte of the server cannot forge a line of the output (finding
// A-12).
func TestHealthValuesQuoted(t *testing.T) {
	for path, body := range map[string]string{"/api/health": `{"status":"down\nhealthy: x"}`, "/api/status": `{"version":"0.2.0\u001b[2K"}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == path:
				fmt.Fprint(w, body)
			case r.URL.Path == "/api/health":
				fmt.Fprint(w, `{"status":"ok"}`)
			}
		}))
		err := probe(srv.Client(), srv.URL, "0.2.0")
		srv.Close()
		if err == nil || strings.ContainsAny(err.Error(), "\n\x1b") || !strings.Contains(err.Error(), `\n`+"healthy") && !strings.Contains(err.Error(), `\x1b`) {
			t.Fatalf("%s: %q", path, err)
		}
	}
}
