package tasks

import (
	"fmt"
	"math/bits"
	"strings"
	"time"
)

// FirstYear and LastYear bound the field year: 1970 to 2199, the values the
// class description of the cron form allows. Next finds no instant after the
// end of LastYear.
const (
	FirstYear = 1970
	LastYear  = 2199
)

// Cron is a parsed cron expression: six or seven fields separated by spaces
// or tabs, for second, minute, hour, day of month, month, day of week and an
// optional year, with the special characters * ? - , / L W # and the names
// JAN to DEC and SUN to SAT in either case. Control characters at the ends
// of a field, as a line break, are no part of it. It is read in UTC.
// Exactly one of the two day fields is ?.
//
// Where the two pages of the form differ or say nothing, the readings follow
// the class of the old scheduler: months 1 to 12, ranges that wrap, a step
// on a range, a day the month does not have is skipped, and Next is strictly
// after its argument. That class reads a name by its first three letters and
// skips an empty item of a list, so a full English name that stands alone
// in its item, as MONDAY or JUNE, and an empty item are taken here too.
// Other text that class drops or reads in its own way is refused here. The
// vectors in testdata/vectors/cron list both.
type Cron struct {
	text    string
	seconds uint64 // bit n: second n
	minutes uint64 // bit n: minute n
	hours   uint64 // bit n: hour n
	months  uint64 // bit n: month n, 1 to 12
	years   set    // bit n: year FirstYear+n
	days    days
}

type dayKind int

const (
	monthDays   dayKind = iota // a set of days of the month
	lastDay                    // L or L-n, with W the weekday nearest to it
	nearDay                    // nW
	weekDays                   // a set of days of the week
	lastWeekday                // nL
	nthWeekday                 // n#k
)

// days is the rule of the day field that is not ?.
type days struct {
	kind    dayKind
	set     uint64       // monthDays: bit n, day n; weekDays: bit n, time.Weekday n
	offset  int          // lastDay: days before the last day of the month
	nearest bool         // lastDay: the weekday nearest to that day
	day     int          // nearDay: the day of the month
	weekday time.Weekday // lastWeekday and nthWeekday
	nth     int          // nthWeekday: 1 to 5
}

// CronError is the refusal of a cron expression. Field is the field it
// names, as "day of month", or "expression" for the expression as a whole.
type CronError struct {
	Field  string
	Reason string
}

func (e *CronError) Error() string { return e.Field + ": " + e.Reason }

// field is one field of the form: its values, its largest step (0 for no
// bound), whether a range may wrap past its end, its names, and the full
// names that may stand alone in an item.
type field struct {
	name     string
	min, max int
	step     int
	wraps    bool
	names    map[string]int
	full     map[string]int
}

