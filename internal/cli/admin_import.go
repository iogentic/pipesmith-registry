package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/importer"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// reportAdministration prints what an import did with the cleanup
// policies, capabilities and tasks of its export (leg 6-state): counts and
// names, never the value of a property.
func (c *command) reportAdministration(r *importer.Result) {
	has := func(k state.Kind) bool { return slices.Contains(r.Kinds, k) }
	var counts []string
	if has(state.KindCleanupPolicies) {
		counts = append(counts, fmt.Sprintf("cleanup policies %d", len(r.Snapshot.CleanupPolicies)))
	}
	if has(state.KindTasks) {
		line := fmt.Sprintf("tasks %d imported, all paused", r.ImportedTasks)
		var local []string
		for _, key := range r.LocalKept {
			if id, ok := strings.CutPrefix(key, "task "); ok {
				local = append(local, id)
			}
		}
		if len(local) > 0 {
			line += ", local tasks kept: " + strings.Join(local, ", ")
		}
		counts = append(counts, line)
	}
	if has(state.KindCapabilities) {
		counts = append(counts, fmt.Sprintf("capabilities %d", len(r.Snapshot.Capabilities)))
	}
	if len(counts) > 0 {
		c.print("%s", strings.Join(counts, ", "))
	}
	if has(state.KindTasks) && r.ImportedTasks > 0 {
		c.print("note: imported tasks arrive paused and run only when an administrator enables them; the old state is kept")
	}
	for _, p := range r.PoliciesNotRun {
		c.print("note: cleanup policy %s is kept and never runs; %s", p.Name, p.Reason)
	}
	for _, n := range r.TaskNotes {
		c.print("note: task %s keeps its schedule as stored, which this server does not take: %s", n.ID, n.Reason)
	}
	if len(r.CronTasks) > 0 {
		c.print("note: %s: %s", importer.CronNote, strings.Join(r.CronTasks, ", "))
	}
	if len(r.UnreadableTasks) > 0 {
		c.print("note: %d jobs hold job data the export could not read, not imported: %s", len(r.UnreadableTasks),
			strings.Join(r.UnreadableTasks, ", "))
	}
	if len(r.TriggersWithoutJob) > 0 {
		c.print("note: %d triggers name a job the export does not hold, not imported: %s", len(r.TriggersWithoutJob),
			strings.Join(r.TriggersWithoutJob, ", "))
	}
	var kept, removed []string
	for _, p := range r.SecretProperties {
		name := p.Kind + " " + p.ID + " " + p.Key
		if p.Kept {
			kept = append(kept, name)
		} else {
			removed = append(removed, name)
		}
	}
	if len(kept) > 0 {
		c.print("note: secret properties kept in secret files of %s/: %s", state.SecretsDir, strings.Join(kept, ", "))
	}
	if len(removed) > 0 {
		c.print("note: secret properties not kept, removed by the export or the import or of a row not written; "+
			"enter them again: %s", strings.Join(removed, ", "))
	}
	if len(r.LocalKept) > 0 {
		c.print("note: rows made on this server are kept over the import: %s", strings.Join(r.LocalKept, ", "))
	}
}
