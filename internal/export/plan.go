package export

import "fmt"

// Limits bound one export. They are checked against the index before the
// first byte of the archive.
type Limits struct {
	MaxFiles int64
	MaxBytes int64
}

// DefaultLimits are export_max_files and export_max_bytes of the plan:
// 20,000 files and 2 GiB.
func DefaultLimits() Limits { return Limits{MaxFiles: 20_000, MaxBytes: 2 << 30} }

// LimitError is an export above a limit. Files and Bytes count up to the
// first entry after the limit was passed; Complete says whether that count
// covers every entry.
type LimitError struct {
	Files, Bytes       int64
	MaxFiles, MaxBytes int64
	Complete           bool
}

func (e *LimitError) Error() string {
	more := ""
	if !e.Complete {
		more = " or more"
	}
	return fmt.Sprintf("the export holds %d files%s and %d bytes%s; the limits are %d files and %d bytes",
		e.Files, more, e.Bytes, more, e.MaxFiles, e.MaxBytes)
}

// Tally counts the entries of an export against its limits.
type Tally struct {
	limits     Limits
	files      int64
	bytes      int64
	over       bool
	incomplete bool
}

// NewTally starts a count.
func NewTally(limits Limits) *Tally { return &Tally{limits: limits} }

// Add counts one entry. It returns false when the caller should stop: an
// entry came after a limit was already passed, so the count stops there.
func (t *Tally) Add(size int64) bool {
	if t.over {
		t.incomplete = true
		return false
	}
	t.files++
	t.bytes += size
	t.over = t.files > t.limits.MaxFiles || t.bytes > t.limits.MaxBytes
	return true
}

// Files and Bytes are the counts so far.
func (t *Tally) Files() int64 { return t.files }

// Bytes is the sum of the sizes so far.
func (t *Tally) Bytes() int64 { return t.bytes }

// Err is a *LimitError when a limit was passed, else nil.
func (t *Tally) Err() error {
	if !t.over {
		return nil
	}
	return &LimitError{Files: t.files, Bytes: t.bytes, MaxFiles: t.limits.MaxFiles, MaxBytes: t.limits.MaxBytes, Complete: !t.incomplete}
}
