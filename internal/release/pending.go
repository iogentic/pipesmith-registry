package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PendingFile is the record of a switch in the install root: an update or a
// rollback writes it before the links change and removes it once the switch
// ended, or its way back did. A command that finds it ends the switch of the
// run that was killed before it does anything else.
const PendingFile = ".switch.json"

// ErrInterrupted is the refusal of a command that found a switch of a run
// that was killed, and ended it.
var ErrInterrupted = errors.New("a switch of a run that stopped was ended")

// ErrStaleSwitch is the refusal of a command that found the record of a
// switch older than the restart bound of that switch. It changes nothing.
var ErrStaleSwitch = errors.New("the record of a switch is older than the restart bound of that switch")

// pending is the content of PendingFile.
type pending struct {
	Action          string        `json:"action"` // update or rollback
	From            string        `json:"from"`   // the release current pointed to before
	To              string        `json:"to"`
	Previous        string        `json:"previous"` // the target of previous before, as written
	PreviousExisted bool          `json:"previous_existed"`
	Back            bool          `json:"back"` // the way back began
	Files           []pendingFile `json:"files"`
	Written         string        `json:"written"`       // when the record was written, RFC 3339 in UTC
	BoundSeconds    int           `json:"bound_seconds"` // the restart bound of the switch: drain, start timeout and margin
}

// maxClockAhead is how far the time of a record may lie ahead of the clock
// before its age counts as unknown.
const maxClockAhead = time.Minute

// pendingFile is a file of the schema plan: its name in the data directory
// and its version before the switch.
type pendingFile struct {
	Name string `json:"name"`
	From int    `json:"from"`
}

func (u *Updater) pendingPath(s *session) string { return s.l.path(PendingFile) }

// writePending records the switch of s to name before the links change.
func (u *Updater) writePending(s *session, action, name string) error {
	s.action = action
	if err := u.recordPending(s, name, false); err != nil {
		return err
	}
	u.began = true
	return nil
}

// PendingRecord is the path of the record of a switch in root and whether
// it exists.
func PendingRecord(root string) (string, bool) {
	path := Layout{Root: root}.path(PendingFile)
	_, err := os.Lstat(path)
	return path, err == nil
}

// EndSwitch ends the switch whose record lies in the root, as update and
// rollback do before anything else, and returns what it did: nil when the
// root holds no record, else ErrInterrupted, ErrStaleSwitch or the error of
// the way back. The scheduled run calls it first (finding R3-2).
func (u *Updater) EndSwitch() error {
	s, err := u.begin(false)
	if err != nil {
		return err
	}
	defer s.unlock()
	return u.endPending(s, &recorder{u: u, s: s, action: "update"})
}

// SwitchBegan reports whether the last update of u wrote the record of its
// switch: before that, a failure changed no link and restarted nothing.
func (u *Updater) SwitchBegan() bool { return u.began }

// markBack records that the way back of the switch to name began.
func (u *Updater) markBack(s *session, name string) error { return u.recordPending(s, name, true) }

func (u *Updater) recordPending(s *session, name string, back bool) error {
	u.holdSignals(s)
	p := pending{Action: s.action, From: s.current, To: name, Previous: s.prevTarget, PreviousExisted: s.prevExisted, Back: back,
		Files: []pendingFile{}, Written: u.clock().UTC().Format(time.RFC3339Nano), BoundSeconds: int(u.switchBound(s) / time.Second)}
	if s.plan != nil {
		for _, f := range s.plan.files {
			p.Files = append(p.Files, pendingFile{Name: f.name, From: f.from})
		}
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp := u.pendingPath(s) + ".new"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := writeSynced(tmp, bytes.NewReader(append(data, '\n')), 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, u.pendingPath(s)); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(s.l.Root)
}

// clearPending removes the record once the switch or its way back ended.
func (u *Updater) clearPending(s *session) {
	err := os.Remove(u.pendingPath(s))
	if err == nil {
		err = syncDir(s.l.Root)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		u.step("warning: %s was not removed: %v; the next command checks the switch again", PendingFile, err)
	}
}

// readPending reads the record of the root, nil when there is none. It must
// be a regular file of the owner of the root that neither group nor others
// may write, and name releases and files that the rules of this package
// accept.
func (u *Updater) readPending(s *session) (*pending, error) {
	path := u.pendingPath(s)
	st, err := s.statOf(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPermissions, err)
	}
	if !st.mode.IsRegular() {
		return nil, fmt.Errorf("%w: %s is no regular file (uid %d, mode %v)", ErrPermissions, path, st.uid, st.mode)
	}
	if err := ownedSafe(path, st, s.owner, true, false); err != nil {
		return nil, err
	}
	f, err := os.Open(path) // #nosec G304 -- the record of the install root, checked above.
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLayout, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfig+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLayout, err)
	}
	var p pending
	bad := func(why string) error {
		return fmt.Errorf("%w: %s is not a record of a switch (%s); look at the links current and previous and at the data directory, then move it away", ErrLayout, path, why)
	}
	if len(data) > maxConfig {
		return nil, bad("too large")
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, bad(err.Error())
	}
	if (p.Action != "update" && p.Action != "rollback") || !validRelease(p.From) || !validRelease(p.To) {
		return nil, bad("action, from or to")
	}
	if p.Written != "" {
		if _, err := time.Parse(time.RFC3339Nano, p.Written); err != nil {
			return nil, bad("written")
		}
	}
	if p.BoundSeconds < 0 {
		return nil, bad("bound_seconds")
	}
	for _, f := range p.Files {
		if (f.Name != StateFile && f.Name != CatalogFile) || f.From < 1 {
			return nil, bad("a file of the schema plan")
		}
	}
	return &p, nil
}

