package tasks

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const cronVectors = "../../testdata/vectors/cron"

// cronCase is one vector: an expression and either the instants that follow
// from, or the field of the expression a refusal names.
type cronCase struct {
	Source     string   `json:"source"`
	What       string   `json:"what"`
	Expression string   `json:"expression"`
	From       string   `json:"from"`
	Next       []string `json:"next"`
	Refused    string   `json:"refused"`
	Field      string   `json:"field"`
}

type cronFile struct {
	Note  string     `json:"note"`
	Cases []cronCase `json:"cases"`
}

var (
	cronSources = []string{"tutorial", "api page", "source"}
	cronFields  = []string{"expression", "seconds", "minutes", "hours", "day of month", "month", "day of week", "year",
		"day of month and day of week"}
)

// cronFiles reads every vector file by its name without .json.
func cronFiles(t testing.TB) map[string]cronFile {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(cronVectors, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no vector in %s: %v", cronVectors, err)
	}
	files := map[string]cronFile{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var file cronFile
		if err := json.Unmarshal(data, &file); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		files[strings.TrimSuffix(filepath.Base(path), ".json")] = file
	}
	return files
}

func instant(t testing.TB, text string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t.Fatalf("instant %q: %v", text, err)
	}
	return at
}

// Every vector holds: the instants follow one another from its start, or the
// expression is refused and the refusal names the field of the vector.
// Cases of unverified.json are skipped with their reason.
func TestCronVectors(t *testing.T) {
	for name, file := range cronFiles(t) {
		if file.Note == "" {
			t.Errorf("%s.json has no note", name)
		}
		for i, c := range file.Cases {
			t.Run(name+"/"+c.Expression, func(t *testing.T) {
				if name == "unverified" {
					t.Skipf("not checked against a page or the source: %s", c.What)
				}
				if !slices.Contains(cronSources, c.Source) || c.What == "" {
					t.Fatalf("case %d: source %q, what %q", i, c.Source, c.What)
				}
				cron, err := ParseCron(c.Expression)
				if c.Refused != "" {
					var refusal *CronError
					switch {
					case err == nil:
						t.Fatalf("%q was accepted, want a refusal at %s (%s)", c.Expression, c.Field, c.Refused)
					case !errors.As(err, &refusal):
						t.Fatalf("%q: %v is no CronError", c.Expression, err)
					case refusal.Field != c.Field:
						t.Fatalf("%q refused at %q (%v), want %q (%s)", c.Expression, refusal.Field, err, c.Field, c.Refused)
					}
					return
				}
				if err != nil {
					t.Fatalf("%q: %v", c.Expression, err)
				}
				if len(c.Next) == 0 {
					t.Fatalf("case %d has neither next nor refused", i)
				}
				at := instant(t, c.From)
				for _, want := range c.Next {
					got, ok := cron.Next(at)
					if want == "none" {
						if ok {
							t.Fatalf("%q after %s: %s, want none", c.Expression, at.Format(time.RFC3339), got.Format(time.RFC3339))
						}
						return
					}
					if !ok || !got.Equal(instant(t, want)) || got.Location() != time.UTC {
						t.Fatalf("%q after %s: %s %v, want %s", c.Expression, at.Format(time.RFC3339Nano), got, ok, want)
					}
					at = got
				}
			})
		}
	}
}

// The examples of the tutorial page, the table and section Format, each
// stand in tutorial.json.
func TestCronVectorsHoldTheTutorialExamples(t *testing.T) {
	examples := []string{"0 0 12 * * ?", "0 15 10 ? * *", "0 15 10 * * ?", "0 15 10 * * ? *", "0 15 10 * * ? 2005",
		"0 * 14 * * ?", "0 0/5 14 * * ?", "0 0/5 14,18 * * ?", "0 0-5 14 * * ?", "0 10,44 14 ? 3 WED",
		"0 15 10 ? * MON-FRI", "0 15 10 15 * ?", "0 15 10 L * ?", "0 15 10 L-2 * ?", "0 15 10 ? * 6L",
		"0 15 10 ? * 6L 2002-2005", "0 15 10 ? * 6#3", "0 0 12 1/5 * ?", "0 11 11 11 11 ?", "* * * * ? *",
		"0/5 14,18,3-39,52 * ? JAN,MAR,SEP MON-FRI 2002-2010"}
	var held []string
	for _, c := range cronFiles(t)["tutorial"].Cases {
		if c.Source == "tutorial" {
			held = append(held, c.Expression)
		}
	}
	for _, example := range examples {
		if !slices.Contains(held, example) {
			t.Errorf("tutorial.json lacks the example %q", example)
		}
	}
	if len(examples) != 21 {
		t.Fatalf("%d examples, the page has 21 distinct ones", len(examples))
	}
}

