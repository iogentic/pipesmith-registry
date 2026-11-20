package metrics

import (
	"slices"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
)

// UsageReader reads the daily counts back; *catalog.Catalog is one.
type UsageReader interface {
	Downloads(from, to string) ([]catalog.Download, error)
	RequestDays(from, to string) ([]catalog.RequestDay, error)
}

// Now is the time of the clock of the recorder; for a nil Recorder the time
// of the machine.
func (r *Recorder) Now() time.Time {
	if r == nil {
		return time.Now()
	}
	return r.clock.Now()
}

// reader is the usage when it can read, else nil.
func (r *Recorder) reader() UsageReader {
	if r == nil {
		return nil
	}
	u, _ := r.usage.(UsageReader)
	return u
}

// RequestDays are the requests per day from from to to, both included, as
// written plus what waits for the next flush.
func (r *Recorder) RequestDays(from, to string) ([]catalog.RequestDay, error) {
	if r != nil {
		r.flushMu.Lock() // a flush in progress holds counts that are in neither place
		defer r.flushMu.Unlock()
	}
	var rows []catalog.RequestDay
	if u := r.reader(); u != nil {
		var err error
		if rows, err = u.RequestDays(from, to); err != nil {
			return nil, err
		}
	}
	if r == nil {
		return rows, nil
	}
	r.mu.Lock()
	for day, t := range r.days {
		if day < from || day > to {
			continue
		}
		i := slices.IndexFunc(rows, func(d catalog.RequestDay) bool { return d.Day == day })
		if i < 0 {
			rows = append(rows, catalog.RequestDay{Day: day})
			i = len(rows) - 1
		}
		rows[i].Requests += t.count
		rows[i].BytesSent += t.bytes
	}
	r.mu.Unlock()
	slices.SortFunc(rows, func(a, b catalog.RequestDay) int { return strings.Compare(a.Day, b.Day) })
	return rows, nil
}

// Downloads are the downloads per repository and day from from to to, both
// included, as written plus what waits for the next flush.
func (r *Recorder) Downloads(from, to string) ([]catalog.Download, error) {
	if r != nil {
		r.flushMu.Lock() // a flush in progress holds counts that are in neither place
		defer r.flushMu.Unlock()
	}
	var rows []catalog.Download
	if u := r.reader(); u != nil {
		var err error
		if rows, err = u.Downloads(from, to); err != nil {
			return nil, err
		}
	}
	if r == nil {
		return rows, nil
	}
	r.mu.Lock()
	for k, t := range r.pending {
		if k.day < from || k.day > to {
			continue
		}
		i := slices.IndexFunc(rows, func(d catalog.Download) bool { return d.Day == k.day && d.Repo == k.repo })
		if i < 0 {
			rows = append(rows, catalog.Download{Repo: k.repo, Day: k.day})
			i = len(rows) - 1
		}
		rows[i].Count += t.count
		rows[i].Bytes += t.bytes
	}
	r.mu.Unlock()
	slices.SortFunc(rows, func(a, b catalog.Download) int {
		if c := strings.Compare(a.Day, b.Day); c != 0 {
			return c
		}
		return strings.Compare(a.Repo, b.Repo)
	})
	return rows, nil
}
