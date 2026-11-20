package server

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// update.json of root with mode 0600, as the runbook allows, is not readable
// by serve: the answer says so and why, and does not call the key refused
// (finding R3-10).
func TestUpdatesWithAConfigTheServerCannotRead(t *testing.T) {
	root, _, _ := installLayout(t)
	saved := loadUpdateConfig
	loadUpdateConfig = func(root string) (*release.UpdateConfig, error) {
		p := filepath.Join(root, release.ConfigFile)
		return nil, fmt.Errorf("%w: cannot read %s: %w", release.ErrConfig, p, &fs.PathError{Op: "open", Path: p, Err: syscall.EACCES})
	}
	t.Cleanup(func() { loadUpdateConfig = saved })
	got := updatesOf(t, startUpdates(t, root, modeRule), asAdmin)
	if answerField(t, got, "key.state") != "unreadable" || answerField(t, got, "config.found") != true {
		t.Fatalf("answer %v", got)
	}
	problem, _ := answerField(t, got, "key.problem").(string)
	for _, words := range []string{"cannot read", "chmod 0644 " + filepath.Join(root, release.ConfigFile), "update and rollback read it"} {
		if !strings.Contains(problem, words) {
			t.Fatalf("problem %q does not say %q", problem, words)
		}
	}
}
