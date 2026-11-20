package logship

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// Logger names the lines of the manager and its senders. The tap never
// queues a line of this logger, so a dead target cannot feed itself.
const Logger = "pipesmith_registry.logship"

// reloadEvery is the time of the clock between two reads of the setting.
const reloadEvery = 5 * time.Second

// maxSecret is the most bytes of a secret.
const maxSecret = 4096

// Options configure a Manager.
type Options struct {
	Rows    func() ([]byte, error)            // the value of the setting log_targets; nil for none
	Secret  func(name string) (string, error) // the content of the secret file name below secrets/; its error names the file, never the content
	Logger  *slog.Logger                      // the manager adds the logger attribute Logger; nil logs nothing
	Version string                            // the version of the build, for User-Agent
	Host    string                            // the host name the adapters send; empty is the name of the machine
}

// Manager runs one sender per target of the setting and is the tap of the
// log files. Its methods are safe for use by several goroutines.
type Manager struct {
	rows   func() ([]byte, error)
	secret func(name string) (string, error)
	log    *slog.Logger
	env    env
	clock  clock

	senders atomic.Pointer[[]*sender] // what the tap reaches, in the order of the setting

	mu     sync.Mutex // closed, warned and the change of senders
	closed bool
	warned map[string]bool // the warnings of the last reload
	quit   chan struct{}
	loops  sync.WaitGroup // the reload loop and every sender
}

// New starts the senders of the targets of the setting and reads it again
// every 5 s.
func New(opts Options) *Manager { return newManager(opts, realClock{}) }

func newManager(opts Options, clk clock) *Manager {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	host := opts.Host
	if host == "" {
		host, _ = os.Hostname() // a machine without a name sends localhost
	}
	if host == "" {
		host = "localhost"
	}
	m := &Manager{rows: opts.Rows, secret: opts.Secret, log: log.With(slog.String("logger", Logger)),
		env: env{host: host, version: opts.Version, pid: os.Getpid(), clock: clk}, clock: clk,
		warned: map[string]bool{}, quit: make(chan struct{})}
	m.senders.Store(&[]*sender{})
	m.reload()
	m.loops.Add(1)
	go m.loop()
	return m
}

// Ship is the tap of the log files: it queues the line for every running
// target that takes its kind and returns at once. A full queue drops the
// line; a line of the logger Logger is never queued.
func (m *Manager) Ship(line logs.Line) {
	if line.Logger == Logger {
		return
	}
	bit := fileBits[line.Kind]
	for _, s := range *m.senders.Load() {
		if s.mask&bit != 0 {
			s.offer(line)
		}
	}
}

// Stats are the counters of the running targets, in the order of the
// setting. It takes no lock.
func (m *Manager) Stats() []Stats {
	list := *m.senders.Load()
	out := make([]Stats, 0, len(list))
	for _, s := range list {
		out = append(out, s.stats())
	}
	return out
}

// Close stops the tap, gives every sender 2 s in total to send what is
// queued, then cancels the attempts in flight and returns once every
// goroutine of the manager ended.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.quit)
	list := *m.senders.Swap(&[]*sender{})
	m.mu.Unlock()
	for _, s := range list {
		s.stop()
	}
	m.loops.Wait()
}

// loop reads the setting every 5 s of the clock until Close.
func (m *Manager) loop() {
	defer m.loops.Done()
	for {
		select {
		case <-m.quit:
			return
		case <-m.clock.After(reloadEvery):
			m.reload()
		}
	}
}

// warning is one line that a reload logs once while its cause lasts.
type warning struct {
	msg   string
	attrs []any
}

func (w warning) key() string { return w.msg + fmt.Sprint(w.attrs...) }

// row is one target to run, with its secret.
type row struct {
	target Target
	secret string
}

// reload reads the setting and the secret files. A new row starts a sender,
// a changed row or secret restarts it, a removed row stops it. A row that
// cannot be used is left out with one warning; a setting that cannot be read
// leaves the senders as they are. The tap reaches the new sender of a
// changed row from the swap of the list on; the old one sends what its queue
// holds within its 2 s, and a line that a tap hands it after its queue closed
// goes to the new one, so each line reaches one of the two.
func (m *Manager) reload() {
	var value []byte
	var err error
	if m.rows != nil {
		value, err = m.rows()
	}
	if err != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		w := warning{"log targets not read", []any{slog.String("error", err.Error())}}
		if !m.warned[w.key()] {
			m.log.Warn(w.msg, w.attrs...)
			m.warned[w.key()] = true
		}
		return
	}
	targets, problems := ParseTargets(value)
	var warnings []warning
	for _, p := range problems {
		warnings = append(warnings, warning{"log target skipped", []any{slog.String("error", p.Error())}})
	}
	var rows []row
	for _, t := range targets {
		secret, err := m.secretOf(t)
		if err != nil {
			warnings = append(warnings, warning{"log target left out", []any{slog.String("target", t.Name), slog.String("error", err.Error())}})
			continue
		}
		rows = append(rows, row{t, secret})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	old := map[string]*sender{}
	for _, s := range *m.senders.Load() {
		old[s.target.Name] = s
	}
	next := []*sender{}
	for _, r := range rows {
		if s := old[r.target.Name]; s != nil && sameRow(s, r) {
			next = append(next, s)
			delete(old, r.target.Name)
			continue
		}
		s, err := m.start(r)
		if err != nil {
			warnings = append(warnings, warning{"log target not started", []any{slog.String("target", r.target.Name), slog.String("error", err.Error())}})
			continue
		}
		if prev := old[r.target.Name]; prev != nil {
			prev.next.Store(s) // a line that finds the queue of prev closed goes to s
		}
		next = append(next, s)
	}
	m.senders.Store(&next)
	for _, s := range old {
		s.stop()
	}
	seen := map[string]bool{}
	for _, w := range warnings {
		if !m.warned[w.key()] && !seen[w.key()] {
			m.log.Warn(w.msg, w.attrs...)
		}
		seen[w.key()] = true
	}
	m.warned = seen
}

// secretOf reads the secret of a target: 1 to 4096 bytes without a control
// character.
func (m *Manager) secretOf(t Target) (string, error) {
	if t.Secret == "" {
		return "", nil
	}
	if m.secret == nil {
		return "", errors.New("no secret files")
	}
	value, err := m.secret(t.Secret)
	switch {
	case err != nil:
		return "", err
	case value == "":
		return "", fmt.Errorf("the secret file %s is empty", t.Secret)
	case len(value) > maxSecret:
		return "", fmt.Errorf("the secret file %s holds more than %d bytes", t.Secret, maxSecret)
	case strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "", fmt.Errorf("the secret file %s holds a control character", t.Secret)
	}
	return value, nil
}

// sameRow reports whether a running sender serves r unchanged.
func sameRow(s *sender, r row) bool {
	a, errA := json.Marshal(s.target) // #nosec G117 -- Secret is the name of a file below secrets/, not its content; the bytes are only compared
	b, errB := json.Marshal(r.target) // #nosec G117 -- Secret is the name of a file below secrets/, not its content; the bytes are only compared
	return errA == nil && errB == nil && string(a) == string(b) && s.secret == r.secret
}

// start opens the adapter of r and starts its sender; m.mu is held.
func (m *Manager) start(r row) (*sender, error) {
	a, err := openAdapter(r.target, r.secret, m.env)
	if err != nil {
		return nil, err
	}
	s := newSender(r.target, r.secret, a, m.clock, m.log)
	m.loops.Add(1)
	go func() {
		defer m.loops.Done()
		s.run()
	}()
	return s, nil
}
