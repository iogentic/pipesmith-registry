package channel

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// The refusal of a redirect names its target within a bound (C-7).
func TestRedirectTargetIsBounded(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	host := strings.Repeat("a", 3000) + ".example"
	r.wrap = onFile(release.SumsFile, func(w http.ResponseWriter, req *http.Request, _ http.Handler) {
		w.Header().Set("Location", "http://"+host+":1/x")
		w.WriteHeader(http.StatusFound)
	})
	r.start()
	err := r.refused(ErrRedirect)
	if len(err.Error()) > 400 || strings.Contains(err.Error(), host) || !strings.Contains(err.Error(), "aaaa") {
		t.Fatalf("message of %d bytes: %.500s", len(err.Error()), err)
	}
}

// An answer without a length that ends before its last chunk is refused
// without a length of -1 in the message (C-8).
func TestShortAnswerWithoutLength(t *testing.T) {
	r := newRig(t)
	r.publish(published{version: "0.2.0"})
	r.wrap = onFile(platformBinary(), func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(buf, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nabcde\r\n10\r\nabc")
		buf.Flush()
	})
	r.start()
	err := r.refused(ErrShort)
	if strings.Contains(err.Error(), "-1") || !strings.Contains(err.Error(), "8 bytes") {
		t.Fatalf("message %v", err)
	}
}
