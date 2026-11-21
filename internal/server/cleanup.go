package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	adminrules "github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/cleanup"
	"github.com/iogentic/pipesmith-registry/internal/docker"
	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/rest"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// The run of a cleanup policy over one repository (task 12 of the phase 6
// plan, rulings CL1 to CL3, CL6, CL9 to CL12 of leg 6-cleanup). A preview
// is the dry run of the same function: the same selection and the same
// plan, nothing removed, no unit of work taken and nothing written. A run
// removes each planned asset in a unit of work of its own: the index row
// and the delete mark of its blob in the write store, then its catalog
// rows, in the order of eraseRepository.

// CleanupCounts are the counts of a cleanup run (ruling CL10): the assets
// the policy selected, those removed, those kept because a store of the old
// product holds them or a docker manifest that stays names them, and those
// skipped because their path changed since the catalog was read. Removed
// counts the layers a removed docker manifest left unnamed too (CL6).
type CleanupCounts struct{ Selected, Removed, Kept, Skipped int }

// CleanupResult is what RunCleanup gives: the counts and the preview of the
// planned removals.
type CleanupResult struct {
	Counts  CleanupCounts
	Preview cleanup.Preview
}

// cleanupItem is one asset of the plan of a run.
type cleanupItem struct {
	cleanup.Asset
	layer bool // a docker blob that no manifest that stays names (CL6)
}

// errCleanupNoState is the failure of a cleanup on a server without state.
var errCleanupNoState = failure("the cleanup needs the state of the server, which is off")

// CleanupPreview is the dry run of the policy over the repository (ruling
// CL11): the planned removals, at most cleanup.PreviewLimit of them, with
// their total. It takes no unit of work and writes nothing. A policy with a
// criterion this server does not know is the refusal of decision A9.
func (s *Server) CleanupPreview(ctx context.Context, policy model.CleanupPolicy, repository string) (cleanup.Preview, error) {
	res, err := s.RunCleanup(ctx, policy, repository, true)
	return res.Preview, err
}

// CleanupRun runs the policy over the repository and removes what its
// preview lists (ruling CL12).
func (s *Server) CleanupRun(ctx context.Context, policy model.CleanupPolicy, repository string) (CleanupCounts, error) {
	res, err := s.RunCleanup(ctx, policy, repository, false)
	return res.Counts, err
}

// RunCleanup selects the assets of the repository that the policy names and
// plans their removal; dry stops there. The refusals are those of package
// admin: the format of the policy, its criteria (A9) and the repository of
// a preview (ruling Q8).
func (s *Server) RunCleanup(ctx context.Context, policy model.CleanupPolicy, repository string, dry bool) (CleanupResult, error) {
	var res CleanupResult
	if s.sec == nil {
		return res, errCleanupNoState
	}
	snap := s.sec.Snapshot()
	if snap == nil {
		return res, errCleanupNoState
	}
	if err := adminrules.CheckFormatValue(policy.Format, nil); err != nil {
		return res, err
	}
	if runs, _ := adminrules.Runnable(policy.Criteria); !runs {
		if err := adminrules.CheckCriteria(policy.Criteria, nil); err != nil {
			return res, err
		}
		return res, failure("the policy cannot run")
	}
	if err := adminrules.CheckPreviewTarget(snap, policy.Format, repository); err != nil {
		return res, err
	}
	st, err := s.rp.sync()
	if err != nil {
		return res, failed("the repositories cannot be read", err)
	}
	repo, ok := st.repositories.Get(repository)
	if !ok {
		return res, failure("the repository of the policy does not exist")
	}
	plan, counts, err := s.planCleanup(ctx, st, repo, policy.Criteria)
	if err != nil {
		return res, err
	}
	res.Counts = counts
	assets := make([]cleanup.Asset, len(plan))
	for i, item := range plan {
		assets[i] = item.Asset
	}
	res.Preview = cleanup.PreviewList(assets)
	if dry {
		return res, nil
	}
	return res, s.removePlan(ctx, repo, plan, &res.Counts)
}