// endPending ends the switch of a run that was killed, when the root holds
// its record (decision P8-30). A record younger than the restart bound of
// its switch may still have a restart in flight: links that never changed
// stay; a release that answers healthy and active as the one switched to
// within the full health wait means the switch had ended; a way back that
// began goes on; any other state takes the way back of that switch, data
// files included. An older record, or one whose age is unknown, changes no
// link and no data file: only a release that answers healthy now, or links
// that never changed, end it; else the command refuses with ErrStaleSwitch
// and says what to run. Every case that found a record returns with what it
// did, and the command does nothing else.
func (u *Updater) endPending(s *session, rec *recorder) error {
	p, err := u.readPending(s)
	if err != nil || p == nil {
		return err
	}
	u.holdSignals(s)
	u.step("found: %s, the %s from %s/%s to %s/%s of a run that stopped", PendingFile, p.Action, ReleasesDir, p.From, ReleasesDir, p.To)
	young, written := u.youngRecord(p)
	if s.current == p.From && !p.Back {
		again := ""
		if s.prevExisted != p.PreviousExisted || s.prevTarget != p.Previous {
			if !s.prevExisted || s.prevTarget != ReleasesDir+"/"+p.From {
				err := u.staleRefusal(s, p, nil)
				rec.own(p.Action, ResultRefused, p.From, p.To, err)
				return err
			}
			// killed between the change of previous and the change of current
			if err := s.l.restoreLink(PreviousLink, p.Previous, p.PreviousExisted); err != nil {
				return fmt.Errorf("%w: the %s from %s/%s to %s/%s stopped after previous changed, and previous was not put back to %q: %v",
					ErrInterrupted, p.Action, ReleasesDir, p.From, ReleasesDir, p.To, p.Previous, err)
			}
			again = "; previous points to " + p.Previous + " again"
			if !p.PreviousExisted {
				again = "; previous was removed again, as before"
			}
			u.step("put back: previous%s", strings.TrimPrefix(again, "; previous"))
		}
		u.clearPending(s)
		when := "before the links changed"
		if again != "" {
			when = "after previous changed and before current changed"
		}
		err := fmt.Errorf("%w: the %s from %s/%s to %s/%s stopped %s%s; nothing was restarted; run the command again",
			ErrInterrupted, p.Action, ReleasesDir, p.From, ReleasesDir, p.To, when, again)
		rec.own(p.Action, ResultRefused, p.From, p.To, err)
		return err
	}
	var health error
	if s.current == p.To && !p.Back {
		version := u.versionOf(s, p.To)
		if young {
			wait := written.Add(time.Duration(p.BoundSeconds)*time.Second).Sub(u.clock()) + orDefault(u.HealthTimeout, DefaultHealthTimeout)
			u.step("waiting: up to %s for %s/%s to answer healthy; the record is from %s and its restart may still run",
				wait.Round(time.Second), ReleasesDir, p.To, p.Written)
			health = u.waitFor(s, version, wait)
		} else {
			health = u.healthyOnce(s, version)
		}
		if health == nil {
			u.clearPending(s)
			u.step("healthy: %s answers with version %s", s.base, version)
			err := fmt.Errorf("%w: the %s from %s/%s to %s/%s had ended: %s/%s answers healthy; run the command again for anything further",
				ErrInterrupted, p.Action, ReleasesDir, p.From, ReleasesDir, p.To, ReleasesDir, p.To)
			rec.own(p.Action, ResultOK, p.From, p.To, err)
			return err
		}
	}
	if !young || (!p.Back && s.current != p.To) {
		err := u.staleRefusal(s, p, health)
		rec.own(p.Action, ResultRefused, p.From, p.To, err)
		return err
	}
	s.action, s.current, s.prevTarget, s.prevExisted = p.Action, p.From, p.Previous, p.PreviousExisted
	s.plan = &schemaPlan{dataDir: s.dataDir}
	for _, f := range p.Files {
		s.plan.files = append(s.plan.files, planFile{name: f.Name, path: filepath.Join(s.dataDir, f.Name), from: f.From})
	}
	cause := fmt.Errorf("%w: the run that switched stopped before it ended", ErrInterrupted)
	err = u.wayBack(s, p.To, "", cause)
	rec.own(p.Action, ResultBack, p.From, p.To, err)
	return err
}

