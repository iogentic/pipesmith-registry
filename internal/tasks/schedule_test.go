package tasks

import (
	"errors"
	"testing"
	"time"
)

func at(t testing.TB, text string) time.Time {
	t.Helper()
	return instant(t, text)
}

// Each type fires at the instants of the rulings, one after the other from
// after; none ends a list.
func TestScheduleInstants(t *testing.T) {
	for _, c := range []struct {
		name     string
		schedule Schedule
		after    string
		next     []string
	}{
		{"manual never fires by schedule", Schedule{Type: TypeManual, StartAt: at(t, "2026-11-20T01:00:00Z")},
			"2026-11-19T00:00:00Z", []string{"none"}},
		{"once at startAt", Schedule{Type: TypeOnce, StartAt: at(t, "2026-11-20T22:00:00Z")},
			"2026-11-19T00:00:00Z", []string{"2026-11-20T22:00:00Z", "none"}},
		{"once, a nanosecond before startAt", Schedule{Type: TypeOnce, StartAt: at(t, "2026-11-20T22:00:00Z")},
			"2026-11-20T21:59:59.999999999Z", []string{"2026-11-20T22:00:00Z", "none"}},
		{"once, after startAt itself", Schedule{Type: TypeOnce, StartAt: at(t, "2026-11-20T22:00:00Z")},
			"2026-11-20T22:00:00Z", []string{"none"}},
		{"once, startAt cut to the second", Schedule{Type: TypeOnce, StartAt: at(t, "2026-11-20T22:00:00.750Z")},
			"2026-11-20T21:00:00Z", []string{"2026-11-20T22:00:00Z", "none"}},
		{"once, startAt in another zone", Schedule{Type: TypeOnce,
			StartAt: time.Date(2026, 11, 20, 23, 0, 0, 0, time.FixedZone("CET", 3600))},
			"2026-11-19T00:00:00Z", []string{"2026-11-20T22:00:00Z", "none"}},
		{"hourly from startAt", Schedule{Type: TypeHourly, StartAt: at(t, "2026-11-19T10:15:30Z")},
			"2026-11-19T00:00:00Z", []string{"2026-11-19T10:15:30Z", "2026-11-19T11:15:30Z", "2026-11-19T12:15:30Z"}},
		{"hourly at the minute and second of startAt", Schedule{Type: TypeHourly, StartAt: at(t, "2026-11-19T10:15:30Z")},
			"2026-11-21T03:20:00Z", []string{"2026-11-21T04:15:30Z", "2026-11-21T05:15:30Z"}},
		{"hourly, after an instant", Schedule{Type: TypeHourly, StartAt: at(t, "2026-11-19T10:15:30Z")},
			"2026-11-19T11:15:30Z", []string{"2026-11-19T12:15:30Z"}},
		{"hourly over the end of a day and a year", Schedule{Type: TypeHourly, StartAt: at(t, "2026-11-19T10:15:30Z")},
			"2026-12-31T23:15:30Z", []string{"2027-01-01T00:15:30Z"}},
		{"daily from startAt", Schedule{Type: TypeDaily, StartAt: at(t, "2026-09-01T01:00:00Z")},
			"2026-08-01T00:00:00Z", []string{"2026-09-01T01:00:00Z", "2026-09-02T01:00:00Z"}},
		{"daily at the time of startAt", Schedule{Type: TypeDaily, StartAt: at(t, "2026-09-01T01:00:00Z")},
			"2026-11-19T00:59:59Z", []string{"2026-11-19T01:00:00Z", "2026-11-20T01:00:00Z"}},
		{"daily, after an instant", Schedule{Type: TypeDaily, StartAt: at(t, "2026-09-01T01:00:00Z")},
			"2026-11-19T01:00:00Z", []string{"2026-11-20T01:00:00Z"}},
		{"daily over February of a leap year", Schedule{Type: TypeDaily, StartAt: at(t, "2026-09-01T01:00:00Z")},
			"2028-02-28T12:00:00Z", []string{"2028-02-29T01:00:00Z", "2028-03-01T01:00:00Z"}},
		{"weekly on the day of startAt", Schedule{Type: TypeWeekly, StartAt: at(t, "2026-10-04T02:00:00Z"), WeekDays: []string{"SUN"}},
			"2026-10-01T00:00:00Z", []string{"2026-10-04T02:00:00Z", "2026-10-11T02:00:00Z"}},
		{"weekly on three days, never before startAt", Schedule{Type: TypeWeekly, StartAt: at(t, "2026-11-19T12:00:00Z"),
			WeekDays: []string{"MON", "WED", "FRI"}},
			"2026-11-01T00:00:00Z", []string{"2026-11-20T12:00:00Z", "2026-11-23T12:00:00Z", "2026-11-25T12:00:00Z", "2026-11-27T12:00:00Z"}},
		{"weekly, startAt on a day not listed", Schedule{Type: TypeWeekly, StartAt: at(t, "2026-11-19T12:00:00Z"),
			WeekDays: []string{"TUE"}},
			"2026-11-19T00:00:00Z", []string{"2026-11-24T12:00:00Z", "2026-12-01T12:00:00Z"}},
		{"weekly, later in the day of an instant", Schedule{Type: TypeWeekly, StartAt: at(t, "2026-11-19T12:00:00Z"),
			WeekDays: []string{"THU", "SAT"}},
			"2026-11-26T12:00:00Z", []string{"2026-11-28T12:00:00Z", "2026-12-03T12:00:00Z"}},
		{"monthly, never before startAt, the 31st skipped in November", Schedule{Type: TypeMonthly,
			StartAt: at(t, "2026-11-19T03:00:00Z"), MonthDays: []int{1, 15, 31}},
			"2026-11-01T00:00:00Z", []string{"2026-12-01T03:00:00Z", "2026-12-15T03:00:00Z", "2026-12-31T03:00:00Z", "2027-01-01T03:00:00Z"}},
		{"monthly on the 30th skips February", Schedule{Type: TypeMonthly, StartAt: at(t, "2026-11-19T03:00:00Z"),
			MonthDays: []int{30}},
			"2027-01-01T00:00:00Z", []string{"2027-01-30T03:00:00Z", "2027-03-30T03:00:00Z"}},
		{"monthly on the 29th of a leap year", Schedule{Type: TypeMonthly, StartAt: at(t, "2026-11-19T03:00:00Z"),
			MonthDays: []int{29}},
			"2028-02-01T00:00:00Z", []string{"2028-02-29T03:00:00Z", "2028-03-29T03:00:00Z"}},
		{"monthly on the day of startAt", Schedule{Type: TypeMonthly, StartAt: at(t, "2026-11-19T03:00:00Z"),
			MonthDays: []int{19}},
			"2026-11-19T02:59:59Z", []string{"2026-11-19T03:00:00Z", "2026-12-19T03:00:00Z"}},
		{"cron as the expression says", Schedule{Type: TypeCron, Cron: "0 0 4 ? * MON-FRI"},
			"2026-11-20T04:00:00Z", []string{"2026-11-23T04:00:00Z", "2026-11-24T04:00:00Z"}},
		{"cron does not wait for a startAt", Schedule{Type: TypeCron, Cron: "0 0 3 * * ?", StartAt: at(t, "2030-01-01T00:00:00Z")},
			"2026-11-19T00:00:00Z", []string{"2026-11-19T03:00:00Z"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			trigger, err := c.schedule.Trigger()
			if err != nil {
				t.Fatal(err)
			}
			from := at(t, c.after)
			for _, want := range c.next {
				got, ok := trigger.Next(from)
				if want == "none" {
					if ok {
						t.Fatalf("after %s: %s, want none", from.Format(time.RFC3339Nano), got.Format(time.RFC3339))
					}
					return
				}
				if !ok || !got.Equal(at(t, want)) || got.Location() != time.UTC {
					t.Fatalf("after %s: %s %v, want %s", from.Format(time.RFC3339Nano), got, ok, want)
				}
				from = got
			}
		})
	}
}