// planCleanup is the selection and the plan of a run: the selected assets
// that a run removes, in the order of the selection, then the docker layers
// of CL6; the counts of the selected assets and of those kept and skipped.
// A path whose blob in the index is not the blob the catalog read is
// skipped: it was written again or removed since the last scan.
func (s *Server) planCleanup(ctx context.Context, st *state, repo repos.Repo, criteria map[string]any) ([]cleanupItem, CleanupCounts, error) {
	var c CleanupCounts
	in, err := s.cleanupInput(repo.Name)
	if err != nil {
		return nil, c, err
	}
	selection := cleanup.Select(criteria, in)
	writeStore := repo.WriteSettings().WriteStore // the store the repository writes into: local-write or a created store (SR2)
	rows, err := s.rp.index.RepoRows(repo.Name)
	if err != nil {
		return nil, c, failed("the index cannot be read", err)
	}
	byPath := map[string][]index.Asset{}
	for _, r := range rows {
		byPath[r.Path] = append(byPath[r.Path], r)
	}
	var plan []cleanupItem
	c.Selected = len(selection.Assets)
	for _, a := range selection.Assets {
		if err := ctx.Err(); err != nil {
			return nil, c, err
		}
		held := byPath[a.Path]
		switch {
		case slices.ContainsFunc(held, func(r index.Asset) bool { return r.Store != writeStore }):
			c.Kept++ // CL1: a store of the old product holds the path
		case len(held) == 0 || held[0].CreatedMS != a.BlobCreated.UnixMilli():
			c.Skipped++
		default:
			plan = append(plan, cleanupItem{Asset: a})
		}
	}
	if repos.AccessOf(repo) == "docker" {
		var dockerKept int
		plan, dockerKept, err = s.planDockerLayers(ctx, st, repo, in, plan, rows, writeStore)
		if err != nil {
			return nil, c, err
		}
		c.Kept += dockerKept
	}
	return plan, c, nil
}

// cleanupInput reads the assets of a repository from the catalog file, with
// the download times that wait for their batch, the build time of the file
// and the clock. Without the file there is no asset.
func (s *Server) cleanupInput(repo string) (cleanup.Input, error) {
	side := s.side()
	in := cleanup.Input{Now: side.clock().UTC()}
	cat, err := side.catalog()
	if err != nil {
		return in, failed("the catalog cannot be read", err)
	}
	if cat == nil {
		return in, nil
	}
	if in.CatalogBuilt, err = cat.Built(); err != nil {
		return in, failed("the catalog cannot be read", err)
	}
	rows, err := cat.AssetRows(repo)
	if err != nil {
		return in, failed("the catalog cannot be read", err)
	}
	pending := side.downloads.Pending(repo)
	in.Assets = make([]cleanup.Asset, len(rows))
	for i, r := range rows {
		a := cleanup.Asset{Repository: repo, Path: r.Path, Store: r.Store, Group: r.Group, Name: r.Name, Version: r.Version,
			BlobCreated: r.Created, LastDownloaded: r.Downloaded, ImportedLastDownloaded: r.ImportedDownloaded}
		if at, ok := pending[r.Path]; ok && at.After(a.LastDownloaded) {
			a.LastDownloaded = at.UTC()
		}
		in.Assets[i] = a
	}
	return in, nil
}

// dockerManifest reports a manifest path of a docker repository,
// v2/<name>/manifests/<reference>.
func dockerManifest(path string) bool {
	rest, ok := strings.CutPrefix(path, "v2/")
	return ok && !strings.HasPrefix(path, dockerBlobPrefix) && strings.Contains(rest, "/manifests/")
}

// manifestReference is the reference of a manifest path of a docker
// repository, a digest or a tag.
func manifestReference(path string) (string, bool) {
	if !dockerManifest(path) {
		return "", false
	}
	_, ref, ok := strings.Cut(path, "/manifests/")
	return ref, ok && ref != ""
}

// dockerLayerGrace is the age below which a blob that no manifest names
// stays: a push writes its layers before the manifest that names them.
const dockerLayerGrace = 6 * time.Hour

// maxManifestBytes bounds the read of one manifest of CL6.
const maxManifestBytes = 4 << 20

// planDockerLayers applies CL6 to the plan of a docker repository: a blob
// the plan holds that a manifest that stays names is kept, and when the plan
// removes a manifest, every blob of the write store that no manifest that
// stays names joins the plan as a layer. A manifest that stays and cannot be
// read keeps every blob.
func (s *Server) planDockerLayers(ctx context.Context, st *state, repo repos.Repo, in cleanup.Input, plan []cleanupItem,
	rows []index.Asset, writeStore string) ([]cleanupItem, int, error) {
	planned := map[string]bool{}
	removesManifest := false
	for _, item := range plan {
		planned[item.Path] = true
		removesManifest = removesManifest || dockerManifest(item.Path)
	}
	named := map[string]bool{}
	sure := true
	seen := map[string]bool{}
	for _, r := range rows {
		if !dockerManifest(r.Path) || planned[r.Path] || seen[r.Path] {
			continue
		}
		seen[r.Path] = true
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		digests, err := s.manifestDigests(st, repo.Name, r.Path)
		if err != nil {
			sure = false
			continue
		}
		for _, d := range digests {
			named[d] = true
		}
	}
	kept := 0
	out := plan[:0:0]
	removesManifest = false
	for _, item := range plan {
		if digest, ok := strings.CutPrefix(item.Path, dockerBlobPrefix); ok && (!sure || named[digest]) {
			kept++
			continue
		}
		if ref, ok := manifestReference(item.Path); ok && (!sure || named[ref]) {
			kept++ // a manifest that a list or index that stays names
			continue
		}
		removesManifest = removesManifest || dockerManifest(item.Path)
		out = append(out, item)
	}
	if !removesManifest || !sure {
		return out, kept, nil
	}
	catalogRows := map[string]cleanup.Asset{}
	for _, a := range in.Assets {
		catalogRows[a.Path] = a
	}
	foreign := map[string]bool{}
	for _, r := range rows {
		if r.Store != writeStore {
			foreign[r.Path] = true
		}
	}
	added := map[string]bool{}
	for _, r := range rows {
		digest, ok := strings.CutPrefix(r.Path, dockerBlobPrefix)
		if !ok || named[digest] || planned[r.Path] || foreign[r.Path] || added[r.Path] {
			continue
		}
		added[r.Path] = true
		a, ok := catalogRows[r.Path]
		if !ok {
			a = cleanup.Asset{Repository: repo.Name, Path: r.Path, Store: r.Store, BlobCreated: time.UnixMilli(r.CreatedMS).UTC()}
		}
		if a.BlobCreated.After(in.Now.Add(-dockerLayerGrace)) {
			continue // a push may still write the manifest that names it
		}
		out = append(out, cleanupItem{Asset: a, layer: true})
	}
	return out, kept, nil
}

