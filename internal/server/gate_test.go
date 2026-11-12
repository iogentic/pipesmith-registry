package server

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// script is a connection whose reads return the given chunks and errors in
// turn, then io.EOF; it records what is written.
type script struct {
	net.Conn
	reads   []any // string or error
	written bytes.Buffer
}

func (s *script) Read(p []byte) (int, error) {
	if len(s.reads) == 0 {
		return 0, io.EOF
	}
	next := s.reads[0]
	s.reads = s.reads[1:]
	if err, ok := next.(error); ok {
		return 0, err
	}
	text := next.(string)
	n := copy(p, text)
	if n < len(text) {
		s.reads = append([]any{text[n:]}, s.reads...)
	}
	return n, nil
}

func (s *script) Write(p []byte) (int, error) { return s.written.Write(p) }

// through is what net/http reads behind the gate when the client sends
// input in chunks of the given size.
func through(input string, chunk int) (string, error) {
	var reads []any
	for i := 0; i < len(input); i += chunk {
		reads = append(reads, input[i:min(i+chunk, len(input))])
	}
	g := &gateConn{Conn: &script{reads: reads}, marker: "/m/", escape: "m~"}
	out, err := io.ReadAll(g)
	return string(out), err
}

// TestGateEscapesHeaderValues: a header value h11 takes and net/http
// refuses gets the escape marker and percent escapes, which ServeHTTP turns
// back. Framing headers, values h11 refuses and lines that are no header
// stay as they are.
func TestGateEscapesHeaderValues(t *testing.T) {
	for _, c := range []struct{ in, out string }{
		{"GET / HTTP/1.1\r\nHost: user@localhost:1\r\n\r\n", "GET / HTTP/1.1\r\nHost: m~user%40localhost:1\r\n\r\n"},
		{"GET / HTTP/1.1\r\nhost:  a b \t\r\n\r\n", "GET / HTTP/1.1\r\nhost:  m~a%20b \t\r\n\r\n"},
		{"GET / HTTP/1.1\r\nHOST: \xa0localhost\r\n\r\n", "GET / HTTP/1.1\r\nHOST: m~%A0localhost\r\n\r\n"},
		{"GET / HTTP/1.1\r\nHost: a/b?c#d\\e\r\n\r\n", "GET / HTTP/1.1\r\nHost: m~a%2Fb%3Fc%23d%5Ce\r\n\r\n"},
		{"GET / HTTP/1.1\r\nOrigin: it's \"x\" \\ \xe9\x7f\r\n\r\n", "GET / HTTP/1.1\r\nOrigin: m~it's%20%22x%22%20%5C%20%E9%7F\r\n\r\n"},
		{"GET / HTTP/1.1\r\nX-A: 1\x01 %\r\n\r\n", "GET / HTTP/1.1\r\nX-A: m~1%01%20%25\r\n\r\n"},
		{"GET / HTTP/1.1\r\nX-A:\x1f\r\n\r\n", "GET / HTTP/1.1\r\nX-A:m~%1F\r\n\r\n"},
		{"GET / HTTP/1.1\nX-A: a\x7f\n\n", "GET / HTTP/1.1\nX-A: m~a%7F\n\n"},
		{"GET / HTTP/1.1\r\nX-A: a\x01\r\n\r\nGET /b HTTP/1.1\r\nHost: a b\r\n\r\n",
			"GET / HTTP/1.1\r\nX-A: m~a%01\r\n\r\nGET /b HTTP/1.1\r\nHost: m~a%20b\r\n\r\n"},
		{"POST / HTTP/1.1\r\nContent-Length: 2\r\nX-A: \x01\r\n\r\nX\x01GET / HTTP/1.1\r\nX-A: \x01\r\n\r\n",
			"POST / HTTP/1.1\r\nContent-Length: 2\r\nX-A: m~%01\r\n\r\nX\x01GET / HTTP/1.1\r\nX-A: \x01\r\n\r\n"},
		// values net/http takes
		{"GET / HTTP/1.1\r\nHost: localhost:1\r\n\r\n", "GET / HTTP/1.1\r\nHost: localhost:1\r\n\r\n"},
		{"GET / HTTP/1.1\r\nHost: 100%\r\n\r\n", "GET / HTTP/1.1\r\nHost: 100%\r\n\r\n"},
		{"GET / HTTP/1.1\r\nHost: \r\n\r\n", "GET / HTTP/1.1\r\nHost: \r\n\r\n"},
		{"GET / HTTP/1.1\r\nX-A: caf\xe9 \"%\" a\tb\r\n\r\n", "GET / HTTP/1.1\r\nX-A: caf\xe9 \"%\" a\tb\r\n\r\n"},
		// values h11 refuses
		{"GET / HTTP/1.1\r\nX-A: a\x00\x01\r\n\r\n", "GET / HTTP/1.1\r\nX-A: a\x00\x01\r\n\r\n"},
		{"GET / HTTP/1.1\r\nX-A: a\x0b\x01\r\n\r\n", "GET / HTTP/1.1\r\nX-A: a\x0b\x01\r\n\r\n"},
		{"GET / HTTP/1.1\r\nX-A: a\x0c\x01\r\n\r\n", "GET / HTTP/1.1\r\nX-A: a\x0c\x01\r\n\r\n"},
		{"GET / HTTP/1.1\r\nX-A: a\r\x01\r\n\r\n", "GET / HTTP/1.1\r\nX-A: a\r\x01\r\n\r\n"},
		{"GET / HTTP/1.1\r\nHost: a\x00b\r\n\r\n", "GET / HTTP/1.1\r\nHost: a\x00b\r\n\r\n"},
		// framing headers and lines that are no header field
		{"GET / HTTP/1.1\r\nConnection: close\x01\r\n\r\n", "GET / HTTP/1.1\r\nConnection: close\x01\r\n\r\n"},
		{"GET / HTTP/1.1\r\nContent-Length: 0\x01\r\n\r\n", "GET / HTTP/1.1\r\nContent-Length: 0\x01\r\n\r\n"},
		{"GET / HTTP/1.1\r\nTransfer-Encoding: x\x01\r\n\r\n", "GET / HTTP/1.1\r\nTransfer-Encoding: x\x01\r\n\r\n"},
		{"GET / HTTP/1.1\r\nExpect: x\x01\r\n\r\n", "GET / HTTP/1.1\r\nExpect: x\x01\r\n\r\n"},
		{"GET / HTTP/1.1\r\nX A: a\x01\r\n\r\n", "GET / HTTP/1.1\r\nX A: a\x01\r\n\r\n"},
		{"GET / HTTP/1.1\r\nX-A : a\x01\r\n\r\n", "GET / HTTP/1.1\r\nX-A : a\x01\r\n\r\n"},
		{"GET / HTTP/1.1\r\nX-B: 1\r\n X-A: a\x01\r\n\r\n", "GET / HTTP/1.1\r\nX-B: 1\r\n X-A: a\x01\r\n\r\n"},
		{"GET / HTTP/1.1\r\n: a\x01\r\n\r\n", "GET / HTTP/1.1\r\n: a\x01\r\n\r\n"},
	} {
		for _, chunk := range []int{1, 5, 4096} {
			got, err := through(c.in, chunk)
			if err != nil || got != c.out {
				t.Errorf("chunks of %d: %q gives %q, %v; want %q", chunk, c.in, got, err, c.out)
			}
		}
	}
}