// Next of each type takes an instant of another zone as that instant in UTC.
// 00:30 on Friday at UTC+1 is 23:30 on Thursday in UTC.
func TestScheduleInstantsAfterAnInstantInAnotherZone(t *testing.T) {
	east, far := time.FixedZone("UTC+1", 3600), time.FixedZone("UTC+14", 14*3600)
	friday := time.Date(2026, 11, 20, 0, 30, 0, 0, east)
	for _, c := range []struct {
		name     string
		schedule Schedule
		after    time.Time
		want     string
	}{
		{"once", Schedule{Type: TypeOnce, StartAt: at(t, "2026-11-19T23:45:00Z")}, friday, "2026-11-19T23:45:00Z"},
		{"hourly", Schedule{Type: TypeHourly, StartAt: at(t, "2026-11-01T00:45:00Z")}, friday, "2026-11-19T23:45:00Z"},
		{"daily", Schedule{Type: TypeDaily, StartAt: at(t, "2026-11-01T23:45:00Z")}, friday, "2026-11-19T23:45:00Z"},
		{"weekly", Schedule{Type: TypeWeekly, StartAt: at(t, "2026-11-01T23:45:00Z"), WeekDays: []string{"THU"}}, friday,
			"2026-11-19T23:45:00Z"},
		{"monthly", Schedule{Type: TypeMonthly, StartAt: at(t, "2026-11-01T23:45:00Z"), MonthDays: []int{19}}, friday,
			"2026-11-19T23:45:00Z"},
		{"monthly, fourteen hours east of UTC", Schedule{Type: TypeMonthly, StartAt: at(t, "2026-11-01T12:00:00Z"),
			MonthDays: []int{19}}, time.Date(2026, 11, 20, 1, 0, 0, 0, far), "2026-11-19T12:00:00Z"},
		{"cron", Schedule{Type: TypeCron, Cron: "0 45 23 ? * THU"}, friday, "2026-11-19T23:45:00Z"},
	} {
		t.Run(c.name, func(t *testing.T) {
			trigger, err := c.schedule.Trigger()
			if err != nil {
				t.Fatal(err)
			}
			got, ok := trigger.Next(c.after)
			if !ok || !got.Equal(at(t, c.want)) || got.Location() != time.UTC {
				t.Fatalf("after %s: %s %v, want %s", c.after.Format(time.RFC3339), got, ok, c.want)
			}
		})
	}
}

