package logship

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The rules of syslog, read at https://www.rfc-editor.org on 2026-11-19:
// RFC 5424, section 6 (the message: PRI of 0 to 191 without a leading zero,
// VERSION 1, a TIMESTAMP with an upper case T and Z and at most six
// decimals, HOSTNAME up to 255, APP-NAME up to 48, PROCID up to 128 and
// MSGID up to 32 printable US-ASCII characters, STRUCTURED-DATA, and a MSG
// in UTF-8 that starts with the byte order mark EF BB BF); RFC 5426, section
// 3 (one message per UDP datagram, which every receiver should take up to
// 2,048 octets); RFC 6587, section 3.4.1 (octet counting over TCP: MSG-LEN
// SP SYSLOG-MSG, MSG-LEN a decimal that starts with 1 to 9); RFC 5425,
// sections 4.3, 4.4 and 5.2 (the same framing over TLS, close_notify before
// the close, the host name checked against the subjectAltName of the
// certificate); RFC 9662, section 4 (TLS 1.2 at least, TLS 1.3 preferred).
// The choices of this server on top: facility local0, severity DEBUG 7,
// INFO 6, WARN 4 and ERROR 3, APP-NAME pipesmith-registry, PROCID the
// process id, MSGID the kind of the line, no STRUCTURED-DATA, a TIMESTAMP in
// UTC with milliseconds, and a message over UDP above 2,048 octets cut at
// the start of a character. Not built: the certificate fingerprints of RFC
// 5425, section 5.1, and the check of the common name that section 5.2 asks
// for as a SHOULD (Go checks the subjectAltName only). unverified: whether
// receivers keep the byte order mark in front of a JSON line.

// stubHost is the host name the manager of a test sends (options in
// harness_test.go).
const stubHost = "demo-host"

// msgBOM is the byte order mark that starts a MSG in UTF-8.
const msgBOM = "\xef\xbb\xbf"

// severityOf is the syslog severity of each level of a line, the choice of
// this server: RFC 5424 names no mapping.
var severityOf = map[slog.Level]int{slog.LevelDebug: 7, slog.LevelInfo: 6, slog.LevelWarn: 4, slog.LevelError: 3}

// severityByName is severityOf by the level names of a JSON line.
var severityByName = map[string]int{"DEBUG": 7, "INFO": 6, "WARN": 4, "ERROR": 3}

// lineKinds are the kinds of line: the MSGID of a syslog message, the _kind
// of a GELF message.
var lineKinds = map[string]bool{"server": true, "requests": true, "audit": true, "tasks": true}

// refusal is a rule of the protocol that a stub found broken.
type refusal string

func (r refusal) Error() string { return string(r) }

// syslogStub is a receiver of syslog over udp, tcp or tls. It reads each
// message with parseSyslog and keeps what it took and why it refused the
// rest. A broken frame over tcp or tls closes the connection, as the
// receiver cannot find the next frame; a bad datagram, or a bad message in
// a good frame, is dropped.
type syslogStub struct {
	pc     net.PacketConn
	ln     net.Listener
	scheme string
	caFile string // tls: the authority of the certificate of the stub
	wg     sync.WaitGroup

	mu         sync.Mutex
	closeAfter int // tcp and tls: the stub ends a connection after this many messages; 0 keeps it
	open       []net.Conn
	took       []syslogMessage
	refused    []string
	conns      int      // connections accepted
	closed     int      // connections the stub ended that the sender closed in turn
	versions   []uint16 // tls: the version of each handshake
	handshakes []string // tls: the handshakes that failed
}

// syslogMessage is one message a stub took.
type syslogMessage struct {
	pri       int
	timestamp string
	msgID     string
	msg       []byte // MSG after the byte order mark
	size      int    // the octets of SYSLOG-MSG
	cut       bool   // a datagram cut at 2,048 octets: msg is the start of its line
	id        string // the id of the line
	conn      int    // tcp and tls: the connection it came on, from 1
}