var (
	monthNames = map[string]int{"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6, "JUL": 7, "AUG": 8,
		"SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12}
	monthFull = map[string]int{"JANUARY": 1, "FEBRUARY": 2, "MARCH": 3, "APRIL": 4, "JUNE": 6, "JULY": 7,
		"AUGUST": 8, "SEPTEMBER": 9, "OCTOBER": 10, "NOVEMBER": 11, "DECEMBER": 12}
	dayNames = map[string]int{"SUN": 1, "MON": 2, "TUE": 3, "WED": 4, "THU": 5, "FRI": 6, "SAT": 7}
	dayFull  = map[string]int{"SUNDAY": 1, "MONDAY": 2, "TUESDAY": 3, "WEDNESDAY": 4, "THURSDAY": 5, "FRIDAY": 6,
		"SATURDAY": 7}

	fieldSecond  = field{name: "seconds", min: 0, max: 59, step: 59, wraps: true}
	fieldMinute  = field{name: "minutes", min: 0, max: 59, step: 59, wraps: true}
	fieldHour    = field{name: "hours", min: 0, max: 23, step: 23, wraps: true}
	fieldDay     = field{name: "day of month", min: 1, max: 31, step: 31, wraps: true}
	fieldMonth   = field{name: "month", min: 1, max: 12, step: 12, wraps: true, names: monthNames, full: monthFull}
	fieldWeekday = field{name: "day of week", min: 1, max: 7, step: 7, wraps: true, names: dayNames, full: dayFull}
	fieldYear    = field{name: "year", min: FirstYear, max: LastYear}

	inOrder = []field{fieldSecond, fieldMinute, fieldHour, fieldDay, fieldMonth, fieldWeekday, fieldYear}
)

const bothDays = "day of month and day of week"

// set holds one bit per value of a field: bit n for the value min+n.
type set [4]uint64

func (s *set) add(n int) { s[n/64] |= 1 << (n % 64) }

func (s *set) has(n int) bool { return s[n/64]&(1<<(n%64)) != 0 }

func (f field) fail(format string, args ...any) error {
	return &CronError{Field: f.name, Reason: fmt.Sprintf(format, args...)}
}

// ParseCron reads expr.
func ParseCron(expr string) (*Cron, error) {
	upper := []byte(expr)
	for i, b := range upper {
		if 'a' <= b && b <= 'z' {
			upper[i] = b - 'a' + 'A'
		}
	}
	parts := strings.FieldsFunc(string(upper), func(r rune) bool { return r == ' ' || r == '\t' })
	if len(parts) < 6 || len(parts) > 7 {
		return nil, &CronError{Field: "expression", Reason: fmt.Sprintf("%d fields; the form has six or seven", len(parts))}
	}
	for i, part := range parts {
		// The class trims each field: control characters at its ends are no part of it.
		parts[i] = strings.TrimFunc(part, func(r rune) bool { return r <= ' ' })
		if parts[i] == "" {
			return nil, inOrder[i].fail("%q is white space alone", part)
		}
	}
	if len(parts) == 6 {
		parts = append(parts, "*")
	}
	c := &Cron{text: expr}
	s, err := fieldSecond.list(parts[0])
	if err != nil {
		return nil, err
	}
	c.seconds = s[0]
	if s, err = fieldMinute.list(parts[1]); err != nil {
		return nil, err
	}
	c.minutes = s[0]
	if s, err = fieldHour.list(parts[2]); err != nil {
		return nil, err
	}
	c.hours = s[0]
	domAny, dom, err := dayOfMonth(parts[3])
	if err != nil {
		return nil, err
	}
	if s, err = fieldMonth.list(parts[4]); err != nil {
		return nil, err
	}
	c.months = s[0] << 1
	dowAny, dow, err := dayOfWeek(parts[5])
	if err != nil {
		return nil, err
	}
	if c.years, err = fieldYear.list(parts[6]); err != nil {
		return nil, err
	}
	switch {
	case domAny && dowAny:
		return nil, &CronError{Field: bothDays, Reason: "? in both; one of them names the days"}
	case !domAny && !dowAny:
		return nil, &CronError{Field: bothDays, Reason: "both name days; one of them is ?"}
	case domAny:
		c.days = dow
	default:
		c.days = dom
	}
	return c, nil
}

// String is the expression as it was given.
func (c *Cron) String() string { return c.text }

// dayOfMonth reads the day of month: ?, L, L-n, LW, L-nW, nW, or a list. It
// is true for ?.
func dayOfMonth(text string) (bool, days, error) {
	f := fieldDay
	switch {
	case text == "?":
		return true, days{}, nil
	case strings.ContainsAny(text, "LW") && strings.Contains(text, ","):
		return false, days{}, f.fail("%s: L and W stand alone in the field", text)
	case strings.HasPrefix(text, "L"):
		rest, nearest := strings.CutSuffix(text[1:], "W")
		offset := 0
		if rest != "" {
			digits, dash := strings.CutPrefix(rest, "-")
			n, ok := number(digits)
			if !dash || !ok {
				return false, days{}, f.fail("%s: L stands as L, L-n, LW or L-nW", text)
			}
			if n > 30 {
				return false, days{}, f.fail("%s: the offset %d is above 30", text, n)
			}
			offset = n
		}
		return false, days{kind: lastDay, offset: offset, nearest: nearest}, nil
	case strings.HasSuffix(text, "W"):
		n, ok := number(strings.TrimSuffix(text, "W"))
		if !ok {
			return false, days{}, f.fail("%s: W follows one day", text)
		}
		if n < f.min || n > f.max {
			return false, days{}, f.fail("%s: the day %d is outside 1 to 31", text, n)
		}
		return false, days{kind: nearDay, day: n}, nil
	}
	s, err := f.list(text)
	return false, days{kind: monthDays, set: s[0] << 1}, err
}

// dayOfWeek reads the day of week: ?, L, nL, n#k, or a list. It is true
// for ?.
func dayOfWeek(text string) (bool, days, error) {
	f := fieldWeekday
	switch {
	case text == "?":
		return true, days{}, nil
	case strings.ContainsAny(text, "L#") && strings.Contains(text, ","):
		return false, days{}, f.fail("%s: L and # stand alone in the field", text)
	case text == "L":
		return false, days{kind: weekDays, set: 1 << time.Saturday}, nil
	case strings.HasSuffix(text, "L"):
		v, err := f.single(text, strings.TrimSuffix(text, "L"))
		return false, days{kind: lastWeekday, weekday: time.Weekday(v - 1)}, err
	case strings.Contains(text, "#"):
		day, nth, _ := strings.Cut(text, "#")
		v, err := f.single(text, day)
		if err != nil {
			return false, days{}, err
		}
		n, ok := number(nth)
		if !ok || n < 1 || n > 5 {
			return false, days{}, f.fail("%s: # takes a number from 1 to 5", text)
		}
		return false, days{kind: nthWeekday, weekday: time.Weekday(v - 1), nth: n}, nil
	}
	s, err := f.list(text)
	return false, days{kind: weekDays, set: s[0]}, err
}

// single reads the one day before L or #, a number or a name.
func (f field) single(text, value string) (int, error) {
	if value == "" || strings.ContainsAny(value, "-/,*?#") {
		return 0, f.fail("%s: L and # follow one day", text)
	}
	v, _, err := f.value(text, value)
	return v, err
}

// list reads a list of items: *, a value, a full name or a range, each but
// a full name with an optional step; the step of a value runs to the end of
// the field. An empty item is skipped; a list without an item is refused.
func (f field) list(text string) (set, error) {
	var s set
	items := 0
	for _, item := range strings.Split(text, ",") {
		if item == "" {
			continue
		}
		items++
		if err := f.item(item, &s); err != nil {
			return set{}, err
		}
	}
	if items == 0 {
		return set{}, f.fail("%q holds no value", text)
	}
	return s, nil
}

func (f field) item(item string, s *set) error {
	if v, ok := f.full[item]; ok {
		s.add(v - f.min)
		return nil
	}
	body, stepText, stepped := strings.Cut(item, "/")
	step := 1
	if stepped {
		n, ok := number(stepText)
		switch {
		case !ok:
			return f.fail("%s: the step is no number", item)
		case n < 1:
			return f.fail("%s: a step of 0", item)
		case f.step > 0 && n > f.step:
			return f.fail("%s: a step above %d", item, f.step)
		}
		step = n
	}
	from, to := f.min, f.max
	if body != "*" {
		low, high, ranged := strings.Cut(body, "-")
		named := false
		var err error
		if from, named, err = f.value(item, low); err != nil {
			return err
		}
		to = from
		switch {
		case ranged:
			highNamed := false
			if to, highNamed, err = f.value(item, high); err != nil {
				return err
			}
			if named != highNamed {
				return f.fail("%s: a range from a name to a number", item)
			}
		case stepped:
			to = f.max
		}
		if named && stepped {
			return f.fail("%s: a step after a name", item)
		}
	}
	span := f.max - f.min + 1
	if to < from {
		if !f.wraps {
			return f.fail("%s: the range ends before it starts", item)
		}
		to += span // the range goes on past the end of the field, from its start
	}
	for v := from; v <= to; v += step {
		s.add((v - f.min) % span)
	}
	return nil
}

// value reads a number within the field, or a name of the field; it is
// true for a name.
func (f field) value(item, text string) (int, bool, error) {
	if v, ok := f.names[text]; ok {
		return v, true, nil
	}
	n, ok := number(text)
	switch {
	case f.full[text] > 0:
		return 0, false, f.fail("%s: the full name %s stands alone in its item", item, text)
	case !ok && f.names != nil && text != "" && 'A' <= text[0] && text[0] <= 'Z':
		return 0, false, f.fail("%s: %s is no name of the field", item, text)
	case !ok:
		return 0, false, f.fail("%s: %q is no number", item, text)
	case n < f.min || n > f.max:
		return 0, false, f.fail("%s: %d is outside %d to %d", item, n, f.min, f.max)
	}
	return n, false, nil
}

// number reads decimal digits; it refuses an empty text and a value above
// 2^31-1.
func number(text string) (int, bool) {
	if text == "" {
		return 0, false
	}
	n := 0
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 1<<31-1 {
			return 0, false
		}
	}
	return n, true
}

