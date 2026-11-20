package logs

// The files are off while Options.Dir is empty: Open makes no directory and
// opens no file, every writer hands its JSON lines to the tap of
// Options.Ship and keeps nothing, nothing rotates and nothing is kept, and
// the classic line is not made. Errors stays 0 and Health green. Without a
// tap a writer returns at once.

// off reports whether the files are off.
func (f *Files) off() bool { return f.dir == "" }

// idle reports whether the files are off and no tap takes the lines, so a
// writer has nothing to make.
func (f *Files) idle() bool { return f.dir == "" && f.tap == nil }

// openOff gives files that are off their four writers, each without a file.
func (f *Files) openOff() *Files {
	writer := func(kind string) *logFile { return &logFile{files: f, kind: kind} }
	f.server, f.requests, f.audit, f.tasks = writer(kindServer), writer(kindRequests), writer(kindAudit), writer(kindTasks)
	return f
}