// newSyslogStub starts a stub of scheme on 127.0.0.1. A tls stub serves
// conf, or without one a certificate of testCA.
func newSyslogStub(t *testing.T, scheme string, conf *tls.Config) *syslogStub {
	t.Helper()
	s := &syslogStub{scheme: scheme}
	if scheme == "udp" {
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
		return s
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if scheme == "tls" {
		if conf == nil {
			var cert tls.Certificate
			s.caFile, cert = testCA(t)
			conf = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		}
		ln = tls.NewListener(ln, conf)
	}
	s.ln = ln
	s.wg.Add(1)
	go s.accept()
	t.Cleanup(s.close)
	return s
}

// readBuffer asks for a receive buffer of 4 MiB, so that a burst of
// datagrams on loopback waits in the socket instead of being lost.
func readBuffer(t *testing.T, pc net.PacketConn) {
	t.Helper()
	if udp, ok := pc.(*net.UDPConn); ok {
		if err := udp.SetReadBuffer(4 << 20); err != nil {
			t.Fatal(err)
		}
	}
}

func (s *syslogStub) address() string {
	if s.pc != nil {
		return "udp://" + s.pc.LocalAddr().String()
	}
	return s.scheme + "://" + s.ln.Addr().String()
}

func (s *syslogStub) setCloseAfter(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeAfter = n
}

func (s *syslogStub) readDatagrams() {
	defer s.wg.Done()
	buf := make([]byte, 65536)
	for {
		n, _, err := s.pc.ReadFrom(buf)
		if err != nil {
			return
		}
		m, err := parseSyslog(append([]byte(nil), buf[:n]...), true)
		if err != nil {
			s.refuse(err.Error())
			continue
		}
		s.take(m)
	}
}

func (s *syslogStub) accept() {
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

// serve reads the frames of connection n.
func (s *syslogStub) serve(conn net.Conn, n int) {
	defer func() { _ = conn.Close() }()
	if tc, ok := conn.(*tls.Conn); ok {
		err := tc.Handshake()
		s.mu.Lock()
		if err != nil {
			s.handshakes = append(s.handshakes, err.Error())
		} else {
			s.versions = append(s.versions, tc.ConnectionState().Version)
		}
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
	r := bufio.NewReader(conn)
	taken := 0
	for {
		frame, err := readOctetFrame(r)
		var broken refusal
		switch {
		case errors.As(err, &broken):
			s.refuse(string(broken)) // the start of the next frame is lost: the connection ends
			return
		case err != nil:
			return // the end of the stream, or of the stub
		}
		m, err := parseSyslog(frame, false)
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

// endConnection is a receiver that ends a connection: it closes its side,
// with close_notify over tls, and reads until the sender has closed its
// side as well.
func endConnection(conn net.Conn, r io.Reader) {
	switch c := conn.(type) {
	case *tls.Conn:
		_ = c.CloseWrite()
	case *net.TCPConn:
		_ = c.CloseWrite()
	}
	_, _ = io.Copy(io.Discard, r)
}

func (s *syslogStub) close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for _, c := range s.open {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *syslogStub) take(m syslogMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.took = append(s.took, m)
}

func (s *syslogStub) refuse(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused = append(s.refused, reason)
}

func (s *syslogStub) messages() []syslogMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]syslogMessage(nil), s.took...)
}

func (s *syslogStub) refusals() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.refused...)
}

// seen is the number of messages taken and refused.
func (s *syslogStub) seen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.took) + len(s.refused)
}

// connections is the number of connections accepted, and of those the stub
// ended after closeAfter messages and the sender closed in turn.
func (s *syslogStub) connections() (accepted, closed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns, s.closed
}

func (s *syslogStub) tlsState() (versions []uint16, failed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint16(nil), s.versions...), append([]string(nil), s.handshakes...)
}

// maxSyslogFrame bounds MSG-LEN in the stub; RFC 6587 sets no bound.
const maxSyslogFrame = 4 << 20

// readOctetFrame reads one frame of octet counting: MSG-LEN SP SYSLOG-MSG,
// MSG-LEN = NONZERO-DIGIT *DIGIT, the count of the octets of SYSLOG-MSG
// (RFC 6587, section 3.4.1). A frame that breaks the rule is a refusal; any
// other error ends the stream.
func readOctetFrame(r *bufio.Reader) ([]byte, error) {
	first, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if first < '1' || first > '9' {
		return nil, refusal(fmt.Sprintf("a frame that starts with %q, not with a digit 1 to 9", first))
	}
	n := int(first - '0')
	for {
		b, err := r.ReadByte()
		switch {
		case err != nil:
			return nil, refusal("a frame that ends inside MSG-LEN")
		case b == ' ':
			frame := make([]byte, n)
			if _, err := io.ReadFull(r, frame); err != nil {
				return nil, refusal(fmt.Sprintf("a frame that ends before its %d octets", n))
			}
			return frame, nil
		case b < '0' || b > '9':
			return nil, refusal(fmt.Sprintf("MSG-LEN holds %q", b))
		}
		if n = n*10 + int(b-'0'); n > maxSyslogFrame {
			return nil, refusal("MSG-LEN above the bound of the stub")
		}
	}
}