func TestUnescapeHeaderValues(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Host = "m~user%40localhost%3"
	r.Header.Add("Origin", "m~it's%20%22x%22%20%5C%20%E9%7F")
	r.Header.Add("X-A", "1")
	r.Header.Add("X-A", "m~%01%25")
	r.Header.Add("X-B", "xm~%01")
	unescapeHeaders(r, "m~")
	if r.Host != "user@localhost%3" || r.Header.Get("Origin") != "it's \"x\" \\ \xe9\x7f" ||
		strings.Join(r.Header.Values("X-A"), "|") != "1|\x01%" || r.Header.Get("X-B") != "xm~%01" {
		t.Fatalf("%q %q", r.Host, r.Header)
	}
}

func TestGateRewritesRequestLines(t *testing.T) {
	long := "GET /" + strings.Repeat("a", gateHeadLimit) + "%zz HTTP/1.1\r\n\r\n"
	for _, c := range []struct{ in, out string }{
		{"GET /a%zz HTTP/1.1\r\nHost: x\r\n\r\n", "GET /a%25zz HTTP/1.1\r\nHost: x\r\n\r\n"},
		{"GET /a% HTTP/1.1\r\n\r\nHEAD /b%4 HTTP/1.1\r\n\r\n", "GET /a%25 HTTP/1.1\r\n\r\nHEAD /b%254 HTTP/1.1\r\n\r\n"},
		{"GET /%%41%4a%4G HTTP/1.1\r\n\r\n", "GET /%25%41%4a%254G HTTP/1.1\r\n\r\n"},
		{"GET /a%zz?b=%zz&c=% HTTP/1.1\r\n\r\n", "GET /a%25zz?b=%zz&c=% HTTP/1.1\r\n\r\n"},
		{"GET /a%25?%zz HTTP/1.0\r\n\r\n", "GET /a%25?%zz HTTP/1.0\r\n\r\n"},
		{"GET http://h/x%zz HTTP/1.1\r\n\r\n", "GET /m/http://h/x%25zz HTTP/1.1\r\n\r\n"},
		{"GET foo HTTP/1.1\r\n\r\nGET * HTTP/1.1\r\n\r\n", "GET /m/foo HTTP/1.1\r\n\r\nGET * HTTP/1.1\r\n\r\n"},
		{"GET /a% HTTP/1.1\nHost: x\n\nGET /b% HTTP/1.1\n\n", "GET /a%25 HTTP/1.1\nHost: x\n\nGET /b%25 HTTP/1.1\n\n"},
		{"\r\nGET /a% HTTP/1.1\r\n\r\n", "\r\nGET /a%25 HTTP/1.1\r\n\r\n"},
		{"GET /a% HTTP/1.1\r\nContent-Length: 0\r\n\r\nGET /b% HTTP/1.1\r\n\r\n", "GET /a%25 HTTP/1.1\r\nContent-Length: 0\r\n\r\nGET /b%25 HTTP/1.1\r\n\r\n"},
		{"GET /a% HTTP/1.1\r\ncontent-length:  0 \r\n\r\nGET /b% HTTP/1.1\r\n\r\n", "GET /a%25 HTTP/1.1\r\ncontent-length:  0 \r\n\r\nGET /b%25 HTTP/1.1\r\n\r\n"},
		// after a head that may declare a body nothing changes any more
		{"PUT /a% HTTP/1.1\r\nContent-Length: 4\r\n\r\nx%zzGET /b% HTTP/1.1\r\n\r\n", "PUT /a%25 HTTP/1.1\r\nContent-Length: 4\r\n\r\nx%zzGET /b% HTTP/1.1\r\n\r\n"},
		{"POST /a HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\nGET /b% HTTP/1.1\r\n\r\n", "POST /a HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\nGET /b% HTTP/1.1\r\n\r\n"},
		{"GET /a HTTP/1.1\r\nContent-Length: 00\r\n\r\nGET /b% HTTP/1.1\r\n\r\n", "GET /a HTTP/1.1\r\nContent-Length: 00\r\n\r\nGET /b% HTTP/1.1\r\n\r\n"},
		{"GET /a HTTP/1.1\r\nX-A: 1\r\n Content-Length: 5\r\n\r\nGET /b% HTTP/1.1\r\n\r\n", "GET /a HTTP/1.1\r\nX-A: 1\r\n Content-Length: 5\r\n\r\nGET /b% HTTP/1.1\r\n\r\n"},
		{"GET /a HTTP/1.1\r\nno colon\r\n\r\nGET /b% HTTP/1.1\r\n\r\n", "GET /a HTTP/1.1\r\nno colon\r\n\r\nGET /b% HTTP/1.1\r\n\r\n"},
		// lines net/http refuses stay as they are
		{"GET /a%zz b HTTP/1.1\r\n\r\n", "GET /a%zz b HTTP/1.1\r\n\r\n"},
		{"GET /a%zz\r\n\r\n", "GET /a%zz\r\n\r\n"},
		{"GET  /a%zz HTTP/1.1\r\n\r\n", "GET  /a%zz HTTP/1.1\r\n\r\n"},
		{long, long},
		// a connection that ends inside a line passes what came
		{"GET /a%zz HTT", "GET /a%zz HTT"},
	} {
		for _, chunk := range []int{1, 3, 4096} {
			got, err := through(c.in, chunk)
			if err != nil || got != c.out {
				t.Errorf("chunks of %d: %.60q gives %.80q, %v; want %.80q", chunk, c.in, got, err, c.out)
			}
		}
	}
}