// Next is the first instant strictly after after on a whole second, in UTC.
// It is false when no instant follows up to the end of LastYear.
func (c *Cron) Next(after time.Time) (time.Time, bool) {
	start := after.UTC().Truncate(time.Second).Add(time.Second)
	year, month, day := start.Date()
	hour, minute, second := start.Clock()
	if year < FirstYear {
		year, month, day, hour, minute, second = FirstYear, time.January, 1, 0, 0, 0
	}
	for y := year; y <= LastYear; y++ {
		if !c.years.has(y - FirstYear) {
			continue
		}
		m := time.January
		if y == year {
			m = month
		}
		for ; m <= time.December; m++ {
			if c.months&(1<<m) == 0 {
				continue
			}
			mask, first := c.days.in(y, m), 1
			if y == year && m == month {
				first = day
			}
			for d := nextBit(mask, first); d >= 0; d = nextBit(mask, d+1) {
				if y != year || m != month || d != day {
					return time.Date(y, m, d, firstBit(c.hours), firstBit(c.minutes), firstBit(c.seconds), 0, time.UTC), true
				}
				if h, mi, s, ok := c.timeFrom(hour, minute, second); ok {
					return time.Date(y, m, d, h, mi, s, 0, time.UTC), true
				}
			}
		}
	}
	return time.Time{}, false
}

