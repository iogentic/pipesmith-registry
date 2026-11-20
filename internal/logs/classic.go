package logs

import (
	"strconv"
	"time"
)

// classicLayout is the time of a classic line, the date of the common log
// format; it is written in UTC.
const classicLayout = "02/Jan/2006:15:04:05 -0700"

const hexDigits = "0123456789abcdef"

// classicLine is the line of one request in requests-classic.log, in the
// field order of the request log of the old product, the request id in the
// place of the thread name:
//
//	<client> - <subject> [<time>] "<method> <path and query> <protocol>" <status> <content length> <bytes sent> <duration in ms> "<user agent>" [<request id>]
//
// An empty field is "-"; appendClassic escapes every field, so no request
// can break or forge a line. The duration is cut to whole milliseconds.
func (r Request) classicLine(now time.Time) []byte {
	r = r.capped()
	t := r.Time
	if t.IsZero() {
		t = now
	}
	target := r.Path
	if r.Query != "" {
		target += "?" + r.Query
	}
	b := make([]byte, 0, 256)
	b = appendClassic(b, r.Client, false)
	b = append(b, " - "...)
	b = appendClassic(b, r.Subject, false)
	b = append(b, " ["...)
	b = t.UTC().AppendFormat(b, classicLayout)
	b = append(b, `] "`...)
	b = appendClassic(b, r.Method, false)
	b = append(b, ' ')
	b = appendClassic(b, target, false)
	b = append(b, ' ')
	b = appendClassic(b, r.Protocol, false)
	b = append(b, `" `...)
	b = append(b, strconv.Itoa(r.Status)...)
	b = append(b, ' ')
	b = appendClassic(b, r.ContentLength, false)
	b = append(b, ' ')
	b = strconv.AppendInt(b, r.ResponseBytes, 10)
	b = append(b, ' ')
	b = strconv.AppendInt(b, r.Duration.Milliseconds(), 10)
	b = append(b, ` "`...)
	b = appendClassic(b, r.UserAgent, true)
	b = append(b, `" [`...)
	b = appendClassic(b, r.ID, false)
	return append(b, "]\n"...)
}

// appendClassic appends one field of a classic line: a byte outside 0x21 to
// 0x7e, the quote and the backslash as \xHH, the space kept when space is
// true (the user agent). An empty field is "-".
func appendClassic(b []byte, field string, space bool) []byte {
	if field == "" {
		return append(b, '-')
	}
	for i := 0; i < len(field); i++ {
		c := field[i]
		switch {
		case c == ' ' && space:
			b = append(b, c)
		case c < 0x21 || c > 0x7e || c == '"' || c == '\\':
			b = append(b, '\\', 'x', hexDigits[c>>4], hexDigits[c&0x0f])
		default:
			b = append(b, c)
		}
	}
	return b
}