// A timeout, as net/http causes it to stop a background read, keeps the
// line held back; the next read goes on with it.
func TestGateKeepsALineOverATimeout(t *testing.T) {
	timeout := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	g := &gateConn{Conn: &script{reads: []any{"GET /a%", timeout, "zz HTTP/1.1\r\n\r\n"}}, marker: "/m/"}
	buf := make([]byte, 64)
	if n, err := g.Read(buf); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("%d %v", n, err)
	}
	rest, err := io.ReadAll(g)
	if err != nil || string(rest) != "GET /a%25zz HTTP/1.1\r\n\r\n" {
		t.Fatalf("%q %v", rest, err)
	}
	reset := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	g = &gateConn{Conn: &script{reads: []any{"GET /a%", reset}}, marker: "/m/"}
	if n, err := g.Read(buf); n != 7 || err != nil || string(buf[:n]) != "GET /a%" {
		t.Fatalf("%d %v %q", n, err, buf[:n])
	}
}

func TestGateAnswersHTTP10AsHTTP11(t *testing.T) {
	s := &script{}
	g := &gateConn{Conn: s}
	g.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))
	g.answerAs11()
	data := []byte("HTTP/1.0 403 Forbidden\r\n\r\nbody HTTP/1.0 ")
	if n, err := g.Write(data); n != len(data) || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	g.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))
	if string(data) != "HTTP/1.0 403 Forbidden\r\n\r\nbody HTTP/1.0 " {
		t.Fatalf("the caller's bytes changed: %q", data)
	}
	want := "HTTP/1.0 200 OK\r\n\r\nHTTP/1.1 403 Forbidden\r\n\r\nbody HTTP/1.0 HTTP/1.0 200 OK\r\n\r\n"
	if s.written.String() != want {
		t.Fatalf("%q", s.written.String())
	}
}

func TestGateHalfCloses(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	gl := &gateListener{Listener: l, marker: "/m/"}
	done := make(chan net.Conn, 1)
	go func() {
		c, err := gl.Accept()
		if err != nil {
			t.Error(err)
		}
		done <- c
	}()
	client, err := net.Dial("tcp4", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	c := <-done
	defer c.Close()
	if _, ok := c.(*gateConn); !ok {
		t.Fatalf("%T", c)
	}
	if err := c.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := client.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("%d %v", n, err)
	}
}
