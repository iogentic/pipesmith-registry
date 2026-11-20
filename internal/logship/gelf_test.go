package logship

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The rules of GELF 1.1, read on 2026-11-19: the payload and the transports
// in https://go2docs.graylog.org/current/getting_in_log_data/gelf_format.html
// and in the archived https://archivedocs.graylog.org/en/latest/pages/gelf.html
// (version, host and short_message required, timestamp in seconds, level a
// syslog severity, additional fields _[\w.\-]+, no _id; over UDP chunks of
// the magic bytes 0x1e 0x0f, a message id of 8 bytes, a sequence number below
// the count and the count, at most 128 chunks, all within 5 seconds; over TCP
// each message ends with a null byte, without compression and without
// chunks); the JSON schema linked from the first page,
// https://github.com/user-attachments/files/31075418/gelf-1.1.schema.json
// (version the string "1.1", host and short_message not blank, timestamp a
// number above 0, level an integer of 0 to 7, additional field values a
// string or a number, _id false, no other keys); and the input page
// https://go2docs.graylog.org/current/getting_in_log_data/gelf.html (HTTP: a
// POST to /gelf answered with 202 Accepted; several messages in one body only
// with an option the operator turns on). The choices of this server on top:
// the fields version, host, short_message, timestamp with three decimals,
// level, _kind, _logger and _line and no others, a datagram of at most 8,192
// bytes, chunks only for a message above 8,192 bytes, no compression,
// Content-Type application/json, one message per POST. unverified: the
// delimiter a TCP input expects by default (the input page names a newline
// and an option for the null byte, the format pages name the null byte, which
// this server sends), and the statuses a receiver gives a bad request (the
// statuses of the stub are ours).

// gelfStub is a receiver of GELF over udp, tcp, http or https. It reads each
// message with decodeGELF and keeps what it took and why it refused the
// rest. A broken frame over tcp closes the connection; a bad datagram is
// dropped; a bad request gets a 4xx answer.
type gelfStub struct {
	pc     net.PacketConn
	ln     net.Listener
	srv    *httptest.Server
	path   string // http: the path it takes
	auth   string // http: the Authorization it takes; empty takes none
	caFile string // https: the authority of its certificate
	wg     sync.WaitGroup

	mu         sync.Mutex
	status     func(n int) int // http: the status of request n once its message was read; 0 is 202
	retryAfter string          // http: the Retry-After of an answer of status
	closeAfter int             // tcp: the stub ends a connection after this many messages; 0 keeps it
	open       []net.Conn
	sets       map[[8]byte]*chunkSet
	took       []gelfTaken
	refused    []string
	conns      int
	closed     int
	requests   int
}

// chunkSet is the chunks of one message id that came so far.
type chunkSet struct {
	count byte
	parts [][]byte
	got   int
	first time.Time
}

// gelfTaken is one message a GELF stub took.
type gelfTaken struct {
	host, short, timestamp string
	level                  int
	kind, logger, line, id string
	size                   int // the bytes of the message
	chunks                 int // udp: the chunks it came in; 0 for one datagram
	conn                   int // tcp: the connection it came on, from 1
}

// newGELFStub starts a stub of scheme on 127.0.0.1: udp, tcp, http, or https
// with a certificate of testCA. An http stub takes POST requests to path
// with the Authorization auth.
func newGELFStub(t *testing.T, scheme, path, auth string) *gelfStub {
	t.Helper()
	s := &gelfStub{path: path, auth: auth, sets: map[[8]byte]*chunkSet{}}
	switch scheme {
	case "udp":
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		readBuffer(t, pc)
		s.pc = pc
		s.wg.Add(1)
		go s.readDatagrams()
		t.Cleanup(func() {
			_ = pc.Close()
			s.wg.Wait()
		})
	case "tcp":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		s.ln = ln
		s.wg.Add(1)
		go s.accept()
		t.Cleanup(s.close)
	default:
		s.srv = httptest.NewUnstartedServer(s)
		if scheme == "https" {
			var cert tls.Certificate
			s.caFile, cert = testCA(t)
			s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			s.srv.StartTLS()
		} else {
			s.srv.Start()
		}
		t.Cleanup(s.srv.Close)
	}
	return s
}

// address is the address of the stub for a row: udp:// or tcp://, or the URL
// of an http stub, without a path.
func (s *gelfStub) address() string {
	switch {
	case s.pc != nil:
		return "udp://" + s.pc.LocalAddr().String()
	case s.ln != nil:
		return "tcp://" + s.ln.Addr().String()
	}
	return s.srv.URL
}

func (s *gelfStub) setStatus(status func(n int) int, retryAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.retryAfter = status, retryAfter
}

func (s *gelfStub) setCloseAfter(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeAfter = n
}

func (s *gelfStub) readDatagrams() {
	defer s.wg.Done()
	buf := make([]byte, 65536)
	for {
		n, _, err := s.pc.ReadFrom(buf)
		if err != nil {
			return
		}
		s.datagram(append([]byte(nil), buf[:n]...))
	}
}

// datagram takes one datagram: a chunk, or a whole message.
func (s *gelfStub) datagram(d []byte) {
	switch {
	case bytes.HasPrefix(d, []byte{0x1e, 0x0f}):
		s.chunk(d)
	case len(d) > 8192:
		s.refuse(fmt.Sprintf("a datagram of %d bytes that is not chunked: the server chunks above 8,192", len(d)))
	default:
		s.message(d, 0)
	}
}

