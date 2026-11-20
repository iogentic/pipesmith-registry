package logs

import "strings"

// Check is the state of one health check.
type Check struct {
	Name    string
	Healthy bool
	Detail  string // why it is red: the file and the error of the system, never a line
}

// Health is the check of the log directory (L6): red while the last open or
// write of a file failed, green again after the next success. No route shows
// it yet. A failed compression turns it red too (recompress.go).
func (f *Files) Health() Check {
	var details []string
	for _, lf := range f.all() {
		if d := lf.failure.Load(); d != nil {
			details = append(details, *d)
		}
	}
	details = append(details, f.maint.failures()...)
	return Check{Name: "log directory", Healthy: len(details) == 0, Detail: strings.Join(details, "; ")}
}