var (
	// syslogTimestamp is FULL-DATE "T" FULL-TIME of RFC 5424: one to six
	// decimals, the offset Z or +hh:mm or -hh:mm.
	syslogTimestamp = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?(Z|[+-][0-9]{2}:[0-9]{2})$`)
	// syslogMillisUTC is the TIMESTAMP of this server: milliseconds and Z.
	syslogMillisUTC = regexp.MustCompile(`\.[0-9]{3}Z$`)
)

// parseSyslog reads one SYSLOG-MSG by the ABNF of RFC 5424, section 6, and
// the choices of this server; an error names the rule the message breaks.
// A datagram above 2,048 octets is refused, as the server cuts there. A
// datagram of 2,045 to 2,048 octets whose MSG is the start of a JSON object
// is taken as cut: RFC 5424, section 6.1, lets a message be truncated at
// its end, and a cut at the start of a character takes off up to 3 octets
// more.
func parseSyslog(data []byte, datagram bool) (syslogMessage, error) {
	m := syslogMessage{size: len(data)}
	if datagram && len(data) > 2048 {
		return m, fmt.Errorf("a datagram of %d octets: the server cuts at 2,048", len(data))
	}
	end := bytes.IndexByte(data, '>')
	if len(data) == 0 || data[0] != '<' || end < 2 || end > 4 {
		return m, errors.New("PRI is not < with 1 to 3 digits >")
	}
	prival := string(data[1:end])
	pri, err := strconv.Atoi(prival)
	if err != nil || pri < 0 || pri > 191 || strconv.Itoa(pri) != prival {
		return m, fmt.Errorf("PRIVAL %q is not 0 to 191 without a leading zero", prival)
	}
	// VERSION SP TIMESTAMP SP HOSTNAME SP APP-NAME SP PROCID SP MSGID SP
	// STRUCTURED-DATA SP MSG
	parts := strings.SplitN(string(data[end+1:]), " ", 8)
	if len(parts) < 8 {
		return m, errors.New("a header field, STRUCTURED-DATA or MSG is missing")
	}
	version, timestamp, sd, msg := parts[0], parts[1], parts[6], parts[7]
	fields := []struct {
		name, value, want string
		max               int
	}{
		{"HOSTNAME", parts[2], stubHost, 255},
		{"APP-NAME", parts[3], "pipesmith-registry", 48},
		{"PROCID", parts[4], strconv.Itoa(os.Getpid()), 128},
		{"MSGID", parts[5], "", 32},
	}
	if version != "1" {
		return m, fmt.Errorf("VERSION %q, not 1", version)
	}
	if !syslogTimestamp.MatchString(timestamp) {
		return m, fmt.Errorf("TIMESTAMP %q is not FULL-DATE T FULL-TIME", timestamp)
	}
	at, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return m, fmt.Errorf("TIMESTAMP %q is not a time: %v", timestamp, err)
	}
	for _, f := range fields {
		if f.value == "" || len(f.value) > f.max || strings.ContainsFunc(f.value, func(r rune) bool { return r < 33 || r > 126 }) {
			return m, fmt.Errorf("%s %q is not 1 to %d printable US-ASCII characters", f.name, f.value, f.max)
		}
	}
	// The choices of this server.
	if !syslogMillisUTC.MatchString(timestamp) {
		return m, fmt.Errorf("TIMESTAMP %q is not in milliseconds and UTC", timestamp)
	}
	if pri/8 != 16 {
		return m, fmt.Errorf("facility %d, not local0 (16)", pri/8)
	}
	for _, f := range fields {
		if f.want != "" && f.value != f.want {
			return m, fmt.Errorf("%s %q, not %q", f.name, f.value, f.want)
		}
	}
	if !lineKinds[parts[5]] {
		return m, fmt.Errorf("MSGID %q is not a kind of line", parts[5])
	}
	if sd != "-" {
		return m, fmt.Errorf("STRUCTURED-DATA %q: the server sends none", sd)
	}
	if !strings.HasPrefix(msg, msgBOM) {
		return m, errors.New("MSG without the byte order mark EF BB BF")
	}
	text := []byte(msg[len(msgBOM):])
	if !utf8.Valid(text) {
		return m, errors.New("MSG is not UTF-8")
	}
	m.pri, m.timestamp, m.msgID, m.msg = pri, timestamp, parts[5], text
	if !json.Valid(text) {
		if !datagram || len(data) < 2048-3 || !bytes.HasPrefix(text, []byte(`{"time":`)) {
			return m, errors.New("MSG is not one JSON object")
		}
		id, ok := lineIDIn(text)
		if !ok {
			return m, errors.New("a cut MSG without the id of its line")
		}
		m.cut, m.id = true, id
		return m, nil
	}
	var head map[string]any
	if err := json.Unmarshal(text, &head); err != nil || !headIn(head) {
		return m, errors.New("MSG is not a JSON object with time, level, logger and msg by these names")
	}
	if sev, known := severityByName[head["level"].(string)]; !known || pri%8 != sev {
		return m, fmt.Errorf("severity %d for the level %v of the line", pri%8, head["level"])
	}
	if lineAt, err := time.Parse(time.RFC3339Nano, head["time"].(string)); err != nil || !lineAt.Equal(at) {
		return m, fmt.Errorf("TIMESTAMP %s for the time %v of the line", timestamp, head["time"])
	}
	m.id, _ = lineIDOf(text)
	return m, nil
}

// shipPaced hands lines to the tap in groups of size and waits after each
// group until arrived counts them, so that a burst of datagrams never
// outruns the stub on loopback.
func shipPaced(t *testing.T, m *Manager, lines []logs.Line, size int, arrived func() int) {
	t.Helper()
	for start := 0; start < len(lines); start += size {
		end := min(start+size, len(lines))
		shipAll(t, m, lines[start:end])
		waitFor(t, func() bool { return arrived() >= end }, fmt.Sprintf("%d lines at the stub", end))
	}
}

// The 1,000 hostile lines arrive over udp, tcp and tls as 1,000 messages
// with the ids that were sent, each with the PRI of its level, its kind as
// MSGID and its time as TIMESTAMP. Over tcp and tls every line arrives
// whole, byte for byte and in its order, on one connection that is kept
// between the batches, and tls negotiates TLS 1.3. Over udp, which loses by
// design, the test takes a message that would pass 2,048 octets as cut:
// line 500 holds 100 KiB more and arrives as the start of its line, cut at
// the last character within 2,048 octets and counted in cut; every other
// line arrives whole. The udp run is paced, ten lines at a time on loopback,
// as a burst of datagrams can outrun the stub.
func TestSyslogThousandHostileLines(t *testing.T) {
	lines := hostileLines(t)
	want := idSet(t, lines)
	order := make([]string, len(lines))
	sent := map[string]logs.Line{}
	for i, l := range lines {
		order[i], _ = lineIDOf(l.JSON)
		sent[order[i]] = l
	}
	for _, scheme := range []string{"udp", "tcp", "tls"} {
		t.Run(scheme, func(t *testing.T) {
			stub := newSyslogStub(t, scheme, nil)
			row := map[string]any{"name": "demo", "kind": "syslog", "address": stub.address()}
			if scheme == "tls" {
				row["ca_file"] = stub.caFile
			}
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
			cut := 0
			var cutIDs []string
			for i, msg := range got {
				line, ok := sent[msg.id]
				switch {
				case !ok:
					t.Fatalf("message %d holds the id %q, which was not sent", i, msg.id)
				case scheme != "udp" && msg.id != order[i]:
					t.Fatalf("message %d holds %s, line %d is %s", i, msg.id, i, order[i])
				case msg.msgID != line.Kind || msg.pri != 16*8+severityOf[line.Level]:
					t.Fatalf("%s: MSGID %s and PRI %d for a %s line at %v", msg.id, msg.msgID, msg.pri, line.Kind, line.Level)
				case msg.timestamp != line.Time.UTC().Format("2006-01-02T15:04:05.000Z07:00"):
					t.Fatalf("%s: TIMESTAMP %s for a line of %v", msg.id, msg.timestamp, line.Time)
				}
				ids[msg.id] = true
				head := msg.size - len(msgBOM) - len(msg.msg) // the octets in front of MSG
				if scheme != "udp" || head+len(msgBOM)+len(line.JSON) <= 2048 {
					if msg.cut || !bytes.Equal(msg.msg, line.JSON) {
						t.Fatalf("%s did not arrive whole: %q", msg.id, msg.msg)
					}
					continue
				}
				cut++
				cutIDs = append(cutIDs, fmt.Sprintf("%s of %d octets", msg.id, msg.size))
				_, next := utf8.DecodeRune(line.JSON[min(len(msg.msg), len(line.JSON)):])
				if !msg.cut || !bytes.HasPrefix(line.JSON, msg.msg) || msg.size > 2048 || msg.size+next <= 2048 {
					t.Fatalf("%s: %d octets that are not its line cut at the last character within 2,048 octets", msg.id, msg.size)
				}
			}
			if len(ids) != len(want) {
				t.Fatalf("%d ids decoded, %d sent", len(ids), len(want))
			}
			waitCounted(t, m, "demo", len(lines))
			st := statsOf(t, m, "demo")
			if st.Sent != 1000 || st.Failed != 0 || st.Errors != 0 || st.Cut != int64(cut) {
				t.Fatalf("%+v with %d messages cut", st, cut)
			}
			accepted, _ := stub.connections()
			versions, _ := stub.tlsState()
			t.Logf("%d messages in %d attempts over %d connections; cut: %v", len(got), st.Attempts, accepted, cutIDs)
			switch scheme {
			case "udp":
				if cut == 0 || st.Attempts != 100 {
					t.Fatalf("%d messages cut in %d attempts, want line 500 among them in 100", cut, st.Attempts)
				}
			case "tcp":
				if accepted != 1 || st.Attempts != 2 {
					t.Fatalf("%d connections and %d attempts, want 1 and 2", accepted, st.Attempts)
				}
			case "tls":
				if accepted != 1 || st.Attempts != 2 || len(versions) != 1 || versions[0] != tls.VersionTLS13 {
					t.Fatalf("%d connections, %d attempts, versions %x; want 1, 2 and TLS 1.3", accepted, st.Attempts, versions)
				}
			}
		})
	}
}

// runeLine is the server line n at INFO with the field pad.
func runeLine(n int, pad string) logs.Line {
	l := testLine("server", n)
	l.JSON = []byte(strings.TrimSuffix(string(l.JSON), "}") + `,"pad":"` + pad + `"}`)
	return l
}

// Over udp a message above 2,048 octets is cut at the start of a character:
// lines whose pad holds characters of 2, 3 and 4 octets, after 0 to 3
// octets of ASCII, each arrive as their start in valid UTF-8, cut at the
// last character within 2,048 octets, and for each size of character at
// least one cut falls where octet 2,048 is inside a character.
func TestSyslogCutsAtTheStartOfACharacter(t *testing.T) {
	stub := newSyslogStub(t, "udp", nil)
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": "syslog", "address": stub.address(), "batch_lines": 1})
	var log testLog
	m := startManager(t, newFakeClock(), &set, &log)
	statsOf(t, m, "demo")
	var lines []logs.Line
	for _, r := range []string{"é", "漢", "\U00010348"} {
		for shift := 0; shift < 4; shift++ {
			lines = append(lines, runeLine(len(lines), strings.Repeat("x", shift)+strings.Repeat(r, 3000/len(r))))
		}
	}
	shipPaced(t, m, lines, 1, func() int { return len(stub.messages()) })
	if refused := stub.refusals(); len(refused) > 0 {
		t.Fatalf("the stub refused %d messages, the first: %s", len(refused), refused[0])
	}
	inside := map[int]int{} // the cuts inside a character, by its size
	for i, msg := range stub.messages() {
		line := lines[i]
		_, next := utf8.DecodeRune(line.JSON[min(len(msg.msg), len(line.JSON)):])
		if !msg.cut || !bytes.HasPrefix(line.JSON, msg.msg) || msg.size > 2048 || msg.size+next <= 2048 {
			t.Fatalf("line %d: %d octets that are not its line cut at the last character within 2,048 octets", i, msg.size)
		}
		if msg.size < 2048 {
			inside[next]++ // octet 2,048 was inside the next character
		}
	}
	if inside[2] == 0 || inside[3] == 0 || inside[4] == 0 {
		t.Fatalf("cuts inside a character of 2, 3 and 4 octets: %v", inside)
	}
	waitCounted(t, m, "demo", len(lines))
	if st := statsOf(t, m, "demo"); st.Cut != int64(len(lines)) || st.Sent != int64(len(lines)) {
		t.Fatalf("%+v", st)
	}
	t.Logf("%d lines cut, cuts inside a character by its size: %v", len(lines), inside)
}

// syslogText is a message of this server for the server line n at INFO,
// written by hand from the rules above.
func syslogText(n, pad int) string {
	line := paddedLine("server", n, pad)
	return "<134>1 " + line.Time.UTC().Format("2006-01-02T15:04:05.000Z07:00") + " demo-host pipesmith-registry " +
		strconv.Itoa(os.Getpid()) + " server - " + msgBOM + string(line.JSON)
}

// The stub refuses what RFC 5424, RFC 5426 and RFC 6587 do not allow and
// what this server does not send, one bad message each, sent to it
// directly; it takes the good message once as a datagram and once as a
// frame. Each case is refused for its own rule.
func TestSyslogStubRefuses(t *testing.T) {
	good := syslogText(1, 0)
	head := strings.TrimSuffix(good, string(testLine("server", 1).JSON))
	other := func(from, to string) string { return strings.Replace(good, from, to, 1) }
	pid := strconv.Itoa(os.Getpid())
	datagrams := []struct{ name, text, want string }{
		{"good", good, ""},
		{"PRI above 191", other("<134>", "<192>"), "PRIVAL"},
		{"PRI with a leading zero", other("<134>", "<06>"), "PRIVAL"},
		{"PRI of four digits", other("<134>", "<0134>"), "PRI"},
		{"facility other than local0", other("<134>", "<14>"), "facility"},
		{"severity of another level", other("<134>", "<132>"), "severity"},
		{"VERSION 2", other(">1 ", ">2 "), "VERSION"},
		{"nine decimals", other(".001Z", ".001000000Z"), "TIMESTAMP"},
		{"lower case t", other("T10:", "t10:"), "TIMESTAMP"},
		{"offset without a colon", other("10:00:00.001Z", "11:00:00.001+0100"), "TIMESTAMP"},
		{"a day that does not exist", other("2026-11-19T", "2026-02-30T"), "TIMESTAMP"},
		{"NILVALUE as TIMESTAMP", other("2026-11-19T10:00:00.001Z", "-"), "TIMESTAMP"},
		{"six decimals", other(".001Z", ".001000Z"), "milliseconds"},
		{"an offset in place of Z", other("10:00:00.001Z", "11:00:00.001+01:00"), "milliseconds"},
		{"another time than the line", other("10:00:00.001Z", "10:00:01.001Z"), "TIMESTAMP"},
		{"HOSTNAME of 256 characters", other(" demo-host ", " "+strings.Repeat("h", 256)+" "), "HOSTNAME"},
		{"APP-NAME of 49 characters", other(" pipesmith-registry ", " "+strings.Repeat("a", 49)+" "), "APP-NAME"},
		{"PROCID of 129 characters", other(" "+pid+" ", " "+strings.Repeat("1", 129)+" "), "PROCID"},
		{"MSGID of 33 characters", other(" server - ", " "+strings.Repeat("m", 33)+" - "), "MSGID"},
		{"a non ASCII octet in HOSTNAME", other(" demo-host ", " demo-h\xc3\xb6st "), "HOSTNAME"},
		{"an empty field", other(" demo-host ", " demo-host  "), "APP-NAME"},
		{"another HOSTNAME", other(" demo-host ", " demo-other "), "HOSTNAME"},
		{"another APP-NAME", other(" pipesmith-registry ", " demo-app "), "APP-NAME"},
		{"another PROCID", other(" "+pid+" ", " 0 "), "PROCID"},
		{"MSGID that is no kind", other(" server - ", " debug - "), "MSGID"},
		{"STRUCTURED-DATA", other(" server - ", ` server [demo@32473 a="b"] `), "STRUCTURED-DATA"},
		{"no SP before MSG", other(" - "+msgBOM, " -"+msgBOM), "STRUCTURED-DATA"},
		{"no MSG", strings.TrimSuffix(head, " "+msgBOM), "missing"},
		{"no byte order mark", other(msgBOM, ""), "byte order mark"},
		{"MSG not UTF-8", head + `{"msg":"` + "\xff" + `"}`, "UTF-8"},
		{"MSG not JSON", head + "not json", "JSON"},
		{"the head in another case", head + `{"Time":"2026-11-19T10:00:00.001Z","LEVEL":"INFO","Logger":"pipesmith_registry.demo","Msg":"line"}`,
			"time, level, logger and msg"},
		{"two messages in one datagram", good + good, "JSON"},
		{"a datagram above 2,048 octets", syslogText(1, 2100), "2,048"},
	}
	frame := func(text string) string { return strconv.Itoa(len(text)) + " " + text }
	frames := []struct {
		name, text string
		want       []string
	}{
		{"good", frame(good), nil},
		{"LF framing", good + "\n", []string{"digit 1 to 9"}},
		{"MSG-LEN with a leading zero", "0" + frame(good), []string{"digit 1 to 9"}},
		{"MSG-LEN with a letter", "12a " + good, []string{"MSG-LEN holds"}},
		{"a frame cut short", strconv.Itoa(len(good)+10) + " " + good, []string{"ends before"}},
		{"MSG-LEN shorter than the message", strconv.Itoa(len(good)-5) + " " + good, []string{"JSON", "digit 1 to 9"}},
	}
	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			stub := newSyslogStub(t, network, nil)
			var wants []string
			taken := 0
			send := func(name, text string, refusals []string) {
				t.Helper()
				conn, err := net.Dial(network, strings.TrimPrefix(stub.address(), network+"://"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conn.Close() }()
				if _, err := io.WriteString(conn, text); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if tc, ok := conn.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
				if len(refusals) == 0 {
					taken++
				}
				wants = append(wants, refusals...)
				waitFor(t, func() bool { return stub.seen() == taken+len(wants) }, name)
			}
			if network == "udp" {
				for _, c := range datagrams {
					var want []string
					if c.want != "" {
						want = []string{c.want}
					}
					send(c.name, c.text, want)
				}
			} else {
				for _, c := range frames {
					send(c.name, c.text, c.want)
				}
			}
			refused := stub.refusals()
			if len(refused) != len(wants) {
				t.Fatalf("%d refusals, want %d: %q", len(refused), len(wants), refused)
			}
			for i, reason := range refused {
				if !strings.Contains(reason, wants[i]) {
					t.Errorf("refusal %d: %q, want %q in it", i+1, reason, wants[i])
				}
			}
			if got := stub.messages(); len(got) != 1 || got[0].id != "server-0001" {
				t.Fatalf("the stub took %d messages, want the good one", len(got))
			}
		})
	}
}

// refusedAddress is an address on loopback where nothing listens: a port of
// network that was free a moment ago.
func refusedAddress(t *testing.T, network string) string {
	t.Helper()
	if network == "udp" {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := pc.LocalAddr().String()
		_ = pc.Close()
		return addr
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// checkRefusedDial ships one line to a target of kind whose address of
// scheme has nothing listening: 3 attempts with the pauses of 500 ms and of
// 2 s between them, then the line fails and the one warning names the
// class dial.
func checkRefusedDial(t *testing.T, kind, scheme string) {
	t.Helper()
	var set setting
	set.put(t, map[string]any{"name": "demo", "kind": kind, "address": scheme + "://" + refusedAddress(t, "tcp"), "batch_lines": 1})
	clk := newFakeClock()
	var log testLog
	m := startManager(t, clk, &set, &log)
	statsOf(t, m, "demo")
	m.Ship(testLine("tasks", 1))
	clk.waitPending(t, 500*time.Millisecond)
	clk.Advance(500 * time.Millisecond)
	clk.waitPending(t, 2*time.Second)
	clk.Advance(2 * time.Second)
	waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == 1 }, "the line given up")
	if st := statsOf(t, m, "demo"); st.Attempts != 3 || st.Errors != 3 || st.Sent != 0 {
		t.Fatalf("%+v", st)
	}
	m.Close() // the warning of the last attempt is written before the stop line
	if warns := log.records(t, "log target failing"); len(warns) != 1 || warns[0]["error"] != "dial" {
		t.Fatalf("warnings %v", warns)
	}
}

// checkUDPWriteError sends through the adapter of kind to a udp port where
// nothing listens: the first datagram leaves, the answer of the host (ICMP
// port unreachable) makes the next write fail, which is a failure of class
// write that is tried again, and the next attempt opens a new socket and
// sends. This is the one failure over udp that a sender can see.
func checkUDPWriteError(t *testing.T, kind string) {
	t.Helper()
	a, err := openAdapter(Target{Name: "demo", Kind: kind, Address: "udp://" + refusedAddress(t, "udp")}, "",
		env{host: stubHost, version: testVersion, pid: os.Getpid(), clock: realClock{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	send := func(n int) error {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, err := a.Send(ctx, []logs.Line{testLine("server", n)})
		return err
	}
	if err := send(1); err != nil {
		t.Fatalf("the first datagram: %v", err)
	}
	if err := send(2); classOf(err) != "write" || final(err) {
		t.Fatalf("the write after the answer of the host: %v; want a failure of class write that is tried again", err)
	}
	if err := send(2); err != nil {
		t.Fatalf("the attempt after the failure: %v", err)
	}
}

// deafStub accepts TCP connections and never reads from them.
type deafStub struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
	done  chan struct{}
}

func newDeafStub(t *testing.T) *deafStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &deafStub{ln: ln, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-s.done
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, c := range s.conns {
			_ = c.Close()
		}
	})
	return s
}

func (s *deafStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// The rules of a stream that fails: a dial that fails is tried 3 times and
// the line fails as dial; a receiver that stops reading makes the write
// fail by the timeout of the target: 3 attempts, each at least the timeout
// long, at most one connection each; a receiver that ends the connection
// gets it closed by the sender in turn, and the next batch goes over a new
// connection, over tcp and over tls, with no line lost; over udp a local
// write error fails the attempt as write.
func TestSyslogRetryRules(t *testing.T) {
	t.Run("refused", func(t *testing.T) { checkRefusedDial(t, "syslog", "tcp") })
	t.Run("deaf", func(t *testing.T) {
		stub := newDeafStub(t)
		var set setting
		set.put(t, map[string]any{"name": "demo", "kind": "syslog", "address": "tcp://" + stub.ln.Addr().String(),
			"batch_lines": 8, "queue_bytes": 32 << 20, "timeout_ms": 200})
		clk := newFakeClock()
		var log testLog
		m := startManager(t, clk, &set, &log)
		statsOf(t, m, "demo")
		lines := make([]logs.Line, 8) // 8 MiB, far above what the socket buffers of loopback hold
		for i := range lines {
			lines[i] = paddedLine("server", i, 1<<20)
		}
		start := time.Now()
		shipAll(t, m, lines)
		clk.waitPending(t, 500*time.Millisecond)
		clk.Advance(500 * time.Millisecond)
		clk.waitPending(t, 2*time.Second)
		clk.Advance(2 * time.Second)
		waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == 8 }, "the batch given up")
		took := time.Since(start) // the pauses are of the fake clock, so this is the 3 attempts and the test
		if st := statsOf(t, m, "demo"); st.Attempts != 3 || st.Errors != 3 || st.Sent != 0 || took < 3*200*time.Millisecond || stub.count() > 3 {
			t.Fatalf("%+v in %v over %d connections", st, took, stub.count())
		}
		m.Close()
		if warns := log.records(t, "log target failing"); len(warns) != 1 || warns[0]["error"] != "timeout" {
			t.Fatalf("warnings %v", warns)
		}
	})
	for _, scheme := range []string{"tcp", "tls"} {
		t.Run("closed "+scheme, func(t *testing.T) {
			stub := newSyslogStub(t, scheme, nil)
			stub.setCloseAfter(3)
			row := map[string]any{"name": "demo", "kind": "syslog", "address": stub.address(), "batch_lines": 3}
			if scheme == "tls" {
				row["ca_file"] = stub.caFile
			}
			var set setting
			set.put(t, row)
			var log testLog
			m := startManager(t, newFakeClock(), &set, &log)
			statsOf(t, m, "demo")
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
	}
	t.Run("udp", func(t *testing.T) { checkUDPWriteError(t, "syslog") })
}

// certForName makes an authority and a server certificate signed by it for
// the DNS name name only, and writes the authority as PEM to a file for
// ca_file.
func certForName(t *testing.T, name string) (caFile string, server tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "demo name authority"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: name},
		DNSNames: []string{name}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return caFile, tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: key}
}

// Over tls the adapter trusts the roots of ca_file only, else those of the
// system, checks the host name of the address against the certificate, and
// needs TLS 1.2 at least: a receiver with a certificate of an unknown
// authority, with a certificate for another name, or that speaks TLS 1.1 at
// most fails each of the 3 attempts as tls and takes no message. The
// receiver of TLS 1.1 takes a client that allows TLS 1.1, so the version is
// what fails.
func TestSyslogTLSChecksTheReceiver(t *testing.T) {
	trustedFile, trusted := testCA(t)
	otherFile, other := certForName(t, "demo-other.example.com")
	cases := []struct {
		name, caFile string
		conf         *tls.Config
	}{
		{"unknown authority", "", &tls.Config{Certificates: []tls.Certificate{trusted}, MinVersion: tls.VersionTLS12}},
		{"another name", otherFile, &tls.Config{Certificates: []tls.Certificate{other}, MinVersion: tls.VersionTLS12}},
		{"tls 1.1", trustedFile, &tls.Config{Certificates: []tls.Certificate{trusted}, MinVersion: tls.VersionTLS10,
			MaxVersion: tls.VersionTLS11}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := newSyslogStub(t, "tls", c.conf)
			if c.conf.MaxVersion == tls.VersionTLS11 {
				roots, err := rootsOf(trustedFile)
				if err != nil {
					t.Fatal(err)
				}
				conn, err := tls.Dial("tcp", strings.TrimPrefix(stub.address(), "tls://"),
					&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11})
				if err != nil {
					t.Fatalf("a client of TLS 1.1: %v", err)
				}
				version := conn.ConnectionState().Version
				_ = conn.Close()
				if version != tls.VersionTLS11 {
					t.Fatalf("the client of TLS 1.1 got version %x", version)
				}
			}
			row := map[string]any{"name": "demo", "kind": "syslog", "address": stub.address(), "batch_lines": 1}
			if c.caFile != "" {
				row["ca_file"] = c.caFile
			}
			var set setting
			set.put(t, row)
			clk := newFakeClock()
			var log testLog
			m := startManager(t, clk, &set, &log)
			statsOf(t, m, "demo")
			m.Ship(testLine("server", 1))
			clk.waitPending(t, 500*time.Millisecond)
			clk.Advance(500 * time.Millisecond)
			clk.waitPending(t, 2*time.Second)
			clk.Advance(2 * time.Second)
			waitFor(t, func() bool { return statsOf(t, m, "demo").Failed == 1 }, "the line given up")
			if st := statsOf(t, m, "demo"); st.Attempts != 3 || st.Errors != 3 || st.Sent != 0 || len(stub.messages()) != 0 {
				t.Fatalf("%+v, %d messages taken", st, len(stub.messages()))
			}
			m.Close()
			if warns := log.records(t, "log target failing"); len(warns) != 1 || warns[0]["error"] != "tls" {
				t.Fatalf("warnings %v", warns)
			}
		})
	}
}