// chunk takes one chunk: the magic bytes 0x1e 0x0f, a message id of 8 bytes,
// the sequence number and the count, then its part of the message. The
// message is read once its chunks are all there.
func (s *gelfStub) chunk(d []byte) {
	if len(d) > 8192 {
		s.refuse(fmt.Sprintf("a chunk of %d bytes: the server keeps a datagram within 8,192", len(d)))
		return
	}
	if len(d) <= 12 {
		s.refuse("a chunk without a part of a message")
		return
	}
	var id [8]byte
	copy(id[:], d[2:10])
	seq, count := d[10], d[11]
	switch {
	case count == 0 || count > 128:
		s.refuse(fmt.Sprintf("a sequence count of %d, not 1 to 128", count))
		return
	case seq >= count:
		s.refuse(fmt.Sprintf("the sequence number %d of a sequence count of %d", seq, count))
		return
	}
	s.mu.Lock()
	set := s.sets[id]
	if set == nil {
		set = &chunkSet{count: count, parts: make([][]byte, count), first: time.Now()}
		s.sets[id] = set
	}
	problem := ""
	switch {
	case set.count != count:
		problem = fmt.Sprintf("a sequence count of %d after %d for one message id", count, set.count)
	case set.parts[seq] != nil:
		problem = fmt.Sprintf("the chunk %d of a message twice", seq)
	default:
		set.parts[seq] = d[12:]
		set.got++
	}
	done := problem == "" && set.got == int(set.count)
	if done {
		delete(s.sets, id)
	}
	s.mu.Unlock()
	if problem != "" {
		s.refuse(problem)
	}
	if !done {
		return
	}
	switch msg := bytes.Join(set.parts, nil); {
	case time.Since(set.first) > 5*time.Second:
		s.refuse("the chunks of a message came in more than 5 seconds")
	case len(msg) <= 8192:
		s.refuse(fmt.Sprintf("a message of %d bytes in chunks: the server chunks above 8,192 only", len(msg)))
	default:
		s.message(msg, int(count))
	}
}

// message reads one whole message over udp and takes it, or refuses it.
func (s *gelfStub) message(data []byte, chunks int) {
	m, err := decodeGELF(data)
	if err != nil {
		s.refuse(err.Error())
		return
	}
	m.chunks = chunks
	s.take(m)
}

func (s *gelfStub) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns++
		n := s.conns
		s.open = append(s.open, conn)
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(conn, n)
		}()
	}
}

// serve reads the frames of connection n: each message ends with a null
// byte.
func (s *gelfStub) serve(conn net.Conn, n int) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	taken := 0
	for {
		frame, err := readNullFrame(r)
		var broken refusal
		switch {
		case errors.As(err, &broken):
			s.refuse(string(broken))
			return
		case err != nil:
			return // the end of the stream, or of the stub
		}
		if reason := tcpFrameProblem(frame); reason != "" {
			s.refuse(reason) // the stream is not framed by the null byte: the connection ends
			return
		}
		m, err := decodeGELF(frame)
		if err != nil {
			s.refuse(err.Error())
			continue
		}
		m.conn = n
		s.take(m)
		taken++
		s.mu.Lock()
		after := s.closeAfter
		s.mu.Unlock()
		if after > 0 && taken == after {
			endConnection(conn, r)
			s.mu.Lock()
			s.closed++
			s.mu.Unlock()
			return
		}
	}
}

func (s *gelfStub) close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for _, c := range s.open {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// maxGELFFrame bounds a message over tcp and a body over http in the stub;
// the pages name no bound.
const maxGELFFrame = 4 << 20

// readNullFrame reads the bytes up to the next null byte, which ends a
// message over TCP. A stream that ends inside a frame lacks its null byte,
// which is a refusal; another error ends the stream.
func readNullFrame(r *bufio.Reader) ([]byte, error) {
	var frame []byte
	for {
		part, err := r.ReadSlice(0)
		frame = append(frame, part...)
		switch {
		case err == nil:
			return frame[:len(frame)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			if len(frame) > maxGELFFrame {
				return nil, refusal("a frame above the bound of the stub")
			}
		case errors.Is(err, io.EOF) && len(frame) > 0:
			return nil, refusal("a stream that ends without the null byte of its last frame")
		default:
			return nil, err
		}
	}
}

// tcpFrameProblem names what makes a frame no message of GELF over TCP: a
// line break, which newline framing leaves, compression, or a chunk.
func tcpFrameProblem(frame []byte) string {
	switch {
	case bytes.IndexByte(frame, '\n') >= 0:
		return "a line break in a frame: newline framing in place of the null byte"
	case compression(frame) != "":
		return "a frame compressed with " + compression(frame) + ": GELF over TCP takes no compression"
	case bytes.HasPrefix(frame, []byte{0x1e, 0x0f}):
		return "a chunk in a frame: GELF over TCP takes no chunks"
	}
	return ""
}

// compression is the compression a payload starts with by its magic bytes:
// GZIP 0x1f 0x8b, ZLIB 0x78 and a second byte that makes the pair a multiple
// of 31; empty for none.
func compression(data []byte) string {
	switch {
	case len(data) < 2:
	case data[0] == 0x1f && data[1] == 0x8b:
		return "GZIP"
	case data[0] == 0x78 && (int(data[0])<<8|int(data[1]))%31 == 0:
		return "ZLIB"
	}
	return ""
}

func (s *gelfStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests++
	n, status, retryAfter := s.requests, s.status, s.retryAfter
	s.mu.Unlock()
	m, code, reason := s.decodeRequest(r)
	if code == 0 && status != nil {
		if code = status(n); code != 0 {
			reason = "the answer of the test"
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
		}
	}
	if code != 0 {
		s.refuse(reason)
		http.Error(w, reason, code)
		return
	}
	s.take(m)
	w.WriteHeader(http.StatusAccepted)
}

// decodeRequest applies the rules of GELF over HTTP and of this server; a
// code other than 0 refuses the request.
func (s *gelfStub) decodeRequest(r *http.Request) (gelfTaken, int, string) {
	switch {
	case r.Method != http.MethodPost:
		return gelfTaken{}, http.StatusMethodNotAllowed, "method " + r.Method
	case r.URL.Path != s.path:
		return gelfTaken{}, http.StatusNotFound, "path " + r.URL.Path
	case r.Header.Get("Content-Type") != "application/json":
		return gelfTaken{}, http.StatusUnsupportedMediaType, "content type " + r.Header.Get("Content-Type")
	case r.Header.Get("Content-Encoding") != "":
		return gelfTaken{}, http.StatusUnsupportedMediaType, "content encoding: the server sends no compression"
	case r.Header.Get("User-Agent") != "pipesmith-registry/"+testVersion:
		return gelfTaken{}, http.StatusBadRequest, "user agent " + r.Header.Get("User-Agent")
	case r.Header.Get("Authorization") != s.auth:
		return gelfTaken{}, http.StatusUnauthorized, "authorization"
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGELFFrame+1))
	if err != nil || len(body) > maxGELFFrame {
		return gelfTaken{}, http.StatusBadRequest, "body not read"
	}
	m, err := decodeGELF(body)
	if err != nil {
		return gelfTaken{}, http.StatusBadRequest, err.Error()
	}
	return m, 0, ""
}

func (s *gelfStub) take(m gelfTaken) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.took = append(s.took, m)
}

