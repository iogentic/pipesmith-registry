package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// LastRunFile below releases/ holds the outcome of the last scheduled run of
// the timer, for the screen Updates: serve reads it as the service user, so
// it has mode 0644 and holds no secret.
const LastRunFile = "scheduled.json"

// Results of a scheduled run.
const (
	RunNothing  = "nothing"  // no version above the running one
	RunStaged   = "staged"   // a version was staged; scheduled_switch is off
	RunWaiting  = "waiting"  // a version is staged and waits for the window
	RunSwitched = "switched" // the switch ended healthy
	RunBack     = "back"     // the switch failed and the way back ran
	RunFailed   = "failed"   // the run ended with an error before or without a switch
)

// LastRun is the content of releases/scheduled.json.
type LastRun struct {
	Time    string `json:"time"`
	Result  string `json:"result"`
	Version string `json:"version,omitempty"`
	Message string `json:"message,omitempty"`
}

const maxLastRun = 64 << 10

// WriteLastRun replaces releases/scheduled.json of root with r; the time is
// now when r has none, and a message is cut at 2000 bytes.
func WriteLastRun(root string, r LastRun) error {
	if r.Time == "" {
		r.Time = time.Now().UTC().Format(time.RFC3339)
	}
	if len(r.Message) > 2000 {
		r.Message = r.Message[:2000]
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	dir := Layout{Root: root}.Releases()
	tmp := filepath.Join(dir, "."+LastRunFile+".new")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil { // #nosec G306 -- serve reads it as the service user; it holds no secret.
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil { // #nosec G302 -- whatever the umask, serve reads it.
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, LastRunFile)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ReadLastRun reads releases/scheduled.json of root; nil without the file.
func ReadLastRun(root string) (*LastRun, error) {
	f, err := os.Open(filepath.Join(Layout{Root: root}.Releases(), LastRunFile)) // #nosec G304 -- a fixed file of the install root.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxLastRun+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxLastRun {
		return nil, fmt.Errorf("%s is larger than %d bytes", LastRunFile, maxLastRun)
	}
	var r LastRun
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %v", LastRunFile, err)
	}
	return &r, nil
}
