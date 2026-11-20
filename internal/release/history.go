package release

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// The history of the install root: update and rollback, which run as the
// owner of the root outside serve, append one line per switch, refusal and
// way back to releases/history.jsonl. serve reads the lines it has not seen
// and writes one audit line and one server log line for each (ShipHistory),
// so the update writes nothing into the log directory of the service user.
const (
	HistoryFile   = "history.jsonl"
	HistoryCursor = "update-history.cursor" // in the log directory: the id of the last line serve wrote
	HistoryLogger = "pipesmith_registry.update"
)

// maxHistory bounds what ShipHistory reads: the end of the file.
const maxHistory = 4 << 20

// Results of a history line.
const (
	ResultOK      = "ok"      // the switch ended healthy
	ResultRefused = "refused" // refused before the links changed
	ResultBack    = "back"    // the switch failed and the way back ran
	ResultFailed  = "failed"  // a restart in place failed; the links did not change
)

// HistoryEntry is one line of releases/history.jsonl.
type HistoryEntry struct {
	ID     string `json:"id"` // time in UTC and random hex; ids sort in the order of their time
	Time   string `json:"time"`
	Action string `json:"action"` // update or rollback
	Result string `json:"result"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

func newEntry(action, result, from, to, reason string) HistoryEntry {
	now := time.Now().UTC()
	var b [4]byte
	_, _ = rand.Read(b[:]) // crypto/rand does not fail
	return HistoryEntry{ID: now.Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(b[:]),
		Time: now.Format(time.RFC3339Nano), Action: action, Result: result, From: from, To: to, Reason: reason}
}

// record appends e to releases/history.jsonl. A failed write does not fail
// the update; it prints one warning.
func (u *Updater) record(s *session, e HistoryEntry) {
	if err := appendHistory(s.l.Releases(), e); err != nil {
		u.step("warning: %s/%s: %v", ReleasesDir, HistoryFile, err)
	}
}

func appendHistory(dir string, e HistoryEntry) error {
	path := filepath.Join(dir, HistoryFile)
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%s is no regular file", path)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile(HistoryFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	// serve reads the file as the service user, whatever the umask of the caller (finding R3-9)
	if err := f.Chmod(0o644); err != nil { // #nosec G302 -- the history holds no secret and serve reads it.
		f.Close()
		return err
	}
	line, err := json.Marshal(e)
	if err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// recorder writes the history line of one update or rollback once, from the
// error it returns.
type recorder struct {
	u       *Updater
	s       *session
	action  string
	to      string
	back    bool // the switch changed the links
	inPlace bool // a restart of the release current runs already
	done    bool
}

// own writes the line of a switch of a run that stopped, which the command
// ended, in place of its own line.
func (r *recorder) own(action, result, from, to string, err error) {
	r.done = true
	r.u.record(r.s, newEntry(action, result, from, to, err.Error()))
}

func (r *recorder) finish(err error) {
	if r.done || r.s == nil {
		return
	}
	r.done = true
	var result, reason string
	switch {
	case err == nil:
		result, reason = ResultOK, fmt.Sprintf("%s/%s answers healthy", ReleasesDir, r.to)
	case r.back:
		result, reason = ResultBack, err.Error()
	case r.inPlace:
		result, reason = ResultFailed, err.Error()
	default:
		result, reason = ResultRefused, err.Error()
	}
	r.u.record(r.s, newEntry(r.action, result, r.s.current, r.to, reason))
}

// auditResult maps a history result on ok, denied or failed.
func auditResult(result string) string {
	switch result {
	case ResultOK:
		return "ok"
	case ResultRefused:
		return "denied"
	}
	return "failed"
}

// HistoryShipper ships the history of updates to the log files of serve,
// tick after tick. It keeps the id of the last line it wrote, so a cursor
// that cannot be written does not ship the same lines on every tick, and it
// reads the file only when its size or time changed (finding R3-11).
type HistoryShipper struct {
	History string // releases/history.jsonl of the install root
	Cursor  string // the cursor in the log directory
	Files   *logs.Files

	last   string // the id of the last line written
	loaded bool   // last holds the cursor file or a later id
	seen   bool   // size and mod hold the file of the last read
	size   int64
	mod    time.Time
}

// ShipHistory writes one audit line and one server log line to files for
// each line of the history file after the one whose id the cursor holds,
// and then puts the id of the last into cursor. It returns the number of
// lines shipped.
func ShipHistory(history, cursor string, files *logs.Files) (int, error) {
	return (&HistoryShipper{History: history, Cursor: cursor, Files: files}).Ship()
}

// Ship writes one audit line and one server log line for each line of the
// history not shipped yet. Lines count in the order of the file: those
// after the line whose id the cursor holds, so a line written after the
// clock was set back is shipped too; when that line is no longer in the
// tail that is read, the lines whose id sorts after the cursor. A missing
// history file ships nothing.
func (h *HistoryShipper) Ship() (int, error) {
	info, err := os.Stat(h.History)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if h.seen && info.Size() == h.size && info.ModTime().Equal(h.mod) {
		return 0, nil
	}
	if !h.loaded {
		if c, err := os.ReadFile(h.Cursor); err == nil { // #nosec G304 -- the cursor in the log directory of serve.
			h.last = strings.TrimSpace(string(c))
		} else if !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		h.loaded = true
	}
	data, err := tailOf(h.History)
	if err != nil {
		return 0, err
	}
	h.seen, h.size, h.mod = true, info.Size(), info.ModTime()
	var entries []HistoryEntry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), maxHistory)
	from := -1
	for sc.Scan() {
		var e HistoryEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.ID == "" {
			continue
		}
		if h.last != "" && e.ID == h.last {
			from = len(entries)
		}
		entries = append(entries, e)
	}
	logger := slog.New(h.Files.Handler())
	shipped, before := 0, h.last
	for i, e := range entries {
		if (from >= 0 && i <= from) || (from < 0 && e.ID <= before) {
			continue
		}
		_ = h.Files.Audit(logs.AuditEvent{RequestID: e.ID, Subject: "update", Action: "release." + e.Action, ObjectType: "release",
			ObjectName: e.From + " -> " + e.To, Result: auditResult(e.Result), Reason: e.Reason}) // a failed line is counted by Files
		level := slog.LevelInfo
		if e.Result != ResultOK {
			level = slog.LevelWarn
		}
		logger.Log(context.Background(), level, fmt.Sprintf("%s %s: %s -> %s", e.Action, e.Result, e.From, e.To),
			"logger", HistoryLogger, "id", e.ID, "from", e.From, "to", e.To, "result", e.Result, "reason", e.Reason)
		shipped, h.last = shipped+1, e.ID
	}
	if shipped == 0 {
		return 0, nil
	}
	return shipped, writeCursor(h.Cursor, h.last)
}

// tailOf reads at most the last maxHistory bytes of path, from the start of
// a line.
func tailOf(path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- the history file of the install root.
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is no regular file", path)
	}
	start := info.Size() - maxHistory
	if start < 0 {
		start = 0
	}
	data, err := io.ReadAll(io.NewSectionReader(f, start, maxHistory))
	if err != nil {
		return nil, err
	}
	if start > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return data, nil
}

func writeCursor(path, id string) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LastSwitch is the newest line of history.jsonl in dir whose switch ended
// healthy, or nil when there is none or no file.
func LastSwitch(dir string) (*HistoryEntry, error) {
	data, err := tailOf(filepath.Join(dir, HistoryFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var last *HistoryEntry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), maxHistory)
	for sc.Scan() {
		var e HistoryEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Result == ResultOK {
			last = &e
		}
	}
	return last, sc.Err()
}
