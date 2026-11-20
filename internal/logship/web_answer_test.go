package logship

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The field MaxResponseHeaderBytes of http.Transport is the limit of the
// bytes of the head of an answer, and zero means a default limit; read with
// go doc net/http Transport.MaxResponseHeaderBytes of Go 1.27.4 on
// 2026-11-19. Its transport.go writes the failure as "net/http: server
// response headers exceeded %d bytes; aborted".

// countingConn counts the bytes the client reads from a receiver.
type countingConn struct {
	net.Conn
	read *atomic.Int64
}

func (c countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// countReads makes every connection of web count what it reads into read.
// Call it before the first request.
func countReads(web *webClient, read *atomic.Int64) {
	dial := web.transport.DialContext
	web.transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return countingConn{Conn: c, read: read}, nil
	}
}

// rawStub reads one request per connection and writes the answer its write
// function makes, one connection at a time; answered gets the bytes of each
// answer that reached the connection.
type rawStub struct {
	ln       net.Listener
	write    func(w io.Writer) (int64, error)
	answered chan int64
	done     chan struct{}
}

func newRawStub(t *testing.T, write func(w io.Writer) (int64, error)) *rawStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &rawStub{ln: ln, write: write, answered: make(chan int64, 16), done: make(chan struct{})}
	go s.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		<-s.done
	})
	return s
}

func (s *rawStub) serve() {
	defer close(s.done)
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if req, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
			_, _ = io.Copy(io.Discard, req.Body)
			n, _ := s.write(conn) // the client closing the connection ends the answer
			s.answered <- n
		}
		_ = conn.Close()
	}
}

func (s *rawStub) url() string { return "http://" + s.ln.Addr().String() }

// counting writes to w and counts what reached it.
type counting struct {
	w io.Writer
	n int64
}

func (c *counting) write(text string) error {
	n, err := io.WriteString(c.w, text)
	c.n += int64(n)
	return err
}

// hugeHead is an answer 200 whose head holds size bytes of header lines.
func hugeHead(size int) func(w io.Writer) (int64, error) {
	return func(w io.Writer) (int64, error) {
		c := &counting{w: w}
		if err := c.write("HTTP/1.1 200 OK\r\n"); err != nil {
			return c.n, err
		}
		pad := strings.Repeat("x", 1000)
		for i := 0; int(c.n) < size; i++ {
			if err := c.write(fmt.Sprintf("X-Pad-%04d: %s\r\n", i, pad)); err != nil {
				return c.n, err
			}
		}
		return c.n, c.write("Content-Length: 0\r\n\r\n")
	}
}

// An answer whose head holds 1 MiB of header lines is a failed attempt of
// class network: the transport of a target reads at most 64 KiB of a head,
// so the client never reads the megabyte. Both what the client read and
// what the stub could write are logged.
func TestAnswerHeadIsBounded(t *testing.T) {
	const head = 1 << 20
	stub := newRawStub(t, hugeHead(head))
	a, err := openHTTPJSON(Target{Name: "demo", Kind: kindHTTPJSON, Address: stub.url()}, "", env{version: testVersion, clock: realClock{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	var read atomic.Int64
	countReads(a.(*httpJSON).web, &read)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = a.Send(ctx, []logs.Line{testLine("server", 1)})
	var written int64
	select {
	case written = <-stub.answered:
	case <-time.After(10 * time.Second):
		t.Fatal("the stub still writes its answer")
	}
	t.Logf("the client read %d bytes; the stub wrote %d bytes of a head of %d", read.Load(), written, head)
	if class := classOf(err); err == nil || class != "network" {
		t.Fatalf("the attempt: %v, class %s; want a failure of class network", err, class)
	}
	if n := read.Load(); n == 0 || n > 64<<10 {
		t.Fatalf("the client read %d bytes, want 1 to %d", n, 64<<10)
	}
}

// bodyOf is an answer 200 with a body of size bytes.
func bodyOf(size int) func(w io.Writer) (int64, error) {
	return func(w io.Writer) (int64, error) {
		c := &counting{w: w}
		if err := c.write(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", size)); err != nil {
			return c.n, err
		}
		chunk := strings.Repeat("y", 16<<10)
		for sent := 0; sent < size; sent += len(chunk) {
			if err := c.write(chunk[:min(len(chunk), size-sent)]); err != nil {
				return c.n, err
			}
		}
		return c.n, nil
	}
}

// The bound of an answer body is the limit an adapter passes to post: of a
// body of 1 MiB, post reads the limit given, and 64 KiB for a limit of 0.
// The adapter http-json passes that default: counted at the connection it
// reads at least 64 KiB of the body and at most 8 KiB more, the head and
// what the reader of the transport reads ahead.
func TestAnswerBodyBoundIsAParameter(t *testing.T) {
	const body = 1 << 20
	stub := newRawStub(t, bodyOf(body))
	target := Target{Name: "demo", Kind: kindHTTPJSON, Address: stub.url()}
	web, err := newWebClient(target, "", env{version: testVersion, clock: realClock{}})
	if err != nil {
		t.Fatal(err)
	}
	defer web.close()
	for _, c := range []struct{ limit, want int64 }{{0, 64 << 10}, {64<<10 + 3<<10, 64<<10 + 3<<10}, {100, 100}, {-1, 64 << 10}} {
		ans, err := web.post(t.Context(), contentTypeNDJSON, []byte("{}\n"), nil, c.limit)
		if err != nil || ans.status != 200 {
			t.Fatalf("limit %d: %v, status %d", c.limit, err, ans.status)
		}
		if int64(len(ans.body)) != c.want {
			t.Errorf("limit %d: %d bytes of the body read, want %d", c.limit, len(ans.body), c.want)
		}
		<-stub.answered
	}
	a, err := openHTTPJSON(target, "", env{version: testVersion, clock: realClock{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	var read atomic.Int64
	countReads(a.(*httpJSON).web, &read)
	if _, err := a.Send(t.Context(), []logs.Line{testLine("server", 1)}); err != nil {
		t.Fatal(err)
	}
	<-stub.answered
	if n := read.Load(); n < 64<<10 || n > 64<<10+8<<10 {
		t.Fatalf("http-json read %d bytes of an answer with a body of %d, want 64 KiB of the body and at most 8 KiB more", n, body)
	}
	t.Logf("http-json read %d bytes of an answer with a body of %d bytes", read.Load(), body)
}