func (s *gelfStub) refuse(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused = append(s.refused, reason)
}

func (s *gelfStub) messages() []gelfTaken {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gelfTaken(nil), s.took...)
}

func (s *gelfStub) refusals() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.refused...)
}

// seen is the number of messages taken and refused.
func (s *gelfStub) seen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.took) + len(s.refused)
}

// connections is the number of connections accepted, and of those the stub
// ended after closeAfter messages and the sender closed in turn.
func (s *gelfStub) connections() (accepted, closed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns, s.closed
}

func (s *gelfStub) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// pending is the number of messages over udp whose chunks are not all there.
func (s *gelfStub) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sets)
}

var (
	// gelfFieldName is the name of an additional field in the JSON schema.
	gelfFieldName = regexp.MustCompile(`^_[\w.\-]+$`)
	// gelfTime is a timestamp as this server writes it: seconds with three
	// decimals.
	gelfTime = regexp.MustCompile(`^[0-9]+\.[0-9]{3}$`)
)

// gelfSpecified are the keys the schema names, besides the additional
// fields.
var gelfSpecified = map[string]bool{"version": true, "host": true, "short_message": true, "full_message": true,
	"timestamp": true, "level": true, "facility": true, "line": true, "file": true}

// gelfSent are the keys of a message of this server, no more and no fewer,
// in their sorted order.
var gelfSent = []string{"_kind", "_line", "_logger", "host", "level", "short_message", "timestamp", "version"}

// decodeGELF reads one message by the JSON schema of GELF 1.1 and the fields
// of this server; an error names the rule it breaks. It reads each key by
// its exact name.
func decodeGELF(data []byte) (gelfTaken, error) {
	m := gelfTaken{size: len(data)}
	if kind := compression(data); kind != "" {
		return m, refusal("a payload compressed with " + kind + ": the server sends no compression")
	}
	if !utf8.Valid(data) || !json.Valid(data) {
		return m, refusal("not one JSON text in UTF-8")
	}
	fields, err := jsonFields(data)
	if err != nil {
		return m, err
	}
	keys := slices.Sorted(maps.Keys(fields))
	for _, key := range keys {
		raw := fields[key]
		switch {
		case key == "_id":
			return m, refusal("the additional field _id")
		case gelfSpecified[key]:
		case !gelfFieldName.MatchString(key):
			return m, refusal(fmt.Sprintf("the key %q, neither a field of GELF 1.1 nor _ with word characters, dots and dashes", key))
		case raw[0] != '"' && !jsonNumber(raw):
			return m, refusal(fmt.Sprintf("the additional field %s, neither a string nor a number", key))
		}
	}
	version, ok := jsonText(fields, "version")
	if !ok || version != "1.1" {
		return m, refusal(`version is not the string "1.1"`)
	}
	host, ok := jsonText(fields, "host")
	if !ok || ecmaBlank(host) {
		return m, refusal("host is not a string with a character other than white space")
	}
	short, ok := jsonText(fields, "short_message")
	if !ok || ecmaBlank(short) {
		return m, refusal("short_message is not a string with a character other than white space")
	}
	for _, key := range []string{"full_message", "facility", "file"} {
		if _, present := fields[key]; present {
			if _, ok := jsonText(fields, key); !ok {
				return m, refusal(key + " is not a string")
			}
		}
	}
	if raw, present := fields["timestamp"]; present {
		if v, err := strconv.ParseFloat(string(raw), 64); !jsonNumber(raw) || err != nil || v <= 0 {
			return m, refusal("timestamp is not a number above 0")
		}
	}
	if raw, present := fields["level"]; present {
		if n, err := strconv.Atoi(string(raw)); err != nil || n < 0 || n > 7 {
			return m, refusal("level is not an integer of 0 to 7")
		}
	}
	if raw, present := fields["line"]; present {
		if n, err := strconv.Atoi(string(raw)); err != nil || n < 0 {
			return m, refusal("line is not an integer of 0 or more")
		}
	}
	// The fields of this server.
	if !slices.Equal(keys, gelfSent) {
		return m, refusal(fmt.Sprintf("the keys %v, not the keys of this server", keys))
	}
	m.timestamp = string(fields["timestamp"])
	if !gelfTime.MatchString(m.timestamp) {
		return m, refusal("timestamp " + m.timestamp + " is not seconds with three decimals")
	}
	m.level, _ = strconv.Atoi(string(fields["level"]))
	kind, okKind := jsonText(fields, "_kind")
	logger, okLogger := jsonText(fields, "_logger")
	line, okLine := jsonText(fields, "_line")
	if !okKind || !lineKinds[kind] || !okLogger || !okLine {
		return m, refusal("_kind is not a kind of line, or _logger or _line is not a string")
	}
	var head map[string]any
	if json.Unmarshal([]byte(line), &head) != nil || !headIn(head) {
		return m, refusal("_line is not a JSON object with time, level, logger and msg by these names")
	}
	want := head["msg"].(string)
	if ecmaBlank(want) {
		want = "-"
	}
	at, err := time.Parse(time.RFC3339Nano, head["time"].(string))
	sev, known := severityByName[head["level"].(string)]
	switch {
	case host != stubHost:
		return m, refusal(fmt.Sprintf("host %q, not %q", host, stubHost))
	case short != want:
		return m, refusal("short_message is not the msg of _line")
	case logger != head["logger"]:
		return m, refusal("_logger is not the logger of _line")
	case !known || m.level != sev:
		return m, refusal(fmt.Sprintf("level %d for the level %v of _line", m.level, head["level"]))
	case err != nil || m.timestamp != fmt.Sprintf("%d.%03d", at.Unix(), at.Nanosecond()/1_000_000):
		return m, refusal(fmt.Sprintf("timestamp %s for the time %v of _line", m.timestamp, head["time"]))
	}
	m.host, m.short, m.kind, m.logger, m.line = host, short, kind, logger, line
	m.id, _ = lineIDOf([]byte(line))
	return m, nil
}