// Each refusal names its field in a FieldError; the one of cron carries the
// CronError of the expression.
func TestScheduleRefusals(t *testing.T) {
	start := at(t, "2026-11-20T01:00:00Z")
	for _, c := range []struct {
		name     string
		schedule Schedule
		field    string
	}{
		{"no type", Schedule{StartAt: start}, "type"},
		{"an unknown type", Schedule{Type: "yearly", StartAt: start}, "type"},
		{"a type in another case", Schedule{Type: "Daily", StartAt: start}, "type"},
		{"once without startAt", Schedule{Type: TypeOnce}, "startAt"},
		{"hourly without startAt", Schedule{Type: TypeHourly}, "startAt"},
		{"daily without startAt", Schedule{Type: TypeDaily}, "startAt"},
		{"weekly without startAt", Schedule{Type: TypeWeekly, WeekDays: []string{"MON"}}, "startAt"},
		{"monthly without startAt", Schedule{Type: TypeMonthly, MonthDays: []int{1}}, "startAt"},
		{"weekly without days", Schedule{Type: TypeWeekly, StartAt: start}, "weekDays"},
		{"weekly with an empty list", Schedule{Type: TypeWeekly, StartAt: start, WeekDays: []string{}}, "weekDays"},
		{"weekly with an unknown day", Schedule{Type: TypeWeekly, StartAt: start, WeekDays: []string{"MON", "FUNDAY"}}, "weekDays"},
		{"weekly with a day in lower case", Schedule{Type: TypeWeekly, StartAt: start, WeekDays: []string{"mon"}}, "weekDays"},
		{"monthly without days", Schedule{Type: TypeMonthly, StartAt: start}, "monthDays"},
		{"monthly with day 0", Schedule{Type: TypeMonthly, StartAt: start, MonthDays: []int{1, 0}}, "monthDays"},
		{"monthly with day 32", Schedule{Type: TypeMonthly, StartAt: start, MonthDays: []int{32}}, "monthDays"},
		{"monthly with a negative day", Schedule{Type: TypeMonthly, StartAt: start, MonthDays: []int{-1}}, "monthDays"},
		{"cron without an expression", Schedule{Type: TypeCron}, "cron"},
		{"cron with an expression the form refuses", Schedule{Type: TypeCron, Cron: "0 0 12 * * *"}, "cron"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.schedule.Trigger()
			var refusal *FieldError
			if !errors.As(err, &refusal) || refusal.Field != c.field || refusal.Message == "" {
				t.Fatalf("%+v: %v, want a FieldError at %s", c.schedule, err, c.field)
			}
			if c.field != "cron" || c.schedule.Cron == "" {
				return
			}
			var cron *CronError
			if !errors.As(err, &cron) || cron.Field != bothDays {
				t.Fatalf("%v carries no CronError of the day fields", err)
			}
		})
	}
}

// The seven types of the contract by their words; manual and cron need no
// startAt; a field of another type is no part of the schedule and is not
// checked.
func TestScheduleAccepts(t *testing.T) {
	for _, s := range []Schedule{
		{Type: "manual"},
		{Type: "once", StartAt: at(t, "2026-11-20T01:00:00Z")},
		{Type: "hourly", StartAt: at(t, "2026-11-20T01:00:00Z")},
		{Type: "cron", Cron: "0 0 12 * * ?"},
		{Type: "daily", StartAt: at(t, "2026-11-20T01:00:00Z"), WeekDays: []string{"FUNDAY"}, MonthDays: []int{99}, Cron: "x"},
		{Type: "manual", Cron: "x", MonthDays: []int{0}},
		{Type: "weekly", StartAt: at(t, "2026-11-20T01:00:00Z"), WeekDays: []string{"MON", "TUE", "WED", "THU", "FRI", "SAT", "SUN"}},
		{Type: "monthly", StartAt: at(t, "2026-11-20T01:00:00Z"), MonthDays: []int{31, 1, 31}},
	} {
		trigger, err := s.Trigger()
		if err != nil {
			t.Errorf("%+v: %v", s, err)
			continue
		}
		if trigger.Type() != s.Type {
			t.Errorf("%+v: type %q", s, trigger.Type())
		}
	}
}
