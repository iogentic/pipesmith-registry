package release

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Window is the time of day in which the scheduled run may switch, in the
// local time of the host: From up to To, both minutes after midnight. A
// window whose end lies before its start runs over midnight.
type Window struct {
	From, To int
	Text     string
}

var windowText = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])-([01][0-9]|2[0-3]):([0-5][0-9])$`)

// Window reads window of update.json, "HH:MM-HH:MM"; ok is false without
// the key, and then any time is inside.
func (c *UpdateConfig) Window() (w Window, ok bool, err error) {
	raw, found := c.members["window"]
	if !found {
		return Window{}, false, nil
	}
	var text *string
	if err := json.Unmarshal(raw, &text); err != nil || text == nil {
		return Window{}, false, fmt.Errorf("%w: %s: window is %s, not a text HH:MM-HH:MM", ErrConfig, c.Path, raw)
	}
	m := windowText.FindStringSubmatch(*text)
	if m == nil {
		return Window{}, false, fmt.Errorf("%w: %s: window %q is not HH:MM-HH:MM in 24 hour time", ErrConfig, c.Path, *text)
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	w = Window{From: n(m[1])*60 + n(m[2]), To: n(m[3])*60 + n(m[4]), Text: *text}
	if w.From == w.To {
		return Window{}, false, fmt.Errorf("%w: %s: window %q starts where it ends", ErrConfig, c.Path, *text)
	}
	if !w.meetsTimer() {
		return Window{}, false, fmt.Errorf("%w: %s: window %q holds no full span HH:00 to HH:10; the timer starts in the first 10 minutes of each hour and would never run inside it", ErrConfig, c.Path, *text)
	}
	return w, true, nil
}

// meetsTimer reports whether the window holds the minutes 0 to 10 of some
// hour, the span in which pipesmith-registry-update.timer starts the run
// (OnCalendar=hourly, RandomizedDelaySec=10min).
func (w Window) meetsTimer() bool {
	for h := 0; h < 24; h++ {
		inside := true
		for m := 0; m <= 10; m++ {
			if !w.Contains(time.Date(2026, 1, 1, h, m, 0, 0, time.UTC)) {
				inside = false
				break
			}
		}
		if inside {
			return true
		}
	}
	return false
}

// Contains reports whether t, in its own location, lies in the window; the
// end is outside.
func (w Window) Contains(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	if w.From < w.To {
		return m >= w.From && m < w.To
	}
	return m >= w.From || m < w.To
}

// Default and bound of check_hours.
const (
	DefaultCheckHours = 6
	MaxCheckHours     = 168
)

// CheckHours reads check_hours of update.json: how many hours serve waits
// between two checks of the releases of the channel; 0 turns the check off.
// Without the key it is DefaultCheckHours.
func (c *UpdateConfig) CheckHours() (int, error) {
	raw, found := c.members["check_hours"]
	if !found {
		return DefaultCheckHours, nil
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil || n < 0 || n > MaxCheckHours {
		return 0, fmt.Errorf("%w: %s: check_hours is %s, want a whole number from 0 to %d", ErrConfig, c.Path, raw, MaxCheckHours)
	}
	return n, nil
}
