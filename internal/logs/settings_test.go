package logs

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultSettings(t *testing.T) {
	want := Settings{MaxBytes: 100 << 20, MaxAge: 90 * 24 * time.Hour, MaxTotalBytes: 2 << 30, Classic: true}
	if got := DefaultSettings(); got != want {
		t.Fatalf("%+v, want %+v", got, want)
	}
}

// The setting logs: an absent key keeps its default, a value of the wrong
// kind keeps it too and gives one warning.
func TestSettingsFrom(t *testing.T) {
	defaults := DefaultSettings()
	for _, c := range []struct {
		name     string
		data     string
		want     Settings
		warnings []string
	}{
		{"empty object", `{}`, defaults, nil},
		{"all set", `{"max_bytes": 1024, "max_age_days": 7, "max_total_bytes": 4096, "classic": false}`,
			Settings{MaxBytes: 1024, MaxAge: 7 * 24 * time.Hour, MaxTotalBytes: 4096, Classic: false}, nil},
		{"half a day and a fraction of a byte", `{"max_age_days": 0.5, "max_bytes": 10.5}`,
			Settings{MaxBytes: 11, MaxAge: 12 * time.Hour, MaxTotalBytes: defaults.MaxTotalBytes, Classic: true}, nil},
		{"an unknown key", `{"max_lines": 5}`, defaults, nil},
		{"wrong kinds", `{"max_bytes": -1, "max_age_days": "7", "max_total_bytes": 0, "classic": "no"}`, defaults,
			[]string{"max_bytes", "max_age_days", "max_total_bytes", "classic"}},
		{"null and a list", `{"max_bytes": null, "classic": [true]}`, defaults, []string{"max_bytes", "classic"}},
		{"a number too large", `{"max_total_bytes": 1e400}`, defaults, []string{"max_total_bytes"}},
		{"not an object", `[1, 2]`, defaults, []string{"not a JSON object"}},
		{"not JSON", `{"max_bytes": `, defaults, []string{"not a JSON object"}},
	} {
		got, warnings := SettingsFrom([]byte(c.data))
		if got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
		if len(warnings) != len(c.warnings) {
			t.Errorf("%s: warnings %q, want one for each of %q", c.name, warnings, c.warnings)
			continue
		}
		for i, w := range warnings {
			if !strings.Contains(w, c.warnings[i]) || !strings.Contains(w, "the default") {
				t.Errorf("%s: warning %q, want it to name %s and the default", c.name, w, c.warnings[i])
			}
		}
	}
	if got, _ := SettingsFrom([]byte(`{"max_bytes": 1e300}`)); got.MaxBytes <= 0 || reflect.DeepEqual(got, defaults) {
		t.Errorf("a very large positive number: %+v", got)
	}
}