// jsonFields reads the keys of one JSON object by their exact names and
// refuses a key that comes twice.
func jsonFields(data []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, refusal("not a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		key, isKey := tok.(string)
		var raw json.RawMessage
		if err != nil || !isKey || dec.Decode(&raw) != nil {
			return nil, refusal("a JSON object that cannot be read")
		}
		if _, twice := fields[key]; twice {
			return nil, refusal(fmt.Sprintf("the key %q twice", key))
		}
		fields[key] = raw
	}
	return fields, nil
}

// jsonText is the value of key in fields when it is a string.
func jsonText(fields map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := fields[key]
	var v string
	if !ok || raw[0] != '"' || json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	return v, true
}

// jsonNumber reports whether a JSON value is a number.
func jsonNumber(raw json.RawMessage) bool {
	return len(raw) > 0 && (raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9')
}

// ecmaBlank reports whether text holds only the white space and the line
// terminators of ECMAScript, the \s of the pattern \S that the schema gives
// host and short_message.
func ecmaBlank(text string) bool {
	return !strings.ContainsFunc(text, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u2028', '\u2029', '\ufeff':
			return false
		}
		return !unicode.Is(unicode.Zs, r)
	})
}

// The 1,000 hostile lines arrive over udp, tcp and http as 1,000 messages
// with the ids that were sent: _line is the line byte for byte, _kind its
// kind, level the severity of its level, short_message its msg. Over udp
// exactly the messages above 8,192 bytes come in chunks, line 500 among
// them, and the run is paced, ten lines at a time on loopback, as a burst of
// datagrams can outrun the stub. Over tcp the lines arrive in their order
// on one connection kept between the batches; over http, an address without
// a path, they arrive in their order at /gelf, one POST each.
func TestGELFThousandHostileLines(t *testing.T) {
	lines := hostileLines(t)
	want := idSet(t, lines)
	order := make([]string, len(lines))
	sent := map[string]logs.Line{}
	for i, l := range lines {
		order[i], _ = lineIDOf(l.JSON)
		sent[order[i]] = l
	}
	for _, scheme := range []string{"udp", "tcp", "http"} {
		t.Run(scheme, func(t *testing.T) {
			stub := newGELFStub(t, scheme, "/gelf", "")
			row := map[string]any{"name": "demo", "kind": "gelf", "address": stub.address()}
			if scheme == "udp" {
				row["batch_lines"] = 10
			}
			var set setting
			set.put(t, row)
			var log testLog
			m := startManager(t, newFakeClock(), &set, &log)
			statsOf(t, m, "demo")
			if scheme == "udp" {
				shipPaced(t, m, lines, 10, func() int { return len(stub.messages()) })
			} else {
				shipAll(t, m, lines)
				waitFor(t, func() bool { return len(stub.messages()) >= len(lines) }, "1,000 messages decoded")
			}
			if refused := stub.refusals(); len(refused) > 0 {
				t.Fatalf("the stub refused %d messages, the first: %s", len(refused), refused[0])
			}
			got := stub.messages()
			if len(got) != len(lines) {
				t.Fatalf("%d messages decoded, %d lines sent", len(got), len(lines))
			}
			ids := map[string]bool{}
			var chunked []string
			for i, msg := range got {
				line, ok := sent[msg.id]
				switch {
				case !ok:
					t.Fatalf("message %d holds the id %q, which was not sent", i, msg.id)
				case scheme != "udp" && msg.id != order[i]:
					t.Fatalf("message %d holds %s, line %d is %s", i, msg.id, i, order[i])
				case msg.line != string(line.JSON):
					t.Fatalf("%s: _line %q, sent %q", msg.id, msg.line, line.JSON)
				case msg.kind != line.Kind || msg.level != severityOf[line.Level] || msg.short != line.Message:
					t.Fatalf("%s: _kind %s, level %d, short_message %q for a %s line at %v", msg.id, msg.kind, msg.level, msg.short,
						line.Kind, line.Level)
				case scheme == "udp" && ((msg.chunks > 0) != (msg.size > 8192) || msg.chunks > 128):
					t.Fatalf("%s: a message of %d bytes in %d chunks", msg.id, msg.size, msg.chunks)
				}
				if msg.chunks > 0 {
					chunked = append(chunked, fmt.Sprintf("%s of %d bytes in %d chunks", msg.id, msg.size, msg.chunks))
				}
				ids[msg.id] = true
			}
			if len(ids) != len(want) {
				t.Fatalf("%d ids decoded, %d sent", len(ids), len(want))
			}
			waitCounted(t, m, "demo", len(lines))
			st := statsOf(t, m, "demo")
			if st.Sent != 1000 || st.Failed != 0 || st.Errors != 0 {
				t.Fatalf("%+v", st)
			}
			accepted, _ := stub.connections()
			t.Logf("%d messages in %d attempts, %d connections, %d requests; chunked: %v", len(got), st.Attempts, accepted,
				stub.requestCount(), chunked)
			switch scheme {
			case "udp":
				if !slices.ContainsFunc(chunked, func(c string) bool { return strings.HasPrefix(c, "hostile-0500 ") }) ||
					st.Attempts != 100 || stub.pending() != 0 {
					t.Fatalf("chunked %v in %d attempts, %d incomplete; want line 500 among them in 100", chunked, st.Attempts, stub.pending())
				}
			case "tcp":
				if accepted != 1 || st.Attempts != 2 {
					t.Fatalf("%d connections and %d attempts, want 1 and 2", accepted, st.Attempts)
				}
			case "http":
				if stub.requestCount() != 1000 || st.Attempts != 2 {
					t.Fatalf("%d requests and %d attempts, want 1,000 and 2", stub.requestCount(), st.Attempts)
				}
			}
		})
	}
}

