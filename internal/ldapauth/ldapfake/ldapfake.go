// Package ldapfake is a directory server on 127.0.0.1 for tests: it answers
// a simple bind, a search and an unbind in the encoding of the LDAP
// protocol (RFC 4511, sections 4.2, 4.5 and 4.3) without the LDAP module,
// so that a test can see what a client asks and send it more than it asks
// for. It does not evaluate filters: a search below a base that holds
// "ou=groups" gets Groups, a search of the base object gets one entry of
// that base, any other search gets Users. Every value of a test is
// invented.
package ldapfake

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Entry is an entry of the directory.
type Entry struct {
	DN         string
	Attributes map[string][]string
}

// Search is a search request as the server received it.
type Search struct {
	Base      string
	Scope     int
	SizeLimit int
	TimeLimit int
}

// Server is the directory. Set its fields before the first request.
type Server struct {
	// Password is the one bind password the server takes for a name; an
	// anonymous bind always passes.
	Password string
	Users    []Entry
	Groups   []Entry
	// IgnoreSizeLimit sends every entry whatever the size limit of the
	// request asks.
	IgnoreSizeLimit bool
	// Delay is the wait before the answer of a search.
	Delay time.Duration

	ln       net.Listener
	mu       sync.Mutex
	binds    []string
	searches []Search
}

// Start listens on a free port of 127.0.0.1.
func Start(password string) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{Password: password, ln: ln}
	go s.accept()
	return s, nil
}

// Port is the port the server listens on.
func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Close stops the server.
func (s *Server) Close() { _ = s.ln.Close() }

// Binds are the passwords of the binds of names, in their order.
func (s *Server) Binds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.binds...)
}

// Searches are the search requests in their order.
func (s *Server) Searches() []Search {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Search(nil), s.searches...)
}

func (s *Server) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serve(conn)
	}
}

// element is one BER element: its tag and its content.
type element struct {
	tag     byte
	content []byte
}

func readElement(r io.Reader) (element, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil {
		return element{}, err
	}
	n := int(head[1])
	if n&0x80 != 0 {
		size := make([]byte, n&0x7f)
		if len(size) > 4 {
			return element{}, errors.New("ldapfake: a length of more than 4 bytes")
		}
		if _, err := io.ReadFull(r, size); err != nil {
			return element{}, err
		}
		n = 0
		for _, b := range size {
			n = n<<8 | int(b)
		}
	}
	content := make([]byte, n)
	_, err := io.ReadFull(r, content)
	return element{head[0], content}, err
}

// children parses the content of a constructed element.
func children(data []byte) []element {
	var out []element
	for len(data) >= 2 {
		n, i := int(data[1]), 2
		if n&0x80 != 0 {
			size := n & 0x7f
			if len(data) < 2+size {
				return out
			}
			n = 0
			for _, b := range data[2 : 2+size] {
				n = n<<8 | int(b)
			}
			i += size
		}
		if len(data) < i+n {
			return out
		}
		out = append(out, element{data[0], data[i : i+n]})
		data = data[i+n:]
	}
	return out
}

func integer(content []byte) int {
	n := 0
	for _, b := range content {
		n = n<<8 | int(b)
	}
	return n
}

func wrap(tag byte, parts ...[]byte) []byte {
	var content []byte
	for _, p := range parts {
		content = append(content, p...)
	}
	if len(content) < 0x80 {
		return append([]byte{tag, byte(len(content))}, content...) // #nosec G115 -- the branch holds lengths below 0x80.
	}
	size := make([]byte, 4)
	binary.BigEndian.PutUint32(size, uint32(len(content))) // #nosec G115 -- an entry of a test is far below 4 GiB.
	for len(size) > 1 && size[0] == 0 {
		size = size[1:]
	}
	return append(append([]byte{tag, 0x80 | byte(len(size))}, size...), content...) // #nosec G115 -- size holds at most 4 bytes.
}

func text(tag byte, value string) []byte { return wrap(tag, []byte(value)) }

// result is an LDAPResult of the operation tag with the result code.
func result(tag byte, code int) []byte {
	return wrap(tag, []byte{0x0a, 1, byte(code)}, text(0x04, ""), text(0x04, "")) // #nosec G115 -- LDAP result codes of the fake are below 256.
}

func entry(e Entry) []byte {
	var attributes [][]byte
	for name, values := range e.Attributes {
		var vals [][]byte
		for _, v := range values {
			vals = append(vals, text(0x04, v))
		}
		attributes = append(attributes, wrap(0x30, text(0x04, name), wrap(0x31, vals...)))
	}
	return wrap(0x64, text(0x04, e.DN), wrap(0x30, attributes...))
}

func (s *Server) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	for {
		message, err := readElement(conn)
		if err != nil {
			return
		}
		parts := children(message.content)
		if len(parts) < 2 {
			return
		}
		id := wrap(0x02, parts[0].content)
		send := func(op []byte) bool {
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_, err := conn.Write(wrap(0x30, id, op))
			return err == nil
		}
		switch op := parts[1]; op.tag {
		case 0x60: // BindRequest: version, name, simple [0] password
			fields := children(op.content)
			name, password := "", ""
			if len(fields) > 2 {
				name, password = string(fields[1].content), string(fields[2].content)
			}
			code := 49
			if name == "" && password == "" || password == s.Password {
				code = 0
			}
			if name != "" {
				s.mu.Lock()
				s.binds = append(s.binds, password)
				s.mu.Unlock()
			}
			send(result(0x61, code))
		case 0x63: // SearchRequest: base, scope, deref, size limit, time limit, ...
			fields := children(op.content)
			if len(fields) < 5 {
				return
			}
			req := Search{Base: string(fields[0].content), Scope: integer(fields[1].content), SizeLimit: integer(fields[3].content),
				TimeLimit: integer(fields[4].content)}
			s.mu.Lock()
			s.searches = append(s.searches, req)
			s.mu.Unlock()
			time.Sleep(s.Delay)
			entries := s.Users
			switch {
			case req.Scope == 0:
				entries = []Entry{{DN: req.Base}}
			case strings.Contains(strings.ToLower(req.Base), "ou=groups"):
				entries = s.Groups
			}
			code := 0
			if !s.IgnoreSizeLimit && req.SizeLimit > 0 && len(entries) > req.SizeLimit {
				entries, code = entries[:req.SizeLimit], 4
			}
			for _, e := range entries {
				if !send(entry(e)) {
					return
				}
			}
			send(result(0x65, code))
		default: // UnbindRequest and anything else end the connection
			return
		}
	}
}
