package server

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// The gate sits between each listener and net/http and rewrites the target
// of a request line before net/http parses it, where net/http and the
// parser of the Python server part:
//
//   - net/http refuses a "%" that starts no escape, the Python server keeps
//     it as it is. The gate writes such a "%" of the path as "%25", which
//     decodes to the same bytes.
//   - net/http takes the host of an absolute target over the Host header,
//     the Python server routes such a target as a path and judges the Host
//     header. The gate puts the marker of the server before a target that
//     does not start with "/", and ServeHTTP takes it off again.
//   - net/http refuses a header value with a control character other than
//     tab, and a Host header outside the bytes of a host name; h11 takes
//     both and the guard judges the Host header. The gate writes such a
//     value as the escape marker of the server and the value in percent
//     escapes, and ServeHTTP turns it back. The framing headers stay as
//     they are, so net/http frames the request as before.
//
// The gate finds a request line only while it can tell where the head
// before it ended and no body followed. After a head that may declare a
// body, or one it does not follow, it passes the rest of the connection
// through unchanged.
//
// For an HTTP/1.0 request ServeHTTP asks the gate to send the status line
// of the answer as HTTP/1.1, as uvicorn answers every request.

// gateHeadLimit is the head limit of net/http: MaxHeaderBytes plus 4096.
const gateHeadLimit = maxHeaderBytes + 4096

type gateKey struct{}

type gateListener struct {
	net.Listener
	marker, escape string
}

func (l *gateListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &gateConn{Conn: c, marker: l.marker, escape: l.escape}, nil
}

type gatePhase int

const (
	phaseLine    gatePhase = iota // the next byte starts a request line
	phaseHeaders                  // the header lines of a head
	phaseOpen                     // everything passes unchanged
)

// gateConn is a connection behind the gate. net/http reads it from one
// goroutine at a time; Write may run beside a Read.
type gateConn struct {
	net.Conn
	marker string
	escape string
	phase  gatePhase
	body   bool   // the head being read may declare a body
	head   int    // bytes of the head being read
	line   []byte // a line held back until its end arrives
	out    []byte // bytes for net/http
	start  int    // out[start:] is not read yet
	buf    [4096]byte
	as11   atomic.Bool // the next write starts an answer to HTTP/1.0
}

func (c *gateConn) Read(p []byte) (int, error) {
	for c.start == len(c.out) {
		c.out, c.start = c.out[:0], 0
		n, err := c.Conn.Read(c.buf[:])
		c.feed(c.buf[:n])
		if err != nil {
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() {
				// The connection ends: a held line goes out as it is. A
				// timeout keeps it, net/http reads again after an abort.
				c.pass()
			}
			if c.start == len(c.out) {
				return 0, err
			}
		}
	}
	n := copy(p, c.out[c.start:])
	c.start += n
	return n, nil
}

func (c *gateConn) feed(data []byte) {
	for len(data) > 0 {
		if c.phase == phaseOpen {
			c.out = append(c.out, data...)
			return
		}
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			c.line = append(c.line, data...)
			if c.head+len(c.line) > gateHeadLimit {
				c.pass()
			}
			return
		}
		c.line = append(c.line, data[:end+1]...)
		data = data[end+1:]
		c.endLine()
	}
}

// pass gives the held line out unchanged and stops rewriting.
func (c *gateConn) pass() {
	c.out = append(c.out, c.line...)
	c.line = c.line[:0]
	c.phase = phaseOpen
}

func (c *gateConn) endLine() {
	line := c.line
	c.line = c.line[:0]
	c.head += len(line)
	if c.head > gateHeadLimit {
		// net/http refuses the head as too large
		c.out = append(c.out, line...)
		c.phase = phaseOpen
		return
	}
	blank := len(line) == 1 || len(line) == 2 && line[0] == '\r'
	switch {
	case c.phase == phaseLine && blank:
		// a line end between two requests, which net/http skips or refuses
		c.out = append(c.out, line...)
		c.head = 0
	case c.phase == phaseLine:
		c.out = c.requestLine(c.out, line)
		c.phase, c.body = phaseHeaders, false
	case blank:
		c.out = append(c.out, line...)
		c.head = 0
		c.phase = phaseLine
		if c.body {
			c.phase = phaseOpen
		}
	default:
		c.out = c.headerLine(c.out, line)
		c.body = c.body || declaresBody(line)
	}
}

