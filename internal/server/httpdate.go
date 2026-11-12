package server

import (
	"math"
	"math/big"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mirosso25/pipesmith-registry/internal/pystr"
)

// Header values are latin-1 text in Starlette: one character per byte. The
// helpers here work on the bytes of a Go header value in that sense.

// latin1Space is str.isspace of a latin-1 character.
func latin1Space(c byte) bool {
	return c >= 0x09 && c <= 0x0D || c >= 0x1C && c <= 0x20 || c == 0x85 || c == 0xA0
}

// latin1Digit is str.isdigit of a latin-1 character: the superscripts count.
func latin1Digit(c byte) bool {
	return c >= '0' && c <= '9' || c == 0xB2 || c == 0xB3 || c == 0xB9
}

// latin1Strip is str.strip() of a latin-1 text.
func latin1Strip(s string) string {
	start, end := 0, len(s)
	for start < end && latin1Space(s[start]) {
		start++
	}
	for end > start && latin1Space(s[end-1]) {
		end--
	}
	return s[start:end]
}

// latin1Fields is str.split() of a latin-1 text.
func latin1Fields(s string) []string {
	var out []string
	start := -1
	for i := 0; i < len(s); i++ {
		if latin1Space(s[i]) {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func asciiUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

// latin1Int is int() of a latin-1 text, nil where int() raises ValueError.
func latin1Int(s string) *big.Int {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		b = utf8.AppendRune(b, rune(s[i]))
	}
	n, err := pystr.ParseInt(string(b))
	if err != nil {
		return nil
	}
	return n
}

// overflowError is the OverflowError that email.utils and datetime raise
// for numbers out of range. Python does not catch it in _parse_http_date,
// so the request ends as a server error.
type overflowError struct{ message string }

func (e *overflowError) Error() string { return e.message }

func isOverflow(err error) bool {
	_, ok := err.(*overflowError)
	return ok
}

var (
	monthNames = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec",
		"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november",
		"december"}
	dayNames  = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
	timezones = map[string]int64{"UT": 0, "UTC": 0, "GMT": 0, "Z": 0, "AST": -400, "ADT": -300, "EST": -500,
		"EDT": -400, "CST": -600, "CDT": -500, "MST": -700, "MDT": -600, "PST": -800, "PDT": -700}
)

// dateTuple is the result of email._parseaddr._parsedate_tz; tz is nil for
// no zone.
type dateTuple struct {
	year, month, day, hour, minute, second *big.Int
	tz                                     *big.Int
}

// parsedateTZ ports email._parseaddr._parsedate_tz of Python 3.12. An
// IndexError of Python, which _parse_http_date catches, gives nil as well.
func parsedateTZ(data string) *dateTuple {
	if data == "" {
		return nil
	}
	d := latin1Fields(data)
	if len(d) == 0 {
		return nil
	}
	if strings.HasSuffix(d[0], ",") || slices.Contains(dayNames, asciiLower(d[0])) {
		d = d[1:]
	} else if i := strings.LastIndex(d[0], ","); i >= 0 {
		d[0] = d[0][i+1:]
	}
	if len(d) == 3 {
		if stuff := strings.Split(d[0], "-"); len(stuff) == 3 {
			d = append(stuff, d[1:]...)
		}
	}
	if len(d) == 4 {
		s := d[3]
		i := strings.Index(s, "+")
		if i == -1 {
			i = strings.Index(s, "-")
		}
		if i > 0 {
			d = append(d[:3:3], s[:i], s[i:])
		} else {
			d = append(d, "")
		}
	}
	if len(d) < 5 {
		return nil
	}
	dd, mm, yy, tm, tz := d[0], d[1], d[2], d[3], d[4]
	if dd == "" || mm == "" || yy == "" {
		return nil
	}
	mm = asciiLower(mm)
	if !slices.Contains(monthNames, mm) {
		dd, mm = mm, asciiLower(dd)
		if !slices.Contains(monthNames, mm) {
			return nil
		}
	}
	month := int64(slices.Index(monthNames, mm) + 1)
	if month > 12 {
		month -= 12
	}
	dd = strings.TrimSuffix(dd, ",")
	if i := strings.Index(yy, ":"); i > 0 {
		yy, tm = tm, yy
	}
	if strings.HasSuffix(yy, ",") {
		yy = yy[:len(yy)-1]
		if yy == "" {
			return nil
		}
	}
	if yy == "" || tm == "" {
		return nil // IndexError
	}
	if !latin1Digit(yy[0]) {
		yy, tz = tz, yy
	}
	tm = strings.TrimSuffix(tm, ",")
	var thh, tmm, tss string
	switch parts := strings.Split(tm, ":"); {
	case len(parts) == 2:
		thh, tmm, tss = parts[0], parts[1], "0"
	case len(parts) == 3:
		thh, tmm, tss = parts[0], parts[1], parts[2]
	case len(parts) == 1 && strings.Contains(parts[0], "."):
		dots := strings.Split(parts[0], ".")
		switch len(dots) {
		case 2:
			thh, tmm, tss = dots[0], dots[1], "0"
		case 3:
			thh, tmm, tss = dots[0], dots[1], dots[2]
		default:
			return nil
		}
	default:
		return nil
	}
	t := &dateTuple{month: big.NewInt(month)}
	for _, f := range []struct {
		text string
		into **big.Int
	}{{yy, &t.year}, {dd, &t.day}, {thh, &t.hour}, {tmm, &t.minute}, {tss, &t.second}} {
		if *f.into = latin1Int(f.text); *f.into == nil {
			return nil
		}
	}
	if t.year.Cmp(big.NewInt(100)) < 0 {
		if t.year.Cmp(big.NewInt(68)) > 0 {
			t.year.Add(t.year, big.NewInt(1900))
		} else {
			t.year.Add(t.year, big.NewInt(2000))
		}
	}
	tz = asciiUpper(tz)
	var offset *big.Int
	if v, ok := timezones[tz]; ok {
		offset = big.NewInt(v)
	} else {
		offset = latin1Int(tz)
		if offset != nil && offset.Sign() == 0 && strings.HasPrefix(tz, "-") {
			offset = nil
		}
	}
	if offset != nil && offset.Sign() != 0 {
		sign := int64(1)
		if offset.Sign() < 0 {
			sign = -1
			offset.Neg(offset)
		}
		hours, minutes := new(big.Int).DivMod(offset, big.NewInt(100), new(big.Int))
		hours.Mul(hours, big.NewInt(3600))
		hours.Add(hours, minutes.Mul(minutes, big.NewInt(60)))
		offset = hours.Mul(hours, big.NewInt(sign))
	}
	t.tz = offset
	return t
}

func daysIn(month, year int64) int64 {
	switch month {
	case 2:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

// parseHTTPDate is _parse_http_date: the seconds since the epoch of an HTTP
// date, ok false where Python returns None. The error is the OverflowError
// of timedelta and datetime, which Python lets through.
func parseHTTPDate(value string) (int64, bool, error) {
	t := parsedateTZ(value)
	if t == nil {
		return 0, false, nil
	}
	// The zone is built first, as an argument of datetime().
	var offset int64
	if t.tz != nil {
		days := new(big.Int).Div(t.tz, big.NewInt(86400)) // Euclidean: floor for a positive divisor
		if days.Cmp(big.NewInt(-999_999_999)) < 0 || days.Cmp(big.NewInt(999_999_999)) > 0 {
			return 0, false, &overflowError{"days out of range"}
		}
		if t.tz.Cmp(big.NewInt(-86400)) <= 0 || t.tz.Cmp(big.NewInt(86400)) >= 0 {
			return 0, false, nil
		}
		offset = t.tz.Int64()
	}
	fields := []*big.Int{t.year, t.month, t.day, t.hour, t.minute, t.second}
	for _, n := range fields {
		if n.Cmp(big.NewInt(math.MinInt32)) < 0 || n.Cmp(big.NewInt(math.MaxInt32)) > 0 {
			return 0, false, &overflowError{"signed integer out of range"}
		}
	}
	y, mo, da, h, mi, s := t.year.Int64(), t.month.Int64(), t.day.Int64(), t.hour.Int64(), t.minute.Int64(), t.second.Int64()
	if y < 1 || y > 9999 || mo < 1 || mo > 12 || da < 1 || da > daysIn(mo, y) || h < 0 || h > 23 || mi < 0 || mi > 59 ||
		s < 0 || s > 59 {
		return 0, false, nil
	}
	return time.Date(int(y), time.Month(mo), int(da), int(h), int(mi), int(s), 0, time.UTC).Unix() - offset, true, nil
}

// httpDate is _http_date: formatdate(ms / 1000, usegmt=True), ok false
// where Python gives None. The float steps are those of fromtimestamp.
func httpDate(ms int64) (string, bool) {
	t := float64(ms) / 1000
	whole, frac := math.Modf(t)
	frac = math.RoundToEven(frac * 1e6)
	if frac >= 1e6 {
		whole++
	} else if frac < 0 {
		whole--
	}
	if whole < -9.2e18 || whole > 9.2e18 {
		return "", false
	}
	when := time.Unix(int64(whole), 0).UTC()
	if when.Year() < 1 || when.Year() > 9999 {
		return "", false
	}
	return when.Format("Mon, 02 Jan 2006 15:04:05 GMT"), true
}

// floorDiv is a // b of Python.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