// Accepted vectors use each special character, names in upper and in lower
// case, and refused ones cover every field and the expression.
func TestCronVectorsHoldEachSpecialCharacter(t *testing.T) {
	accepted, refused := "", map[string]bool{}
	for _, file := range cronFiles(t) {
		for _, c := range file.Cases {
			if c.Refused == "" {
				accepted += c.Expression + "\n"
			} else {
				refused[c.Field] = true
			}
		}
	}
	for _, mark := range []string{"*", "?", "-", ",", "/", "L", "W", "#", "LW", "L-", "MON", "mon", "JAN", "jan"} {
		if !strings.Contains(accepted, mark) {
			t.Errorf("no accepted vector uses %q", mark)
		}
	}
	for _, field := range cronFields {
		if !refused[field] {
			t.Errorf("no refused vector names the field %q", field)
		}
	}
}

// Accepted vectors use each name JAN to DEC in the month and each name SUN
// to SAT in the day of week, alone, in a list, in a range, or before L or #.
func TestCronVectorsUseEveryName(t *testing.T) {
	months, days := map[string]bool{}, map[string]bool{}
	for _, file := range cronFiles(t) {
		for _, c := range file.Cases {
			fields := strings.Fields(strings.ToUpper(c.Expression))
			if c.Refused != "" || len(fields) < 6 {
				continue
			}
			for i, used := range []map[string]bool{months, days} {
				for _, item := range strings.Split(fields[4+i], ",") {
					item, _, _ = strings.Cut(item, "/")
					item, _, _ = strings.Cut(item, "#")
					if i == 1 {
						item = strings.TrimSuffix(item, "L") // FRIL, the last Friday
					}
					for _, part := range strings.Split(item, "-") {
						used[part] = true
					}
				}
			}
		}
	}
	for _, name := range strings.Fields("JAN FEB MAR APR MAY JUN JUL AUG SEP OCT NOV DEC") {
		if !months[name] {
			t.Errorf("no accepted vector uses the name %s in the month", name)
		}
	}
	for _, name := range strings.Fields("SUN MON TUE WED THU FRI SAT") {
		if !days[name] {
			t.Errorf("no accepted vector uses the name %s in the day of week", name)
		}
	}
}

// An expression that never matches again gives none: the search ends with
// the year LastYear and returns quickly. The last second of LastYear is the
// last instant there is.
func TestCronNeverIsNoneWithinTheBound(t *testing.T) {
	from := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	begin := time.Now()
	for _, expr := range []string{"0 0 0 30 2 ?", "0 0 0 31 2,4,6,9,11 ?", "0 0 0 ? 2 1#5 2027", "0 0 0 L-30 2 ?",
		"0 0 0 * * ? 2026"} {
		c, err := ParseCron(expr)
		if err != nil {
			t.Fatalf("%q: %v", expr, err)
		}
		if next, ok := c.Next(from.AddDate(1, 0, 0)); ok {
			t.Errorf("%q: %s, want none", expr, next)
		}
	}
	if took := time.Since(begin); took > time.Second {
		t.Errorf("five searches to the end of %d took %s", LastYear, took)
	}
	every, err := ParseCron("* * * ? * * *")
	if err != nil {
		t.Fatal(err)
	}
	end := time.Date(LastYear, 12, 31, 23, 59, 59, 0, time.UTC)
	if next, ok := every.Next(end.Add(-time.Second)); !ok || !next.Equal(end) {
		t.Errorf("the last second of %d: %s %v, want %s", LastYear, next, ok, end)
	}
	if next, ok := every.Next(end); ok {
		t.Errorf("after the last second of %d: %s, want none", LastYear, next)
	}
}