// headerLine appends a header line, its value escaped where net/http would
// refuse a value h11 takes.
func (c *gateConn) headerLine(out, line []byte) []byte {
	text := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
	name, _, ok := bytes.Cut(text, []byte(":"))
	if !ok || c.escape == "" || !isToken(name) {
		return append(out, line...)
	}
	start := len(name) + 1
	for start < len(text) && (text[start] == ' ' || text[start] == '\t') {
		start++
	}
	end := len(text)
	for end > start && (text[end-1] == ' ' || text[end-1] == '\t') {
		end--
	}
	value := text[start:end]
	if !needsEscape(strings.ToLower(string(name)), value) {
		return append(out, line...)
	}
	out = append(out, text[:start]...)
	out = append(out, c.escape...)
	for _, b := range value {
		if hostByte(b) {
			out = append(out, b)
		} else {
			out = append(out, '%', upperHex[b>>4], upperHex[b&15])
		}
	}
	return append(out, line[end:]...)
}

const upperHex = "0123456789ABCDEF"

// needsEscape: h11 takes the value, which holds no NUL, CR, LF, VT or FF,
// and net/http refuses it.
func needsEscape(name string, value []byte) bool {
	switch name {
	case "content-length", "transfer-encoding", "connection", "expect":
		return false
	}
	control := false
	for _, b := range value {
		switch {
		case b == 0 || b >= '\n' && b <= '\r':
			return false
		case b < ' ' && b != '\t' || b == 0x7f:
			control = true
		}
	}
	if name == "host" {
		for _, b := range value {
			if !hostByte(b) && b != '%' {
				return true
			}
		}
	}
	return control
}

// hostByte is a byte of httpguts.ValidHostHeader other than "%".
func hostByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || strings.IndexByte("!$&'()*+,-.:;=[]_~", b) >= 0
}

// isToken is the field name of h11 and of net/http.
func isToken(name []byte) bool {
	if len(name) == 0 {
		return false
	}
	for _, b := range name {
		if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || strings.IndexByte("!#$%&'*+-.^_`|~", b) >= 0) {
			return false
		}
	}
	return true
}

// unescapeHeaders turns the values the gate escaped back.
func unescapeHeaders(r *http.Request, escape string) {
	if rest, ok := strings.CutPrefix(r.Host, escape); ok {
		r.Host = percentDecode(rest)
	}
	for _, values := range r.Header {
		for i, v := range values {
			if rest, ok := strings.CutPrefix(v, escape); ok {
				values[i] = percentDecode(rest)
			}
		}
	}
}

func percentDecode(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			out = append(out, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

// requestLine appends line with its target rewritten. A line that is not
// "<method> <target> <version>" stays as it is; net/http refuses it.
func (c *gateConn) requestLine(out, line []byte) []byte {
	text := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
	method, rest, ok := bytes.Cut(text, []byte(" "))
	target, version, ok2 := bytes.Cut(rest, []byte(" "))
	if !ok || !ok2 || len(target) == 0 || bytes.IndexByte(version, ' ') >= 0 {
		return append(out, line...)
	}
	out = append(out, method...)
	out = append(out, ' ')
	if target[0] != '/' && string(target) != "*" {
		out = append(out, c.marker...)
	}
	path, query, hasQuery := bytes.Cut(target, []byte("?"))
	for i := 0; i < len(path); i++ {
		if path[i] == '%' && !(i+2 < len(path) && isHex(path[i+1]) && isHex(path[i+2])) {
			out = append(out, "%25"...)
			continue
		}
		out = append(out, path[i])
	}
	if hasQuery {
		out = append(out, '?')
		out = append(out, query...)
	}
	out = append(out, ' ')
	out = append(out, version...)
	return append(out, line[len(text):]...)
}

// declaresBody reports whether a header line may give its request a body,
// or is one the gate does not follow: a folded line or a line without a
// colon. A Content-Length counts unless it is exactly 0.
func declaresBody(line []byte) bool {
	if line[0] == ' ' || line[0] == '\t' {
		return true
	}
	name, value, ok := bytes.Cut(line, []byte(":"))
	if !ok {
		return true
	}
	switch strings.ToLower(string(bytes.TrimSpace(name))) {
	case "content-length":
		return string(bytes.TrimSpace(value)) != "0"
	case "transfer-encoding":
		return true
	}
	return false
}

// answerAs11 makes the next write, the status line of an answer to an
// HTTP/1.0 request, start with HTTP/1.1.
func (c *gateConn) answerAs11() {
	c.as11.Store(true)
}

func (c *gateConn) Write(p []byte) (int, error) {
	if c.as11.CompareAndSwap(true, false) && bytes.HasPrefix(p, []byte("HTTP/1.0 ")) {
		q := bytes.Clone(p)
		q[7] = '1'
		return c.Conn.Write(q)
	}
	return c.Conn.Write(p)
}

// CloseWrite lets net/http half close the connection as it does without
// the gate.
func (c *gateConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
