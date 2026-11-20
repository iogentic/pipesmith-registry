package logship

import (
	"context"
	"log/slog"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The adapter syslog: one message of RFC 5424 per line. Over udp each
// message is one datagram (RFC 5426), and a message above 2,048 octets is
// cut; over tcp and tls each message is framed by its octet count,
// MSG-LEN SP SYSLOG-MSG (RFC 6587, section 3.4.1; RFC 5425, section 4.3).

func init() { register(kindSyslog, openSyslog) }

const (
	syslogFacility = 16                              // local0
	syslogApp      = "pipesmith-registry"            // APP-NAME
	syslogDatagram = 2048                            // the most octets of a message over udp
	syslogTime     = "2006-01-02T15:04:05.000Z07:00" // TIMESTAMP, in UTC
	byteOrderMark  = "\xef\xbb\xbf"                  // the start of a MSG in UTF-8 (RFC 5424, section 6.4)
)

type syslogAdapter struct {
	link   *link
	udp    bool
	host   string // HOSTNAME
	procID string // PROCID
	clock  clock
}

func openSyslog(t Target, _ string, e env) (Adapter, error) {
	l, err := newLink(t)
	if err != nil {
		return nil, err
	}
	return &syslogAdapter{link: l, udp: l.network == "udp", host: headerField(e.host, 255),
		procID: headerField(strconv.Itoa(e.pid), 128), clock: e.clock}, nil
}

func (a *syslogAdapter) Send(ctx context.Context, batch []logs.Line) (Result, error) {
	if !a.udp {
		var frames []byte
		for _, line := range batch {
			msg := a.message(line)
			frames = strconv.AppendInt(frames, int64(len(msg)), 10)
			frames = append(append(frames, ' '), msg...)
		}
		return Result{}, a.link.write(ctx, frames)
	}
	var res Result
	for i, line := range batch {
		msg, cut := cutAt(a.message(line), syslogDatagram)
		if err := a.link.write(ctx, msg); err != nil {
			res.Retry = batch[i:]
			return res, err
		}
		if cut {
			res.Cut++
		}
	}
	return res, nil
}

func (a *syslogAdapter) Close() error {
	a.link.close()
	return nil
}

// message is the SYSLOG-MSG of a line: PRI of facility local0 and the
// severity of its level, VERSION 1, its time in UTC with milliseconds,
// HOSTNAME, APP-NAME, PROCID, its kind as MSGID, no STRUCTURED-DATA, and
// the JSON line after the byte order mark as MSG.
func (a *syslogAdapter) message(line logs.Line) []byte {
	msg := make([]byte, 0, 96+len(line.JSON))
	msg = append(msg, '<')
	msg = strconv.AppendInt(msg, int64(syslogFacility*8+severity(line.Level)), 10)
	msg = append(msg, ">1 "...)
	msg = lineTime(line, a.clock).AppendFormat(msg, syslogTime)
	msg = append(append(append(msg, ' '), a.host...), ' ')
	msg = append(append(msg, syslogApp...), ' ')
	msg = append(append(msg, a.procID...), ' ')
	msg = append(append(msg, headerField(line.Kind, 32)...), " - "...)
	return append(append(msg, byteOrderMark...), line.JSON...)
}

// severity is the syslog severity of a level (RFC 5424, table 2): DEBUG 7,
// INFO 6, WARN 4, ERROR 3. The RFC names no mapping; this one is ours.
func severity(level slog.Level) int {
	switch {
	case level >= slog.LevelError:
		return 3
	case level >= slog.LevelWarn:
		return 4
	case level >= slog.LevelInfo:
		return 6
	}
	return 7
}

// lineTime is the time of a line in UTC; a line without a time after 1970
// takes the time of the clock.
func lineTime(line logs.Line, c clock) time.Time {
	if line.Time.After(time.Unix(0, 0)) {
		return line.Time.UTC()
	}
	return c.Now().UTC()
}

// headerField is value as a field of the header: printable US-ASCII, 33 to
// 126, of at most n octets, and the NILVALUE when empty. Every other octet
// becomes an underscore.
func headerField(value string, n int) string {
	b := []byte(value)
	for i, c := range b {
		if c < 33 || c > 126 {
			b[i] = '_'
		}
	}
	if len(b) > n {
		b = b[:n]
	}
	if len(b) == 0 {
		return "-"
	}
	return string(b)
}

// cutAt shortens msg to at most n octets at the start of a UTF-8 character
// and reports whether it did.
func cutAt(msg []byte, n int) ([]byte, bool) {
	if len(msg) <= n {
		return msg, false
	}
	for n > 0 && !utf8.RuneStart(msg[n]) {
		n--
	}
	return msg[:n], true
}