// timeFrom is the first time of the sets at or after h:m:s of one day.
func (c *Cron) timeFrom(h, m, s int) (int, int, int, bool) {
	for hour := nextBit(c.hours, h); hour >= 0; hour = nextBit(c.hours, hour+1) {
		if hour > h {
			return hour, firstBit(c.minutes), firstBit(c.seconds), true
		}
		for minute := nextBit(c.minutes, m); minute >= 0; minute = nextBit(c.minutes, minute+1) {
			if minute > m {
				return hour, minute, firstBit(c.seconds), true
			}
			if second := nextBit(c.seconds, s); second >= 0 {
				return hour, minute, second, true
			}
		}
	}
	return 0, 0, 0, false
}

// in is the days of month m of year y that the rule names, bit n for day n.
func (r days) in(y int, m time.Month) uint64 {
	last := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
	switch r.kind {
	case monthDays:
		return r.set & (uint64(1)<<(last+1) - 2)
	case lastDay:
		n := last - r.offset
		if r.nearest {
			n = nearestWeekday(y, m, n, last)
		}
		return dayBit(n, last)
	case nearDay:
		return dayBit(nearestWeekday(y, m, r.day, last), last)
	case weekDays:
		first := int(weekday(y, m, 1))
		var mask uint64
		for d := 1; d <= last; d++ {
			if r.set&(1<<((first+d-1)%7)) != 0 {
				mask |= 1 << d
			}
		}
		return mask
	case lastWeekday:
		return dayBit(last-(int(weekday(y, m, last))-int(r.weekday)+7)%7, last)
	case nthWeekday:
		return dayBit(1+(int(r.weekday)-int(weekday(y, m, 1))+7)%7+7*(r.nth-1), last)
	}
	return 0
}

// nearestWeekday moves day n of a month to a weekday as the class does: a
// Saturday to the Friday before, the 1st to Monday the 3rd; a Sunday to the
// Monday after, the last day to the Friday before. For a day outside the
// month the weekday is the one of the date it counts to, as on a lenient
// calendar, and the result may lie outside the month.
func nearestWeekday(y int, m time.Month, n, last int) int {
	switch weekday(y, m, n) {
	case time.Saturday:
		if n == 1 {
			return 3
		}
		return n - 1
	case time.Sunday:
		if n == last {
			return n - 2
		}
		return n + 1
	}
	return n
}

func weekday(y int, m time.Month, d int) time.Weekday {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Weekday()
}

func dayBit(n, last int) uint64 {
	if n < 1 || n > last {
		return 0
	}
	return 1 << n
}

// nextBit is the lowest set bit of mask at from or above, or -1.
func nextBit(mask uint64, from int) int {
	if from > 63 {
		return -1
	}
	if rest := mask >> from; rest != 0 {
		return from + bits.TrailingZeros64(rest)
	}
	return -1
}

func firstBit(mask uint64) int { return bits.TrailingZeros64(mask) }