// youngRecord reports whether the restart of the switch of p can still be in
// flight: p holds the time it was written and its restart bound, and that
// time lies less than the bound ago and not ahead of the clock by more than
// maxClockAhead.
func (u *Updater) youngRecord(p *pending) (bool, time.Time) {
	written, err := time.Parse(time.RFC3339Nano, p.Written)
	if err != nil || p.BoundSeconds <= 0 {
		return false, time.Time{}
	}
	age := u.clock().Sub(written)
	return age > -maxClockAhead && age < time.Duration(p.BoundSeconds)*time.Second, written
}

// staleRefusal is the refusal for a record the command does not end by
// itself: it names the record, the links and the answer of the server, and
// what the operator runs.
func (u *Updater) staleRefusal(s *session, p *pending, health error) error {
	path := u.pendingPath(s)
	when := "at an unknown time"
	if p.Written != "" {
		when = "at " + p.Written
	}
	why := fmt.Sprintf("it was written %s, and the restart of that switch may take %d s", when, p.BoundSeconds)
	if p.BoundSeconds <= 0 {
		why = fmt.Sprintf("it was written %s and holds no restart bound", when)
	}
	if young, _ := u.youngRecord(p); young {
		why = "the links match neither side of the switch"
	}
	prev := "nothing"
	if target, err := s.l.rawLink(PreviousLink); err == nil {
		prev = target
	}
	answer := ""
	if health != nil {
		answer = fmt.Sprintf("; %s/%s does not answer healthy: %v", ReleasesDir, p.To, health)
	}
	next := fmt.Sprintf("when %s/%s runs as it should, the switch ended: remove the record with: sudo rm %s; to go back to %s/%s instead, remove the record, then run: sudo %s rollback --root %s",
		ReleasesDir, p.To, path, ReleasesDir, p.From, InstalledBinary, s.l.Root)
	if p.Back {
		next = fmt.Sprintf("the way back of that switch began: put the links and the data files of the plan in order by hand as docs/RUNBOOK-HOST.md, section Copies, says, then remove the record with: sudo rm %s", path)
	}
	return fmt.Errorf("%w: %s records the %s from %s/%s to %s/%s; %s; current points to %s, previous to %s%s; links and data files stay as they are; check the unit with systemctl status %s and the data directory %s; %s",
		ErrStaleSwitch, path, p.Action, ReleasesDir, p.From, ReleasesDir, p.To, why, ReleasesDir+"/"+s.current, prev, answer,
		s.cfg.Unit, s.dataDir, next)
}

// waitFor waits up to timeout for version to answer healthy while the
// service manager reports the unit active.
func (u *Updater) waitFor(s *session, version string, timeout time.Duration) error {
	w := *u
	w.HealthTimeout = timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout+orDefault(u.RequestTimeout, DefaultRequestTimeout))
	defer cancel()
	return w.waitHealth(s.base, s.pin, version, u.activeCheck(ctx, s.cfg.Unit))
}

// switchBound is the restart bound of a switch of s: what a restart may take.
func (u *Updater) switchBound(s *session) time.Duration {
	return orDefault(u.RestartTimeout, s.cfg.restartTimeout())
}

// clock is the time of the Updater, time.Now unless a test sets now.
func (u *Updater) clock() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// healthyOnce asks the server once whether it answers healthy with version
// while the service manager reports the unit active.
func (u *Updater) healthyOnce(s *session, version string) error {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	if s.pin != nil {
		transport.TLSClientConfig = s.pin.tlsConfig()
	}
	client := &http.Client{Timeout: orDefault(u.RequestTimeout, DefaultRequestTimeout), Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if err := probe(client, s.base, version); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), orDefault(u.RequestTimeout, DefaultRequestTimeout))
	defer cancel()
	return u.activeCheck(ctx, s.cfg.Unit)()
}