// manifestDigests are the digests a stored manifest names.
func (s *Server) manifestDigests(st *state, repo, path string) ([]string, error) {
	asset, f, _, err := s.rp.open(st, repo, path)
	if err != nil {
		return nil, err
	}
	if asset == nil {
		return nil, errors.New("the manifest is gone")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxManifestBytes {
		return nil, errors.New("the manifest is too large")
	}
	return docker.ReferencedDigests(data)
}

// removePlan removes the planned assets, each in a unit of work of its own
// (rulings CL2 and CL9), then rebuilds the maven metadata of the artifacts
// it touched (ruling KN18), also after a failed mark ended the run. Before
// the first docker blob it reads the manifests of the repository again,
// after the planned manifests went: a blob that a manifest written since
// the plan names stays (CL6).
func (s *Server) removePlan(ctx context.Context, repo repos.Repo, plan []cleanupItem, c *CleanupCounts) error {
	if len(plan) > 0 && s.writes.writer == nil {
		return failure("the cleanup sets delete marks in the write store, and writes are off")
	}
	docker := repos.AccessOf(repo) == "docker"
	if docker {
		// the manifests go first, so that the blobs are checked against the
		// manifests that stay
		plan = slices.Clone(plan)
		slices.SortStableFunc(plan, func(a, b cleanupItem) int {
			return boolOrder(strings.HasPrefix(a.Path, dockerBlobPrefix)) - boolOrder(strings.HasPrefix(b.Path, dockerBlobPrefix))
		})
	}
	touched := map[maven.Scope]bool{}
	var order []maven.Scope
	var named map[string]bool
	err := func() error {
		for i, item := range plan {
			blob := docker && strings.HasPrefix(item.Path, dockerBlobPrefix)
			if blob && named == nil {
				var sure bool
				var err error
				if named, sure, err = s.namedDigests(ctx, repo.Name, nil); err != nil {
					return err
				}
				if !sure {
					c.Kept += len(plan) - i
					return nil
				}
			}
			if digest, ok := strings.CutPrefix(item.Path, dockerBlobPrefix); ok && blob && named[digest] {
				c.Kept++
				continue
			}
			end, err := s.taskUnit(ctx)
			if errors.Is(err, errFrozen) {
				return &tasks.Failure{Text: "stopped: the server froze", Cause: errFrozen}
			}
			if err != nil {
				return err
			}
			outcome, err := s.removeAsset(ctx, item)
			end()
			if err != nil {
				return err
			}
			switch outcome {
			case removedAsset:
				c.Removed++
				if scope, ok := mavenArtifact(item.Path); ok && !touched[scope] {
					touched[scope] = true
					order = append(order, scope)
				}
			case keptAsset:
				c.Kept++
			default:
				c.Skipped++
			}
		}
		return nil
	}()
	if repo.Format != "maven2" || repo.Type != "hosted" || !repo.TakesWrites() || len(order) == 0 {
		return err
	}
	if err != nil && (ctx.Err() != nil || errors.Is(err, errFrozen)) {
		logAt(slogWarn, serverLogger, "the maven metadata of %d artifacts of %s is not rebuilt after the stopped cleanup; "+
			"a run of %s rebuilds it", len(order), repo.Name, tasks.TypeMavenMetadata)
		return err
	}
	if rebuildErr := s.rebuildTouched(ctx, repo, order, c.Removed); err == nil {
		err = rebuildErr
	}
	return err
}

// boolOrder is 1 for true and 0 for false.
func boolOrder(b bool) int {
	if b {
		return 1
	}
	return 0
}

// namedDigests are the digests the docker manifests of a repository name
// in the index now, except the paths in skip; sure is false when a manifest
// cannot be read.
func (s *Server) namedDigests(ctx context.Context, repo string, skip map[string]bool) (map[string]bool, bool, error) {
	st, err := s.rp.sync()
	if err != nil {
		return nil, false, failed("the repositories cannot be read", err)
	}
	rows, err := s.rp.index.RepoRows(repo)
	if err != nil {
		return nil, false, failed("the index cannot be read", err)
	}
	named := map[string]bool{}
	sure := true
	seen := map[string]bool{}
	for _, r := range rows {
		if !dockerManifest(r.Path) || skip[r.Path] || seen[r.Path] {
			continue
		}
		seen[r.Path] = true
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		digests, err := s.manifestDigests(st, repo, r.Path)
		if err != nil {
			sure = false
			continue
		}
		for _, d := range digests {
			named[d] = true
		}
	}
	return named, sure, nil
}

// The outcomes of the removal of one asset.
const (
	removedAsset = iota
	keptAsset
	skippedAsset
)

// removeAsset removes one planned asset under the lock of its path: its
// rows are read again, a row of a store of the old product keeps it (CL1),
// a path written again or gone since the plan is skipped; else the erase
// step and the catalog rows (CL2). A mark that fails stops the run.
func (s *Server) removeAsset(ctx context.Context, item cleanupItem) (int, error) {
	unlock, err := s.writes.locks.lockCtx(ctx, item.Repository, item.Path)
	if err != nil {
		return 0, err
	}
	defer unlock()
	rows, err := s.rp.index.Rows(item.Repository, item.Path)
	if err != nil {
		return 0, failed("the index cannot be read", err)
	}
	var row *index.Asset
	for i := range rows {
		if !s.writes.ownStore(rows[i].Store) { // a store of the old product holds the path (CL1); a created store is own (SR3)
			return keptAsset, nil
		}
		row = &rows[i]
	}
	if row == nil {
		s.dropCatalogRows(item.Repository, item.Path)
		return skippedAsset, nil
	}
	if !item.layer && row.CreatedMS != item.BlobCreated.UnixMilli() {
		return skippedAsset, nil
	}
	if _, err := s.eraseWriteRow(*row); err != nil {
		if errors.Is(err, index.ErrBusy) {
			return 0, failure("the index stayed busy, so the run stops")
		}
		return 0, failed("a delete mark failed, so the run stops", err)
	}
	s.dropCatalogRows(item.Repository, item.Path)
	return removedAsset, nil
}

// dropCatalogRows removes the catalog rows of a path: through the catalog
// hook, and in catalog.sqlite, which the selection reads. A failure is
// logged: a later run finds the path without an index row and drops them.
func (s *Server) dropCatalogRows(repo, path string) {
	if s.writes.catalog != nil {
		if err := s.writes.catalog.Delete(repo, path); err != nil {
			logAt(slogWarn, serverLogger, "the catalog row of %s/%s is not removed: %v", repo, path, err)
		}
	}
	cat, err := s.side().catalog()
	if err == nil && cat != nil {
		err = cat.Delete(repo, path)
	}
	if err != nil {
		logAt(slogWarn, serverLogger, "the catalog row of %s/%s is not removed: %v", repo, path, err)
	}
}

// mavenArtifact is the artifact of a maven file path, its checksum files
// included; false for a path of no artifact.
func mavenArtifact(path string) (maven.Scope, bool) {
	base, _ := maven.SplitChecksum(path)
	if c := maven.CoordinatesOf(base); c != nil {
		return maven.Scope{GroupID: c.GroupID, ArtifactID: c.ArtifactID}, true
	}
	return maven.Scope{}, false
}

// rebuildTouched writes the maven metadata of each touched artifact again,
// as the system subject, a unit of work for each write (ruling KN18).
func (s *Server) rebuildTouched(ctx context.Context, repo repos.Repo, scopes []maven.Scope, removed int) error {
	subject := taskSubject(rest.ViewPermission(repo.Format, repo.Name, "add"), rest.ViewPermission(repo.Format, repo.Name, "edit"),
		rest.ViewPermission(repo.Format, repo.Name, "delete"))
	for _, scope := range scopes {
		_, err := s.rebuildMaven(rebuildRun{ctx: ctx, subject: subject, address: taskAddress, unit: s.taskUnit, after: rebuildAfter},
			repo.Name, scope)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, errFrozen):
			return &tasks.Failure{Text: "stopped: the server froze", Cause: errFrozen}
		default:
			return failed(fmt.Sprintf("the maven metadata rebuild after %d removed failed", removed), err)
		}
	}
	return nil
}