// Next takes an instant of any zone as that instant in UTC: the day and the
// time of day are those of UTC, and the result is in UTC.
func TestCronNextOfAnInstantInAnotherZone(t *testing.T) {
	east, west := time.FixedZone("UTC+1", 3600), time.FixedZone("UTC-5", -5*3600)
	for _, c := range []struct {
		expr  string
		after time.Time
		want  string
	}{
		{"0 0 3 * * ?", time.Date(2026, 11, 19, 3, 30, 0, 0, east), "2026-11-19T03:00:00Z"},
		{"0 0 3 * * ?", time.Date(2026, 11, 19, 22, 30, 0, 0, west), "2026-11-21T03:00:00Z"},
		{"0 45 23 ? * THU", time.Date(2026, 11, 20, 0, 30, 0, 0, east), "2026-11-19T23:45:00Z"},
		{"0 0 0 1 * ?", time.Date(2026, 11, 30, 20, 0, 0, 0, west), "2027-01-01T00:00:00Z"},
	} {
		cron, err := ParseCron(c.expr)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := cron.Next(c.after)
		if !ok || !got.Equal(instant(t, c.want)) || got.Location() != time.UTC || !got.After(c.after) {
			t.Errorf("%q after %s: %s %v, want %s", c.expr, c.after.Format(time.RFC3339), got, ok, c.want)
		}
	}
}

// refNext is the next instant by a walk over the days of the next limit
// days, with the day rules written out per day.
func refNext(c *Cron, after time.Time, limit int) (time.Time, bool) {
	start := after.UTC().Truncate(time.Second).Add(time.Second)
	day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	for i := 0; i < limit; i, day = i+1, day.AddDate(0, 0, 1) {
		y := day.Year()
		if y < FirstYear || y > LastYear || c.years[(y-FirstYear)/64]&(1<<((y-FirstYear)%64)) == 0 ||
			c.months&(1<<day.Month()) == 0 || !refDay(c, day) {
			continue
		}
		for h := 0; h < 24; h++ {
			for m := 0; m < 60; m++ {
				for s := 0; s < 60; s++ {
					if c.hours&(1<<h) == 0 || c.minutes&(1<<m) == 0 || c.seconds&(1<<s) == 0 {
						continue
					}
					at := day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
					if !at.Before(start) {
						return at, true
					}
				}
			}
		}
	}
	return time.Time{}, false
}

func refDay(c *Cron, day time.Time) bool {
	y, m, d := day.Date()
	last := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
	nearest := func(n int) int {
		switch time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n-1).Weekday() {
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
	w := day.Weekday()
	switch r := c.days; r.kind {
	case monthDays:
		return r.set&(1<<d) != 0
	case lastDay:
		n := last - r.offset
		if r.nearest {
			n = nearest(n)
		}
		return n == d
	case nearDay:
		return nearest(r.day) == d
	case weekDays:
		return r.set&(1<<w) != 0
	case lastWeekday:
		return w == r.weekday && d+7 > last
	case nthWeekday:
		return w == r.weekday && (d-1)/7+1 == r.nth
	}
	return false
}

// The parser never panics, refuses with a CronError that names a field, and
// the next instant lies strictly after the start, on a whole second in UTC,
// and is the one a walk over the days finds first.
func FuzzCron(f *testing.F) {
	for _, file := range cronFiles(f) {
		for _, c := range file.Cases {
			f.Add(c.Expression, int64(1_795_046_400_123))
		}
	}
	const span = int64(7_258_118_400_000) // milliseconds from 1970 to 2200
	f.Fuzz(func(t *testing.T, expr string, ms int64) {
		c, err := ParseCron(expr)
		if err != nil {
			var refusal *CronError
			if !errors.As(err, &refusal) || !slices.Contains(cronFields, refusal.Field) {
				t.Fatalf("%q: %v", expr, err)
			}
			return
		}
		start := time.UnixMilli((ms%span + span) % span).UTC()
		next, ok := c.Next(start)
		if ok && (!next.After(start) || next.Nanosecond() != 0 || next.Location() != time.UTC || next.Year() > LastYear) {
			t.Fatalf("%q after %s: %s", expr, start.Format(time.RFC3339Nano), next)
		}
		const limit = 400
		ref, found := refNext(c, start, limit)
		switch {
		case found && (!ok || !next.Equal(ref)):
			t.Fatalf("%q after %s: %s %v, a walk over the days finds %s", expr, start.Format(time.RFC3339Nano), next, ok, ref)
		case !found && ok && next.Before(start.AddDate(0, 0, limit-1)):
			t.Fatalf("%q after %s: %s, a walk over %d days finds none", expr, start.Format(time.RFC3339Nano), next, limit)
		}
	})
}
