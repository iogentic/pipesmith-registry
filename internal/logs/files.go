package logs

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// The kinds of log file; the active file of a kind is <kind>.log.
const (
	kindServer   = "server"
	kindRequests = "requests"
	kindClassic  = "requests-classic"
	kindAudit    = "audit"
	kindTasks    = "tasks"
)

// Options configure Open.
type Options struct {
	Dir      string // the log directory, made with mode 0750 when missing; empty turns the files off (off.go)
	Settings Settings
	Now      func() time.Time // nil is time.Now
	Ship     func(Line)       // gets every JSON line as it is made, outside the locks; it must return at once
}

// Files are the log files of one server process. Its methods are safe for
// use by several goroutines.
type Files struct {
	dir      string
	settings Settings
	now      func() time.Time
	errors   atomic.Uint64
	maint    maint // the compression of rotated files and the retention
	tap      func(Line)
	lock     *os.File // the lock of the log directory (lock.go)

	server, requests, classic, audit, tasks *logFile // classic is nil while Settings.Classic is false

	pending sync.Mutex // held by takePending (pending.go)

	mu       sync.Mutex // stop, stopped, closed, retained
	stop     chan struct{}
	stopped  chan struct{}
	closed   bool
	retained time.Time // the hour of the clock of the last retention
}

// Open makes the log directory and opens the files for appending. It starts
// no goroutine: the compression of what it rotates and of the leftovers of a
// crash, and the retention, start with Start, Tick or Close. A file that
// cannot be opened counts every line written to it as an error. A limit of
// Settings that is not positive takes its default.
func Open(opts Options) *Files {
	f := &Files{dir: opts.Dir, settings: opts.Settings.withDefaults(), now: opts.Now}
	f.tap = opts.Ship
	if f.now == nil {
		f.now = time.Now
	}
	f.maint.retention = f.retain
	f.maint.sweeper = f.sweep
	if f.off() {
		return f.openOff()
	}
	_ = os.MkdirAll(opts.Dir, 0o750) // a directory that cannot be made fails the open of each file
	if !f.lockDir() {
		f.dir = "" // another server holds the directory: the files are off
		return f.openOff()
	}
	f.leftovers()
	open := func(kind string) *logFile { return openLogFile(f, kind) }
	f.server, f.requests, f.audit, f.tasks = open(kindServer), open(kindRequests), open(kindAudit), open(kindTasks)
	if f.settings.Classic {
		f.classic = open(kindClassic)
	}
	f.retained = f.now().UTC().Truncate(time.Hour)
	f.maint.retainSoon()
	if f.tap == nil {
		f.takePending() // with a tap the first Tick takes them, once the targets listen
	}
	return f
}

// all lists the files that are open, requests-classic.log only while it is
// written.
func (f *Files) all() []*logFile {
	all := []*logFile{f.server, f.requests, f.audit, f.tasks}
	if f.classic != nil {
		all = append(all, f.classic)
	}
	return all
}

// Handler is the slog.Handler of server.log.
func (f *Files) Handler() slog.Handler { return &handler{files: f} }

// Request writes the line of one request to requests.log, and its classic
// line to requests-classic.log while Settings.Classic is true. The lines
// wait for the next flush: a request never waits for the disk.
func (f *Files) Request(r Request) {
	if f.idle() {
		return
	}
	f.requests.add(r.line(f.now()))
	if f.classic != nil {
		f.classic.add(r.classicLine(f.now()))
	}
}

// AuditEvent is one line of audit.log.
type AuditEvent struct {
	RequestID  string
	Subject    string
	Realm      string
	Client     string
	Action     string // <domain>.<verb>
	ObjectType string
	ObjectName string
	Result     string   // ok, denied or failed
	Fields     []string // the names of the changed fields, never their values
	Reason     string   // why the action ended so; a line without it has no key reason
}

// Audit writes the line of one event to audit.log before it returns. A
// result other than ok, denied and failed is refused: nothing is written and
// the line counts as an error.
func (f *Files) Audit(e AuditEvent) error {
	switch e.Result {
	case "ok", "denied", "failed":
	default:
		f.errors.Add(1)
		return fmt.Errorf("audit line refused: result %q is not ok, denied or failed", e.Result)
	}
	if f.idle() {
		return nil
	}
	return f.audit.flush(e.line(f.now()))
}

// TaskEvent is one line of tasks.log.
type TaskEvent struct {
	RunID    string
	TaskID   string
	TaskType string
	Event    string // start, step or end
	Result   string
	Counts   map[string]int64
	Duration time.Duration
}

// Task writes the line of one event to tasks.log; it waits for the next
// flush. An event other than start, step and end is refused: nothing is
// written and the line counts as an error.
func (f *Files) Task(e TaskEvent) error {
	switch e.Event {
	case "start", "step", "end":
	default:
		f.errors.Add(1)
		return fmt.Errorf("task line refused: event %q is not start, step or end", e.Event)
	}
	if f.idle() {
		return nil
	}
	f.tasks.add(e.line(f.now()))
	return nil
}

// Tick does the work of one second: it writes the lines that wait, rotates
// a file at the day change and, once per hour of the clock, starts the
// retention.
func (f *Files) Tick() {
	if f.off() {
		return
	}
	f.takePending()
	for _, lf := range f.all() {
		_ = lf.flush(nil) // a failed write is counted by the file
	}
	f.retainHourly()
	f.maint.kick()
}

// Start runs Tick every second until Close.
func (f *Files) Start() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stop != nil || f.closed || f.off() {
		return
	}
	f.stop, f.stopped = make(chan struct{}), make(chan struct{})
	f.maint.kick()
	go func(stop, stopped chan struct{}) {
		defer close(stopped)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				f.Tick()
			case <-stop:
				return
			}
		}
	}(f.stop, f.stopped)
}

// Close stops Start, writes the lines that wait, closes the files and waits
// for the compression of the rotated files and the retention. A line after
// Close is dropped and counted.
func (f *Files) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	stop, stopped := f.stop, f.stopped
	f.mu.Unlock()
	if stop != nil {
		close(stop)
		<-stopped
	}
	var err error
	for _, lf := range f.all() {
		err = errors.Join(err, lf.close())
	}
	f.maint.kick()
	f.maint.wait()
	f.unlock()
	return err
}

// Errors is the number of lines that did not reach their file.
func (f *Files) Errors() uint64 { return f.errors.Load() }
