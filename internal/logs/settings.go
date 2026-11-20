package logs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// SettingKey is the key of the settings of the log files in the table
// settings of state.sqlite.
const SettingKey = "logs"

// Settings are the limits of the log files (L1 and L3 of the plan).
type Settings struct {
	MaxBytes      int64         // a file rotates before a write takes it past this size
	MaxAge        time.Duration // a rotated file is removed this long after the end of its day
	MaxTotalBytes int64         // the rotated files of one kind together
	Classic       bool          // requests-classic.log is written
}

// DefaultSettings are 100 MiB, 90 days, 2 GiB per kind and the classic file
// on.
func DefaultSettings() Settings {
	return Settings{MaxBytes: 100 << 20, MaxAge: 90 * 24 * time.Hour, MaxTotalBytes: 2 << 30, Classic: true}
}

// withDefaults gives a limit that is not positive its default; Classic
// stays as it is.
func (s Settings) withDefaults() Settings {
	d := DefaultSettings()
	if s.MaxBytes <= 0 {
		s.MaxBytes = d.MaxBytes
	}
	if s.MaxAge <= 0 {
		s.MaxAge = d.MaxAge
	}
	if s.MaxTotalBytes <= 0 {
		s.MaxTotalBytes = d.MaxTotalBytes
	}
	return s
}

// SettingsFrom reads the setting logs of state.sqlite: a JSON object with
// the keys max_bytes, max_age_days, max_total_bytes and classic. An absent
// key keeps its default; a value that is not a positive number, or not a
// boolean for classic, keeps the default and gives one warning.
func SettingsFrom(data []byte) (Settings, []string) {
	s := DefaultSettings()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil || doc == nil {
		return s, []string{"the setting logs is not a JSON object; the defaults hold"}
	}
	var warnings []string
	warn := func(key string, def any) {
		warnings = append(warnings, fmt.Sprintf("the setting logs: %s is not a positive number; the default %v holds", key, def))
	}
	if v, ok := doc["max_bytes"]; ok {
		if n, valid := positive(v); valid {
			s.MaxBytes = wholeBytes(n)
		} else {
			warn("max_bytes", s.MaxBytes)
		}
	}
	if v, ok := doc["max_age_days"]; ok {
		if n, valid := positive(v); valid {
			s.MaxAge = days(n)
		} else {
			warn("max_age_days", s.MaxAge.Hours()/24)
		}
	}
	if v, ok := doc["max_total_bytes"]; ok {
		if n, valid := positive(v); valid {
			s.MaxTotalBytes = wholeBytes(n)
		} else {
			warn("max_total_bytes", s.MaxTotalBytes)
		}
	}
	if v, ok := doc["classic"]; ok {
		if b, valid := v.(bool); valid {
			s.Classic = b
		} else {
			warnings = append(warnings, fmt.Sprintf("the setting logs: classic is not true or false; the default %v holds", s.Classic))
		}
	}
	return s, warnings
}

// positive is a JSON number above 0 that a float64 holds.
func positive(v any) (float64, bool) {
	number, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseFloat(string(number), 64)
	return n, err == nil && n > 0 && !math.IsInf(n, 0)
}

// wholeBytes rounds up to a whole byte, at most the largest int64.
func wholeBytes(n float64) int64 {
	if n >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(math.Ceil(n))
}

// days is n days, at most the largest duration.
func days(n float64) time.Duration {
	d := n * float64(24*time.Hour)
	if d >= math.MaxInt64 {
		return math.MaxInt64
	}
	return time.Duration(d)
}