// Over udp a message that needs more than 128 chunks of 8,192 bytes cannot
// be sent: its line counts as failed and no datagram of it leaves. A message
// of about 1,000,000 bytes arrives in at most 128 chunks.
func TestGELFUDPLimitsAMessageTo128Chunks(t *testing.T) {
	stub := newGELFStub(t, "udp", "", "")
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "gelf", "address": stub.address(), "batch_lines": 1})
	var log testLog
	m := startManager(t, newFakeClock(), &set, &log)
	statsOf(t, m, "demo")
	m.Ship(paddedLine("server", 1, 1_060_000))
	waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == 1 }, "the line above 128 chunks failed")
	fits := paddedLine("server", 2, 1_000_000)
	m.Ship(fits)
	waitFor(t, func() bool { return len(stub.messages()) == 1 }, "the line within 128 chunks")
	got := stub.messages()[0]
	if got.id != "server-0002" || got.line != string(fits.JSON) || got.chunks < 100 || got.chunks > 128 {
		t.Fatalf("%s of %d bytes in %d chunks", got.id, got.size, got.chunks)
	}
	if refused, pending := stub.refusals(), stub.pending(); len(refused) > 0 || pending > 0 {
		t.Fatalf("refused %v, %d messages incomplete", refused, pending)
	}
	waitCounted(t, m, "demo", 2)
	if st := statsOf(t, m, "demo"); st.Failed != 1 || st.Sent != 1 || st.Errors != 0 {
		t.Fatalf("%+v", st)
	}
	t.Logf("a message of %d bytes arrived in %d chunks", got.size, got.chunks)
}

// Over udp a message of 8,192 bytes or less is one datagram and a longer one
// comes in chunks: of lines whose messages lie a few hundred bytes below and
// above 8,192 bytes, the stub takes the one below whole and the one above in
// 2 chunks, each of them within 8,192 bytes.
func TestGELFUDPChunksOnlyAbove8192Bytes(t *testing.T) {
	stub := newGELFStub(t, "udp", "", "")
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "gelf", "address": stub.address(), "batch_lines": 1})
	var log testLog
	m := startManager(t, newFakeClock(), &set, &log)
	statsOf(t, m, "demo")
	lines := []logs.Line{paddedLine("server", 1, 7_600), paddedLine("server", 2, 8_000)}
	shipPaced(t, m, lines, 1, func() int { return len(stub.messages()) })
	if refused := stub.refusals(); len(refused) > 0 {
		t.Fatalf("the stub refused %v", refused)
	}
	got := stub.messages()
	below, above := got[0], got[1]
	if below.size > 8192 || below.size < 7_600 || below.chunks != 0 || above.size <= 8192 || above.size > 2*8180 || above.chunks != 2 {
		t.Fatalf("messages of %d bytes in %d chunks and of %d bytes in %d chunks", below.size, below.chunks, above.size, above.chunks)
	}
	t.Logf("a message of %d bytes in one datagram, one of %d bytes in %d chunks", below.size, above.size, above.chunks)
}

// messageLine is the server line n at INFO with the message msg.
func messageLine(n int, msg string) logs.Line {
	l := testLine("server", n)
	quoted, _ := json.Marshal(msg)
	l.JSON = []byte(strings.Replace(string(l.JSON), `"msg":"line server-`, `"msg":`+string(quoted)+`,"was":"line server-`, 1))
	l.Message = msg
	return l
}

// A line whose msg is empty or holds only white space is sent with the
// short_message "-", as the schema does not allow a blank one.
func TestGELFBlankMessage(t *testing.T) {
	stub := newGELFStub(t, "http", "/gelf", "")
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "gelf", "address": stub.address(), "batch_lines": 3})
	var log testLog
	m := startManager(t, newFakeClock(), &set, &log)
	statsOf(t, m, "demo")
	lines := []logs.Line{messageLine(1, ""), messageLine(2, "  "), messageLine(3, "\xc2\xa0\t")}
	shipAll(t, m, lines)
	waitFor(t, func() bool { return stub.seen() == 3 }, "3 messages")
	if refused := stub.refusals(); len(refused) > 0 {
		t.Fatalf("the stub refused %v", refused)
	}
	for i, msg := range stub.messages() {
		if msg.short != "-" || msg.line != string(lines[i].JSON) {
			t.Fatalf("message %d: short_message %q for the line %s", i, msg.short, msg.line)
		}
	}
}

// The secret reaches the stub as Bearer, and with a user as HTTP Basic over
// https; neither secret, nor the query of the address, is in a log line of
// the manager, its warning with the class of the failure among them.
func TestGELFSecretReachesOnlyTheStub(t *testing.T) {
	const token, password, query = "canary-token-3a7c91e4", "canary-password-b85d02f6", "canary-query-5e1a9c"
	cases := []struct {
		name, user, secret string
		https              bool
	}{
		{"bearer", "", token, false},
		{"basic", "demo-admin", password, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			auth, scheme := "Bearer "+c.secret, "http"
			if c.user != "" {
				auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(c.user+":"+c.secret))
			}
			if c.https {
				scheme = "https"
			}
			stub := newGELFStub(t, scheme, "/gelf", auth)
			stub.setStatus(func(n int) int {
				if n == 1 {
					return http.StatusServiceUnavailable
				}
				return 0
			}, "")
			row := map[string]any{"name": "demo", "kind": "gelf", "address": stub.address() + "/gelf?token=" + query,
				"secret": "log-target-demo", "batch_lines": 3}
			if c.https {
				row["ca_file"], row["user"] = stub.caFile, c.user
			}
			var set setting
			set.put(t, row)
			set.putSecret("log-target-demo", c.secret)
			clk := newFakeClock()
			var log testLog
			m := startManager(t, clk, &set, &log)
			statsOf(t, m, "demo")
			shipAll(t, m, testLines("server", 0, 3))
			clk.waitPending(t, 500*time.Millisecond)
			clk.Advance(500 * time.Millisecond)
			waitFor(t, func() bool { return len(stub.messages()) == 3 }, "the lines taken with the secret")
			m.Close()
			if warns := log.records(t, "log target failing"); len(warns) != 1 || warns[0]["error"] != "status 503" {
				t.Fatalf("warnings %v", warns)
			}
			noCanary(t, "the log of the manager", log.text(), c.secret, query, auth[strings.Index(auth, " ")+1:])
		})
	}
}

