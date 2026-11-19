package tasks

import (
	"fmt"
	"time"
)

// The schedule types of the contract.
const (
	TypeManual  = "manual"
	TypeOnce    = "once"
	TypeHourly  = "hourly"
	TypeDaily   = "daily"
	TypeWeekly  = "weekly"
	TypeMonthly = "monthly"
	TypeCron    = "cron"
)

// The fields of a schedule, as a refusal names them.
const (
	FieldType      = "type"
	FieldStartAt   = "startAt"
	FieldWeekDays  = "weekDays"
	FieldMonthDays = "monthDays"
	FieldCron      = "cron"
)

var weekDayNames = map[string]time.Weekday{"MON": time.Monday, "TUE": time.Tuesday, "WED": time.Wednesday,
	"THU": time.Thursday, "FRI": time.Friday, "SAT": time.Saturday, "SUN": time.Sunday}

// Schedule is the schedule of a task: a type and the fields that type uses.
// StartAt is zero when it is not given. A field of another type is ignored.
type Schedule struct {
	Type      string
	StartAt   time.Time
	WeekDays  []string // MON to SUN, for weekly
	MonthDays []int    // 1 to 31, for monthly
	Cron      string   // for cron
}

// FieldError is the refusal of a schedule: the field it names and why. For
// the field cron, Err is the CronError of the expression.
type FieldError struct {
	Field   string
	Message string
	Err     error
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Message }

func (e *FieldError) Unwrap() error { return e.Err }

// Trigger is a schedule that passed its checks. All its instants are UTC
// and on whole seconds.
type Trigger struct {
	kind      string
	start     time.Time
	weekDays  uint64 // bit n: time.Weekday n
	monthDays uint64 // bit n: day n
	cron      *Cron
}

// Trigger checks s. manual and cron need no startAt, the other five do;
// startAt is cut to whole seconds. weekly needs one day or more of MON to
// SUN, monthly one or more of 1 to 31, cron an expression of the form.
func (s Schedule) Trigger() (Trigger, error) {
	t := Trigger{kind: s.Type, start: s.StartAt.UTC().Truncate(time.Second)}
	switch s.Type {
	case TypeManual:
		return t, nil
	case TypeCron:
		if s.Cron == "" {
			return Trigger{}, &FieldError{Field: FieldCron, Message: "a cron schedule needs an expression"}
		}
		c, err := ParseCron(s.Cron)
		if err != nil {
			return Trigger{}, &FieldError{Field: FieldCron, Message: err.Error(), Err: err}
		}
		t.cron = c
		return t, nil
	case TypeOnce, TypeHourly, TypeDaily, TypeWeekly, TypeMonthly:
	default:
		return Trigger{}, &FieldError{Field: FieldType, Message: fmt.Sprintf(
			"%q is no schedule type; the types are manual, once, hourly, daily, weekly, monthly and cron", s.Type)}
	}
	if s.StartAt.IsZero() {
		return Trigger{}, &FieldError{Field: FieldStartAt, Message: fmt.Sprintf("a %s schedule needs startAt", s.Type)}
	}
	switch s.Type {
	case TypeWeekly:
		if len(s.WeekDays) == 0 {
			return Trigger{}, &FieldError{Field: FieldWeekDays, Message: "a weekly schedule needs one day or more"}
		}
		for _, name := range s.WeekDays {
			day, ok := weekDayNames[name]
			if !ok {
				return Trigger{}, &FieldError{Field: FieldWeekDays, Message: fmt.Sprintf("%q is no day; the days are MON to SUN", name)}
			}
			t.weekDays |= 1 << day
		}
	case TypeMonthly:
		if len(s.MonthDays) == 0 {
			return Trigger{}, &FieldError{Field: FieldMonthDays, Message: "a monthly schedule needs one day or more"}
		}
		for _, day := range s.MonthDays {
			if day < 1 || day > 31 {
				return Trigger{}, &FieldError{Field: FieldMonthDays, Message: fmt.Sprintf("%d is outside 1 to 31", day)}
			}
			t.monthDays |= 1 << day
		}
	}
	return t, nil
}

// Type is the schedule type.
func (t Trigger) Type() string { return t.kind }

// Next is the first instant of the schedule strictly after after. It is
// false when there is none: always for manual, and for once after startAt.
//
// once fires at startAt; hourly at startAt and then every hour at its minute
// and second; daily, weekly and monthly at the time of day of startAt, on
// every day, on the days of the week listed and on the days of the month
// listed, never before startAt. A day a month does not have is skipped in
// that month. cron fires as the expression says.
func (t Trigger) Next(after time.Time) (time.Time, bool) {
	from := after.UTC().Truncate(time.Second)
	switch t.kind {
	case TypeOnce:
		if t.start.After(from) {
			return t.start, true
		}
	case TypeHourly:
		return t.every(from, 3600), true
	case TypeDaily:
		return t.every(from, 86400), true
	case TypeWeekly:
		return t.firstDay(from, func(d time.Time) bool { return t.weekDays&(1<<d.Weekday()) != 0 })
	case TypeMonthly:
		return t.firstDay(from, func(d time.Time) bool { return t.monthDays&(1<<d.Day()) != 0 })
	case TypeCron:
		return t.cron.Next(after)
	}
	return time.Time{}, false
}

// every is the first instant start plus a whole number of periods, in
// seconds, strictly after from and not before start.
func (t Trigger) every(from time.Time, period int64) time.Time {
	if from.Before(t.start) {
		return t.start
	}
	n := (from.Unix()-t.start.Unix())/period + 1
	return time.Unix(t.start.Unix()+n*period, 0).UTC()
}

// firstDay is the first instant at the time of day of start, strictly after
// from and not before start, on a day that fits. 62 days hold every day of
// the month and every day of the week.
func (t Trigger) firstDay(from time.Time, fits func(time.Time) bool) (time.Time, bool) {
	d := t.start
	if !from.Before(t.start) {
		d = time.Date(from.Year(), from.Month(), from.Day(), t.start.Hour(), t.start.Minute(), t.start.Second(), 0, time.UTC)
		if !d.After(from) {
			d = d.AddDate(0, 0, 1)
		}
	}
	for range 62 {
		if fits(d) {
			return d, true
		}
		d = d.AddDate(0, 0, 1)
	}
	return time.Time{}, false
}
