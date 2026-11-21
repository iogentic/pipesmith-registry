package server

import (
	"context"
	"errors"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	adminrules "github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/cleanup"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// The deleting task kinds of leg 6-cleanup on the host (rulings CL1, CL4,
// CL7, CL9 and CL10): every removal goes through removePlan, a unit of
// work per asset, and every message counts only.

// refusedRun reports an error of RunCleanup that refuses the pair of policy
// and repository before anything is selected: the rules of package admin
// and the fixed texts of RunCleanup for them.
func refusedRun(err error) bool {
	var refusals adminrules.Refusals
	var refusal *adminrules.Refusal
	if errors.As(err, &refusals) || errors.As(err, &refusal) {
		return true
	}
	var f *tasks.Failure
	return errors.As(err, &f) && f.Cause == nil &&
		(f.Text == "the policy cannot run" || f.Text == "the repository of the policy does not exist")
}

// CleanupAll is tasks.Host.CleanupAll: each policy of the state, by name,
// over each repository whose attributes name it, all of them for "*" or the
// one given. A refused pair is counted and the run goes on; any other
// failure, a freeze and a stop end it.
func (s *Server) CleanupAll(ctx context.Context, repository string) (tasks.CleanupCounts, error) {
	var c tasks.CleanupCounts
	if s.sec == nil {
		return c, errCleanupNoState
	}
	snap := s.sec.Snapshot()
	if snap == nil {
		return c, errCleanupNoState
	}
	policies := slices.Clone(snap.CleanupPolicies)
	slices.SortFunc(policies, func(a, b model.CleanupPolicy) int { return strings.Compare(a.Name, b.Name) })
	for _, policy := range policies {
		for _, repo := range adminrules.PolicyUsers(snap, policy.Name) {
			if repository != "*" && repo != repository {
				continue
			}
			if err := ctx.Err(); err != nil {
				return c, err
			}
			counts, err := s.CleanupRun(ctx, policy, repo)
			if err != nil && refusedRun(err) {
				c.Refused++
				continue
			}
			c.Runs++
			c.Selected += counts.Selected
			c.Removed += counts.Removed
			c.Kept += counts.Kept
			c.Skipped += counts.Skipped
			if err != nil {
				return c, err
			}
		}
	}
	return c, nil
}

// Compact is tasks.Host.Compact: CompactStore with the age of the task.
func (s *Server) Compact(ctx context.Context, store string, age time.Duration) (tasks.CompactCounts, error) {
	c, err := s.CompactStore(ctx, store, age)
	return tasks.CompactCounts{Removed: c.Removed, Kept: c.Kept, Skipped: c.Skipped, Bytes: c.Bytes}, err
}

// snapshotBuild matches the timestamp and the build number of a file of a
// timestamped snapshot after <artifact>-<base version>-.
var snapshotBuild = regexp.MustCompile(`^([0-9]{8}\.[0-9]{6})-([0-9]+)`)

// snapshotFile is a file of one build of a snapshot version.
type snapshotFile struct {
	version string // <group>/<artifact>/<base>-SNAPSHOT
	build   string // <timestamp>-<number>
	at      time.Time
	number  int
	row     index.Asset
}

// snapshotFileOf is the build a path of a timestamped snapshot belongs to,
// its checksum files included; false for any other path.
func snapshotFileOf(row index.Asset) (snapshotFile, bool) {
	base, _ := maven.SplitChecksum(row.Path)
	c := maven.CoordinatesOf(base)
	if c == nil || !strings.HasSuffix(c.Version, "-SNAPSHOT") {
		return snapshotFile{}, false
	}
	prefix := c.ArtifactID + "-" + strings.TrimSuffix(c.Version, "-SNAPSHOT") + "-"
	name, ok := strings.CutPrefix(path.Base(base), prefix)
	if !ok {
		return snapshotFile{}, false
	}
	m := snapshotBuild.FindStringSubmatch(name)
	if m == nil {
		return snapshotFile{}, false
	}
	at, err := time.Parse("20060102.150405", m[1])
	if err != nil {
		return snapshotFile{}, false
	}
	number := 0
	for _, d := range m[2] {
		if number > 1e8 {
			return snapshotFile{}, false
		}
		number = number*10 + int(d-'0')
	}
	return snapshotFile{version: path.Dir(base), build: m[1] + "-" + m[2], at: at, number: number, row: row}, true
}

// RemoveSnapshots is tasks.Host.RemoveSnapshots (ruling CL7): per snapshot
// version of a hosted maven2 repository the newest keep builds stay, by
// timestamp and build number; of the others each build older than age
// goes with all its files, then the metadata of the touched artifacts is
// written again (ruling KN18).
func (s *Server) RemoveSnapshots(ctx context.Context, repository string, keep int, age time.Duration) (tasks.SnapshotCounts, error) {
	var c tasks.SnapshotCounts
	if s.writes.writer == nil {
		return c, failure("the removal sets delete marks in the write store, and writes are off")
	}
	st, err := s.rp.sync()
	if err != nil {
		return c, failed("the repositories cannot be read", err)
	}
	repo, ok := st.repositories.Get(repository)
	switch {
	case !ok:
		return c, failure("the repository of the task does not exist")
	case repo.Format != "maven2" || repo.Type != "hosted":
		return c, failure("the repository of the task is no hosted maven2 repository")
	case !repo.TakesWrites():
		return c, failure("the repository of the task takes no write now")
	}
	rows, err := s.rp.index.RepoRows(repository)
	if err != nil {
		return c, failed("the index cannot be read", err)
	}
	writeStore := repo.WriteSettings().WriteStore // local-write or the created store of the repository (SR2)
	files := map[string]snapshotFile{}            // by path, the row of the write store first
	for _, r := range rows {
		f, ok := snapshotFileOf(r)
		if !ok {
			continue
		}
		if have, seen := files[r.Path]; seen && have.row.Store == writeStore {
			continue
		}
		files[r.Path] = f
	}
	type build struct {
		at     time.Time
		number int
		files  []snapshotFile
	}
	versions := map[string]map[string]*build{}
	for _, f := range files {
		builds := versions[f.version]
		if builds == nil {
			builds = map[string]*build{}
			versions[f.version] = builds
		}
		b := builds[f.build]
		if b == nil {
			b = &build{at: f.at, number: f.number}
			builds[f.build] = b
		}
		b.files = append(b.files, f)
	}
	cutoff := s.side().clock().Add(-age)
	var plan []cleanupItem
	for _, version := range slices.Sorted(mapKeys(versions)) {
		if err := ctx.Err(); err != nil {
			return c, err
		}
		var list []*build
		for _, b := range versions[version] {
			list = append(list, b)
		}
		slices.SortFunc(list, func(a, b *build) int {
			if d := b.at.Compare(a.at); d != 0 {
				return d
			}
			return b.number - a.number
		})
		for i, b := range list {
			if i < keep || !b.at.Before(cutoff) {
				continue
			}
			slices.SortFunc(b.files, func(x, y snapshotFile) int { return strings.Compare(x.row.Path, y.row.Path) })
			for _, f := range b.files {
				plan = append(plan, cleanupItem{Asset: cleanup.Asset{Repository: repository, Path: f.row.Path, Store: f.row.Store,
					BlobCreated: time.UnixMilli(f.row.CreatedMS).UTC()}})
			}
		}
	}
	c.Selected = len(plan)
	var counts CleanupCounts
	err = s.removePlan(ctx, repo, plan, &counts)
	c.Removed, c.Kept, c.Skipped = counts.Removed, counts.Kept, counts.Skipped
	return c, err
}

// mapKeys are the keys of a map, for slices.Sorted.
func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