// Each retry rule of GELF over http once: 429, 502, 503 and 504 send the line
// again and it counts as sent; every other status outside 2xx, 3xx among
// them, drops it after one attempt; Retry-After sets the pause. One POST per
// line: a failure at the second line of a batch leaves the first sent, and
// the next attempt starts at the second. A refused connection is tried 3
// times and fails as dial, over http and over tcp; a receiver that ends a
// tcp connection gets it closed and the next batch goes over a new one; over
// udp a local write error fails the attempt as write.
func TestGELFRetryRules(t *testing.T) {
	retried := []int{429, 502, 503, 504}
	dropped := []int{301, 302, 307, 308, 400, 401, 403, 404, 405, 409, 413, 415, 500, 501}
	start := func(t *testing.T, stub *gelfStub, batch int) (*Manager, *fakeClock) {
		t.Helper()
		var set setting
		set.put(t, map[string]any{"name": "demo", "kind": "gelf", "address": stub.address(), "batch_lines": batch})
		clk := newFakeClock()
		var log testLog
		m := startManager(t, clk, &set, &log)
		statsOf(t, m, "demo")
		return m, clk
	}
	idsAt := func(stub *gelfStub) []string {
		var out []string
		for _, msg := range stub.messages() {
			out = append(out, msg.id)
		}
		return out
	}
	for _, code := range append(append([]int(nil), retried...), dropped...) {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			stub := newGELFStub(t, "http", "/gelf", "")
			stub.setStatus(func(n int) int {
				if n == 1 {
					return code
				}
				return 0
			}, "")
			m, clk := start(t, stub, 1)
			m.Ship(testLine("tasks", 1))
			if slices.Contains(retried, code) {
				clk.waitPending(t, 500*time.Millisecond)
				clk.Advance(500 * time.Millisecond)
				waitFor(t, func() bool { return statsOf(t, m, "demo").Sent == 1 }, "the line sent again")
				if st := statsOf(t, m, "demo"); st.Attempts != 2 || st.Errors != 1 || st.Failed != 0 || len(stub.messages()) != 1 {
					t.Fatalf("%+v with %d messages taken", st, len(stub.messages()))
				}
				return
			}
			waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == 1 }, "the line dropped")
			if st := statsOf(t, m, "demo"); st.Attempts != 1 || st.Sent != 0 || stub.requestCount() != 1 {
				t.Fatalf("%+v after %d requests", st, stub.requestCount())
			}
		})
	}
	t.Run("Retry-After", func(t *testing.T) {
		stub := newGELFStub(t, "http", "/gelf", "")
		stub.setStatus(func(n int) int {
			if n == 1 {
				return http.StatusTooManyRequests
			}
			return 0
		}, "2")
		m, clk := start(t, stub, 1)
		m.Ship(testLine("server", 1))
		clk.waitPending(t, 2*time.Second)
		if slices.Contains(clk.pending(), 500*time.Millisecond) {
			t.Fatalf("the pause of 500 ms is asked for: %v", clk.pending())
		}
		clk.Advance(2 * time.Second)
		waitFor(t, func() bool { return statsOf(t, m, "demo").Sent == 1 }, "the line sent after Retry-After")
	})
	t.Run("one POST per line", func(t *testing.T) {
		stub := newGELFStub(t, "http", "/gelf", "")
		stub.setStatus(func(n int) int {
			if n == 2 {
				return http.StatusServiceUnavailable
			}
			return 0
		}, "")
		m, clk := start(t, stub, 3)
		shipAll(t, m, testLines("server", 0, 3))
		clk.waitPending(t, 500*time.Millisecond)
		clk.Advance(500 * time.Millisecond)
		waitFor(t, func() bool { return statsOf(t, m, "demo").Sent == 3 }, "the batch sent")
		st := statsOf(t, m, "demo")
		if got := idsAt(stub); !slices.Equal(got, []string{"server-0000", "server-0001", "server-0002"}) ||
			stub.requestCount() != 4 || st.Attempts != 2 || st.Errors != 1 {
			t.Fatalf("taken %v in %d requests, %+v", got, stub.requestCount(), st)
		}
	})
	t.Run("final at the second line", func(t *testing.T) {
		stub := newGELFStub(t, "http", "/gelf", "")
		stub.setStatus(func(n int) int {
			if n == 2 {
				return http.StatusBadRequest
			}
			return 0
		}, "")
		m, _ := start(t, stub, 3)
		shipAll(t, m, testLines("server", 0, 3))
		waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == 2 }, "the rest of the batch dropped")
		st := statsOf(t, m, "demo")
		if got := idsAt(stub); !slices.Equal(got, []string{"server-0000"}) || stub.requestCount() != 2 ||
			st.Sent != 1 || st.Attempts != 1 || st.Errors != 1 {
			t.Fatalf("taken %v in %d requests, %+v", got, stub.requestCount(), st)
		}
	})
	t.Run("refused http", func(t *testing.T) { checkRefusedDial(t, "gelf", "http") })
	t.Run("refused tcp", func(t *testing.T) { checkRefusedDial(t, "gelf", "tcp") })
	t.Run("closed tcp", func(t *testing.T) {
		stub := newGELFStub(t, "tcp", "", "")
		stub.setCloseAfter(3)
		m, _ := start(t, stub, 3)
		shipAll(t, m, testLines("server", 0, 3))
		waitFor(t, func() bool { _, closed := stub.connections(); return closed == 1 }, "the sender to close the connection the stub ended")
		shipAll(t, m, testLines("server", 3, 3))
		waitFor(t, func() bool { return len(stub.messages()) == 6 }, "the next batch on a new connection")
		for i, msg := range stub.messages() {
			if want := fmt.Sprintf("server-%04d", i); msg.id != want || msg.conn != 1+i/3 {
				t.Fatalf("message %d is %s on connection %d, want %s on %d", i, msg.id, msg.conn, want, 1+i/3)
			}
		}
		waitCounted(t, m, "demo", 6)
		if st := statsOf(t, m, "demo"); st.Sent != 6 || st.Attempts != 2 || st.Errors != 0 {
			t.Fatalf("%+v", st)
		}
	})
	t.Run("udp", func(t *testing.T) { checkUDPWriteError(t, "gelf") })
}

