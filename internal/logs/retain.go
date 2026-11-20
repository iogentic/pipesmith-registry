package logs

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// retain removes, per kind, the rotated files whose age passed and then the
// oldest rotated files while the kind holds more than MaxTotalBytes (L3).
// It looks at every kind, requests-classic also while the classic file is
// off. The active files and names outside the pattern are never touched.
func (f *Files) retain() {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return
	}
	now := f.now()
	kinds := map[string][]rotated{}
	for _, e := range entries {
		r, ok := parseRotated(e.Name())
		if !ok || r.ext == extTmp || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		r.size = info.Size()
		kinds[r.kind] = append(kinds[r.kind], r)
	}
	for _, list := range kinds {
		slices.SortFunc(list, func(a, b rotated) int {
			return cmp.Or(cmp.Compare(a.day, b.day), cmp.Compare(a.n, b.n), cmp.Compare(a.name, b.name))
		})
		var kept []rotated
		var total int64
		for _, r := range list {
			if f.expired(r, now) && f.remove(r) {
				continue
			}
			kept = append(kept, r)
			total += r.size
		}
		for _, r := range kept {
			if total <= f.settings.MaxTotalBytes {
				break
			}
			if f.remove(r) {
				total -= r.size
			}
		}
	}
}

// expired: a rotated file of day D goes when the clock is at or past D plus
// one day plus MaxAge.
func (f *Files) expired(r rotated, now time.Time) bool {
	day, err := time.Parse(dayLayout, r.day)
	if err != nil {
		return false
	}
	return !now.Before(day.Add(24 * time.Hour).Add(f.settings.MaxAge))
}

// remove removes a rotated file; false when it is still there.
func (f *Files) remove(r rotated) bool {
	err := os.Remove(filepath.Join(f.dir, r.name))
	return err == nil || os.IsNotExist(err)
}

// retainHourly queues the retention when the hour of the clock changed
// since the last one.
func (f *Files) retainHourly() {
	hour := f.now().UTC().Truncate(time.Hour)
	f.mu.Lock()
	due := !f.closed && !hour.Equal(f.retained)
	if due {
		f.retained = hour
	}
	f.mu.Unlock()
	if due {
		f.maint.hourly()
	}
}
