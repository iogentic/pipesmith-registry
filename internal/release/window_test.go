package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func configOf(t *testing.T, text string) *UpdateConfig {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadUpdateConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 21, h, m, 0, 0, time.UTC) }
	w, ok, err := configOf(t, `{"window": "02:00-05:30"}`).Window()
	if err != nil || !ok || w.Text != "02:00-05:30" {
		t.Fatalf("%+v %v %v", w, ok, err)
	}
	for tm, in := range map[time.Time]bool{at(1, 59): false, at(2, 0): true, at(5, 29): true, at(5, 30): false, at(14, 0): false} {
		if w.Contains(tm) != in {
			t.Errorf("%s: inside %v", tm.Format("15:04"), !in)
		}
	}
	w, _, _ = configOf(t, `{"window": "23:00-01:00"}`).Window()
	for tm, in := range map[time.Time]bool{at(22, 59): false, at(23, 0): true, at(0, 30): true, at(1, 0): false} {
		if w.Contains(tm) != in {
			t.Errorf("over midnight %s: inside %v", tm.Format("15:04"), !in)
		}
	}
	for _, good := range []string{`"02:00-02:11"`, `"02:50-04:00"`, `"23:30-00:30"`} {
		if _, ok, err := configOf(t, `{"window": `+good+`}`).Window(); !ok || err != nil {
			t.Errorf("window %s: %v", good, err)
		}
	}
	if _, ok, err := configOf(t, `{}`).Window(); ok || err != nil {
		t.Fatalf("no key: %v %v", ok, err)
	}
	for _, bad := range []string{`"02:30-03:00"`, `"02:00-02:10"`, `"02:15-03:05"`, `"2:00-5:00"`, `"24:00-01:00"`, `"02:00-02:00"`, `5`, `null`, `"02:00"`} {
		if _, _, err := configOf(t, `{"window": `+bad+`}`).Window(); err == nil || !strings.Contains(err.Error(), "window") {
			t.Errorf("window %s: %v", bad, err)
		}
	}
}

func TestCheckHours(t *testing.T) {
	if n, err := configOf(t, `{}`).CheckHours(); n != DefaultCheckHours || err != nil {
		t.Fatalf("default %d %v", n, err)
	}
	if n, err := configOf(t, `{"check_hours": 0}`).CheckHours(); n != 0 || err != nil {
		t.Fatalf("off %d %v", n, err)
	}
	for _, bad := range []string{`-1`, `169`, `1.5`, `"6"`} {
		if _, err := configOf(t, `{"check_hours": `+bad+`}`).CheckHours(); err == nil {
			t.Errorf("check_hours %s accepted", bad)
		}
	}
}

func TestHistoryNewestFirst(t *testing.T) {
	dir := t.TempDir()
	for i, to := range []string{"0.2.0", "0.3.0", "0.4.0"} {
		e := newEntry("update", ResultOK, "x", to, "r")
		e.Auto = i == 2
		if err := appendHistory(dir, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := History(dir, 2)
	if err != nil || len(got) != 2 || got[0].To != "0.4.0" || !got[0].Auto || got[1].To != "0.3.0" || got[1].Auto {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err := History(t.TempDir(), 5); got != nil || err != nil {
		t.Fatalf("no file: %v %v", got, err)
	}
}