// gelfText is a message of this server for line, written by hand from the
// rules above, with extra at the end of the object.
func gelfText(line logs.Line, extra string) string {
	quote := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}
	at := line.Time.UTC()
	return `{"version":"1.1","host":"demo-host","short_message":` + quote(line.Message) +
		fmt.Sprintf(`,"timestamp":%d.%03d,"level":%d`, at.Unix(), at.Nanosecond()/1_000_000, severityOf[line.Level]) +
		`,"_kind":` + quote(line.Kind) + `,"_logger":` + quote(line.Logger) + `,"_line":` + quote(string(line.JSON)) + extra + "}"
}

// gelfChunk is one chunk: the magic bytes, the message id of 8 bytes id,
// the sequence number and the count, then part.
func gelfChunk(id, seq, count byte, part string) string {
	return string([]byte{0x1e, 0x0f, id, id, id, id, id, id, id, id, seq, count}) + part
}

// compress is text compressed by GZIP or by ZLIB.
func compress(t *testing.T, kind, text string) string {
	t.Helper()
	var b bytes.Buffer
	var w io.WriteCloser = gzip.NewWriter(&b)
	if kind == "ZLIB" {
		w = zlib.NewWriter(&b)
	}
	if _, err := io.WriteString(w, text); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The stub refuses what GELF 1.1 and its JSON schema do not allow and what
// this server does not send, one bad message each, sent to it directly over
// udp, tcp and http, each refused for its own rule; it takes the good
// messages, also one in chunks built by hand.
func TestGELFStubRefuses(t *testing.T) {
	line := testLine("server", 1)
	good := gelfText(line, "")
	other := func(from, to string) string { return strings.Replace(good, from, to, 1) }
	big := gelfText(paddedLine("server", 2, 12_000), "") // two chunks of 8,180 bytes at most
	if len(big) <= 8180 || len(big) > 2*8180 {
		t.Fatalf("a message of %d bytes for two chunks", len(big))
	}
	payloads := []struct{ name, text, want string }{
		{"version as a number", other(`"version":"1.1"`, `"version":1.1`), "version"},
		{"version 1.0", other(`"version":"1.1"`, `"version":"1.0"`), "version"},
		{"no short_message", other(`"short_message":"line server-0001",`, ""), "short_message"},
		{"a blank host", other(`"host":"demo-host"`, `"host":" \u00a0"`), "host"},
		{"a key without the underscore", gelfText(line, `,"foo":"bar"`), `"foo"`},
		{"the additional field _id", gelfText(line, `,"_id":"1"`), "_id"},
		{"a space in a field name", gelfText(line, `,"_a b":"1"`), `"_a b"`},
		{"a boolean additional field", gelfText(line, `,"_ok":true`), "_ok"},
		{"a null additional field", gelfText(line, `,"_ok":null`), "_ok"},
		{"level 8", other(`"level":6`, `"level":8`), "level"},
		{"level as a string", other(`"level":6`, `"level":"6"`), "level"},
		{"timestamp as a string", other(`"timestamp":1795082400.001`, `"timestamp":"1795082400.001"`), "timestamp"},
		{"a key twice", gelfText(line, `,"host":"demo-host"`), "twice"},
		{"keys in another case", strings.NewReplacer(`"version"`, `"Version"`, `"host"`, `"Host"`, `"short_message"`,
			`"Short_Message"`).Replace(good), `"Host"`},
		{"not JSON", strings.TrimSuffix(good, "}"), "JSON"},
		{"not UTF-8", other(`"host":"demo-host"`, "\"host\":\"demo-h\xffst\""), "UTF-8"},
		{"two messages", good + good, "JSON"},
		{"a list", "[1]", "JSON object"},
		{"a field this server does not send", gelfText(line, `,"_extra":"1"`), "keys"},
		{"full_message", gelfText(line, `,"full_message":"x"`), "keys"},
		{"short_message other than msg", other(`"short_message":"line server-0001"`, `"short_message":"another"`), "short_message"},
		{"timestamp with four decimals", other(`1795082400.001`, `1795082400.0010`), "timestamp"},
		{"level of another severity", other(`"level":6`, `"level":4`), "level"},
		{"_kind that is no kind", other(`"_kind":"server"`, `"_kind":"debug"`), "_kind"},
		{"GZIP", compress(t, "GZIP", good), "GZIP"},
		{"ZLIB", compress(t, "ZLIB", good), "ZLIB"},
	}
	t.Run("udp", func(t *testing.T) {
		stub := newGELFStub(t, "udp", "", "")
		conn, err := net.Dial("udp", strings.TrimPrefix(stub.address(), "udp://"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		var wants []string
		taken := 0
		send := func(name, want string, datagrams ...string) {
			t.Helper()
			for _, d := range datagrams {
				if _, err := io.WriteString(conn, d); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			}
			switch want {
			case "":
				taken++
			case "pending":
			default:
				wants = append(wants, want)
			}
			waitFor(t, func() bool { return stub.seen() == taken+len(wants) }, name)
		}
		send("good", "", good)
		send("good in chunks", "", gelfChunk(1, 0, 2, big[:8180]), gelfChunk(1, 1, 2, big[8180:]))
		for _, c := range payloads {
			send(c.name, c.want, c.text)
		}
		send("a datagram above 8,192 bytes, not in chunks", "8,192", gelfText(paddedLine("server", 1, 9000), ""))
		send("a chunk above 8,192 bytes", "8,192", gelfChunk(2, 0, 2, strings.Repeat("x", 8181)))
		send("a sequence number equal to the count", "sequence number", gelfChunk(3, 2, 2, good))
		send("a sequence count above 128", "sequence count", gelfChunk(4, 0, 129, good))
		send("a sequence count of 0", "sequence count", gelfChunk(5, 0, 0, good))
		send("a message of 8,192 bytes or less in chunks", "above 8,192 only", gelfChunk(6, 0, 2, good[:100]), gelfChunk(6, 1, 2, good[100:]))
		send("an incomplete message", "pending", gelfChunk(7, 0, 2, big[:8180]))
		send("counts that differ", "sequence count of 3 after 2", gelfChunk(8, 0, 2, big[:8180]), gelfChunk(8, 1, 3, big[8180:]))
		send("a chunk twice", "twice", gelfChunk(9, 0, 2, big[:8180]), gelfChunk(9, 0, 2, big[:8180]))
		checkRefusals(t, stub.refusals(), wants)
		if got := stub.messages(); len(got) != 2 || got[0].id != "server-0001" || got[1].id != "server-0002" || got[1].chunks != 2 {
			t.Fatalf("the stub took %+v, want the good message and the good message in chunks", got)
		}
		if n := stub.pending(); n != 3 {
			t.Fatalf("%d incomplete messages, want those of the ids 7, 8 and 9", n)
		}
	})
	t.Run("tcp", func(t *testing.T) {
		stub := newGELFStub(t, "tcp", "", "")
		frames := []struct {
			name, text string
			want       []string
		}{
			{"good frames", good + "\x00" + gelfText(testLine("server", 2), "") + "\x00", nil},
			{"newline framing", good + "\n" + good + "\n", []string{"without the null byte"}},
			{"a line break before the null byte", good + "\n\x00", []string{"line break"}},
			{"a frame in GZIP", compress(t, "GZIP", good) + "\x00", []string{"GZIP"}},
			{"a chunk in a frame", gelfChunk(1, 0, 1, good) + "\x00", []string{"chunk"}},
			{"two messages in one frame", good + good + "\x00", []string{"JSON"}},
			{"a frame in another case", strings.NewReplacer(`"host"`, `"Host"`).Replace(good) + "\x00", []string{`"Host"`}},
			{"no null byte at the end", good, []string{"without the null byte"}},
		}
		var wants []string
		for _, f := range frames {
			conn, err := net.Dial("tcp", strings.TrimPrefix(stub.address(), "tcp://"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(conn, f.text); err != nil {
				t.Fatalf("%s: %v", f.name, err)
			}
			_ = conn.(*net.TCPConn).CloseWrite()
			wants = append(wants, f.want...)
			waitFor(t, func() bool { return stub.seen() == 2+len(wants) }, f.name) // the 2 good messages come first
			_ = conn.Close()
		}
		checkRefusals(t, stub.refusals(), wants)
		if got := stub.messages(); len(got) != 2 || got[0].id != "server-0001" || got[1].id != "server-0002" {
			t.Fatalf("the stub took %+v, want the two good messages", got)
		}
	})
	t.Run("http", func(t *testing.T) {
		stub := newGELFStub(t, "http", "/gelf", "Bearer demo-token")
		agent := "pipesmith-registry/" + testVersion
		cases := []struct {
			name, method, path, contentType, encoding, agent, auth, body string
			want                                                         int
		}{
			{"good", "POST", "/gelf", "application/json", "", agent, "Bearer demo-token", good, 202},
			{"method", "GET", "/gelf", "application/json", "", agent, "Bearer demo-token", good, 405},
			{"path", "POST", "/other", "application/json", "", agent, "Bearer demo-token", good, 404},
			{"content type", "POST", "/gelf", "text/plain", "", agent, "Bearer demo-token", good, 415},
			{"no content type", "POST", "/gelf", "", "", agent, "Bearer demo-token", good, 415},
			{"compressed", "POST", "/gelf", "application/json", "gzip", agent, "Bearer demo-token", compress(t, "GZIP", good), 415},
			{"user agent", "POST", "/gelf", "application/json", "", "demo-client/1.0", "Bearer demo-token", good, 400},
			{"no secret", "POST", "/gelf", "application/json", "", agent, "", good, 401},
			{"other secret", "POST", "/gelf", "application/json", "", agent, "Bearer other-token", good, 401},
			{"two messages", "POST", "/gelf", "application/json", "", agent, "Bearer demo-token", good + good, 400},
			{"two messages on two lines", "POST", "/gelf", "application/json", "", agent, "Bearer demo-token", good + "\n" + good, 400},
			{"keys in another case", "POST", "/gelf", "application/json", "", agent, "Bearer demo-token",
				strings.NewReplacer(`"host"`, `"Host"`).Replace(good), 400},
			{"empty body", "POST", "/gelf", "application/json", "", agent, "Bearer demo-token", "", 400},
			{"a list", "POST", "/gelf", "application/json", "", agent, "Bearer demo-token", "[1]", 400},
		}
		for _, c := range cases {
			req, err := http.NewRequest(c.method, stub.address()+c.path, strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			for key, value := range map[string]string{"Content-Type": c.contentType, "Content-Encoding": c.encoding,
				"User-Agent": c.agent, "Authorization": c.auth} {
				if value != "" {
					req.Header.Set(key, value)
				}
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Errorf("%s: %d, want %d", c.name, resp.StatusCode, c.want)
			}
		}
		if got := stub.messages(); len(got) != 1 || got[0].id != "server-0001" {
			t.Fatalf("the stub took %+v, want the good message", got)
		}
	})
}

// checkRefusals fails the test unless each refusal names its rule, in order.
func checkRefusals(t *testing.T, refused, wants []string) {
	t.Helper()
	if len(refused) != len(wants) {
		t.Fatalf("%d refusals, want %d: %q", len(refused), len(wants), refused)
	}
	for i, reason := range refused {
		if !strings.Contains(reason, wants[i]) {
			t.Errorf("refusal %d: %q, want %q in it", i+1, reason, wants[i])
		}
	}
}
