package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	adminrules "github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/blobwrite"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/rest"
	statefile "github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// The host of the built task kinds (leg 6-kinds): startTasks sets the
// server on the registry before the scheduler starts, and Close stops the
// runs and waits for them before it closes the stores, then takes the host
// away (ruling KN10). A run acts as the
// subject system from the address internal with the rights of its kind
// only (ruling KN5). A kind that writes takes the read only mode for each
// unit of work and stops when the server freezes (ruling KN8). No error
// names a path or a property value (ruling KN9).

var _ tasks.Host = (*Server)(nil)

// taskAddress is the address of every run of a task (ruling KN5).
const taskAddress = "internal"

// freezeLook is how often a rescan looks for a freeze that starts.
const freezeLook = 20 * time.Millisecond

// errStoppedByFreeze ends a rescan that a starting freeze stopped.
var errStoppedByFreeze = failure("stopped: the server froze")

// errFrozen ends a run that meets a freeze.
var errFrozen = failure("the server is frozen, so the run stops")

// failure is a fixed text a run message shows (ruling KN13).
func failure(text string) error { return &tasks.Failure{Text: text} }

// failed logs the cause in the log of the server and gives the fixed text.
func failed(text string, cause error) error {
	logAt(slogWarn, serverLogger, "a task run failed: %s: %v", text, cause)
	return failure(text)
}

// taskSubject is the subject of a run, holding the permissions given.
func taskSubject(permissions ...string) *model.Subject {
	return &model.Subject{User: model.User{ID: "system", Source: "system", Status: "active"}, Realm: "system", Permissions: permissions}
}

// beforeTaskUnit is called before each unit of work of a run; tests set it.
var beforeTaskUnit func()

// taskUnit begins one unit of work of a run that writes: it holds the read
// only mode until end is called. It fails when ctx ends or the server is
// frozen.
func (s *Server) taskUnit(ctx context.Context) (end func(), err error) {
	if beforeTaskUnit != nil {
		beforeTaskUnit()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.readOnly == nil {
		return func() {}, nil
	}
	end, _, ok := s.readOnly.Begin(ctx)
	if !ok {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errFrozen
	}
	return end, nil
}

// RebuildMaven is tasks.Host.RebuildMaven: the one rebuild of
// RebuildMavenMetadata (ruling KN18) with the context, a unit of work for
// each write and removal, and the subject system.
func (s *Server) RebuildMaven(ctx context.Context, repository string, scope tasks.MavenScope) (tasks.MavenCounts, error) {
	var c tasks.MavenCounts
	if s.writes.writer == nil {
		return c, failure("the rebuild writes into the write store, and writes are off")
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
	mavenScope := maven.Scope{GroupID: scope.GroupID, ArtifactID: scope.ArtifactID, BaseVersion: scope.BaseVersion}
	if _, err := mavenScope.Dir(); err != nil {
		return c, failure("the scope of the task is no Maven path")
	}
	subject := taskSubject(rest.ViewPermission(repo.Format, repo.Name, "add"), rest.ViewPermission(repo.Format, repo.Name, "edit"),
		rest.ViewPermission(repo.Format, repo.Name, "delete"))
	report, err := s.rebuildMaven(rebuildRun{ctx: ctx, subject: subject, address: taskAddress, unit: s.taskUnit, after: rebuildAfter},
		repo.Name, mavenScope)
	if report != nil {
		c = tasks.MavenCounts{Written: len(report.Written), Unchanged: len(report.Unchanged), Removed: len(report.Removed), Kept: len(report.Kept)}
	}
	switch {
	case err == nil, ctx.Err() != nil:
		return c, err
	case errors.Is(err, errFrozen):
		return c, &tasks.Failure{Text: fmt.Sprintf("the server is frozen, so the run stops after %d written, %d unchanged, %d removed",
			c.Written, c.Unchanged, c.Removed), Cause: errFrozen}
	}
	return c, failed(fmt.Sprintf("a write of the rebuild failed after %d written, %d unchanged, %d removed", c.Written, c.Unchanged, c.Removed), err)
}

// defaultTasksClose bounds the wait of Close for the runs when the options
// give no bound.
const defaultTasksClose = 10 * time.Second

// closeTasks stops the runs of the registry and waits for their ends,
// within the bound of the options, before the server closes its stores and
// its index (ruling KN10); the host goes away after the runs ended.
func (s *Server) closeTasks() {
	// leg 6-cleanup: the download times and the catalog of the cleanup
	// close once the runs ended
	defer s.closeSide()
	if s.tasks == nil {
		return
	}
	bound := s.tasksClose
	if bound <= 0 {
		bound = defaultTasksClose
	}
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	if err := s.tasks.Close(ctx); err != nil {
		logAt(slogWarn, serverLogger, "the task runs did not end within %s; the server closes its stores anyway", bound)
	}
	s.tasks.SetHost(nil)
}

// rebuildAfter is called after each write and removal of a rebuild task;
// tests set it.
var rebuildAfter func(path string)

// Rescan is tasks.Host.Rescan: a scan of the scan job of POST /api/scan,
// one at a time (ruling KN4). It holds the read only mode until the scan
// goroutine has ended; a freeze that starts stops the walk, so that the
// freeze passes within a second, and a stop answers at once while the unit
// stays held until the goroutine is done (ruling KN11).
func (s *Server) Rescan(ctx context.Context, name string) (tasks.ScanCounts, error) {
	var c tasks.ScanCounts
	st, err := s.rp.sync()
	if err != nil {
		return c, failed("the stores cannot be read", err)
	}
	store := st.stores[name]
	if store == nil {
		return c, failure("the store of the task is not attached")
	}
	end, err := s.taskUnit(ctx)
	if err != nil {
		return c, err
	}
	var stop atomic.Bool
	done, started := s.job.startStoppable(store, &stop)
	if !started {
		end()
		return c, failure("a scan runs already, so no second scan starts")
	}
	froze := make(chan struct{})
	go func() {
		tick := time.NewTicker(freezeLook)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				stop.Store(true)
				return
			case <-tick.C:
				if s.readOnly != nil && s.readOnly.Freezing() {
					stop.Store(true)
					close(froze)
					return
				}
			}
		}
	}()
	select {
	case <-done:
		end()
	case <-ctx.Done():
		go func() {
			<-done
			end()
		}()
		return c, ctx.Err()
	case <-froze:
		<-done
		end()
		return c, errStoppedByFreeze
	}
	select {
	case <-froze:
		return c, errStoppedByFreeze
	default:
	}
	s.job.mu.Lock()
	value, _ := s.job.last.Get(name)
	s.job.mu.Unlock()
	outcome, _ := value.(*pyjson.Object)
	if outcome == nil {
		return c, failure("the scan left no outcome")
	}
	if scanErr, _ := outcome.Get("error"); scanErr != nil {
		if scanErr == "cancelled" {
			return c, failure("the scan was cancelled")
		}
		return c, failure("the scan of the store failed; the log of the server names the cause")
	}
	count := func(key string) int {
		v, _ := outcome.Get(key)
		n, _ := v.(int)
		return n
	}
	return tasks.ScanCounts{Live: count("live"), Deleted: count("deleted"), Copies: count("copies"), NoBytes: count("no_bytes"),
		Unparsable: count("unparsable")}, nil
}

// PurgeUploads is tasks.Host.PurgeUploads: it ends each Docker upload
// session of the repository, * for all, that no request holds and that had
// none for age, with its work file, one session per unit of work.
func (s *Server) PurgeUploads(ctx context.Context, repository string, age time.Duration) (tasks.PurgeCounts, error) {
	var c tasks.PurgeCounts
	o := &s.oci
	o.mu.Lock()
	var ids []string
	for id, sess := range o.sessions {
		if repository == "*" || sess.repo == repository {
			ids = append(ids, id)
		}
	}
	o.mu.Unlock()
	slices.Sort(ids)
	for _, id := range ids {
		end, err := s.taskUnit(ctx)
		if err != nil {
			return c, err
		}
		o.mu.Lock()
		sess := o.sessions[id]
		switch {
		case sess == nil:
		case sess.inUse == 0 && o.clock().Sub(sess.lastUsed) >= age:
			o.endLocked(id, sess)
			c.Ended++
		default:
			c.Kept++
		}
		o.mu.Unlock()
		end()
	}
	return c, nil
}

// BackupState is tasks.Host.BackupState (rulings KN6 and KN12): the place
// is checked against the data directory and the root of every store the
// server lists. It writes nothing of the data directory, so it takes no
// unit of work.
func (s *Server) BackupState(ctx context.Context, location string) (tasks.BackupFile, error) {
	guarded, err := s.storeRoots()
	if err != nil {
		return tasks.BackupFile{}, failed("the stores cannot be read", err)
	}
	name, size, err := statefile.Backup(ctx, s.rp.cfg.DataDir, location, guarded, time.Now())
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return tasks.BackupFile{}, ctxErr
		}
		return tasks.BackupFile{}, failure(err.Error()) // the texts of state.Backup are fixed
	}
	return tasks.BackupFile{Name: name, Bytes: size}, nil
}

// storeRoots are the roots of the stores the server lists: the attached
// stores, those of config.json among them, the write store, and the blob
// stores of the state file.
func (s *Server) storeRoots() ([]string, error) {
	st, err := s.rp.sync()
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, store := range st.stores {
		roots = append(roots, store.Root())
	}
	for _, sc := range st.configs {
		roots = append(roots, sc.Root)
	}
	roots = append(roots, writeStoreRoot(s.rp.cfg.DataDir))
	if s.sec != nil {
		if snap := s.sec.Snapshot(); snap != nil {
			for _, b := range snap.BlobStores {
				roots = append(roots, b.Path)
			}
		}
	}
	return roots, nil
}

// RemoveWorkFiles is tasks.Host.RemoveWorkFiles (ruling KN7): the work
// files of content/tmp of a store of this server, the write store or a
// store below the stores folder of the data directory that config.json
// does not attach, last written more than age ago and held by no Docker
// upload session, one file per unit of work.
func (s *Server) RemoveWorkFiles(ctx context.Context, store string, age time.Duration) (tasks.WorkFileCounts, error) {
	var c tasks.WorkFileCounts
	root, err := s.ownStoreRoot(store)
	if err != nil {
		return c, err
	}
	cutoff := time.Now().Add(-age)
	old, young, err := blobwrite.OldWorkFiles(ctx, root, cutoff)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return c, ctxErr
		}
		return c, failure("the work folder of the store cannot be read")
	}
	c.Kept = young
	for _, name := range old {
		end, err := s.taskUnit(ctx)
		if err != nil {
			return c, err
		}
		removed := false
		if !s.heldByUpload(root, name) {
			removed, err = blobwrite.RemoveOldWorkFile(root, name, cutoff)
		}
		end()
		switch {
		case err != nil:
			return c, failure("a work file of the store cannot be removed")
		case removed:
			c.Removed++
		default:
			c.Kept++
		}
	}
	return c, nil
}

// heldByUpload reports a work file of the store at root that an open Docker
// upload session or a running upload of the writer holds (ruling KN15).
func (s *Server) heldByUpload(root, name string) bool {
	path := filepath.Join(root, "content", blobwrite.TmpDir, name)
	for _, o := range s.writes.writers() {
		if w := o.writer; w.Holds(name) && sameDir(filepath.Join(w.Root(), "content", blobwrite.TmpDir, name), path) {
			return true
		}
	}
	o := &s.oci
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, sess := range o.sessions {
		if sess.work != "" && filepath.Base(sess.work) == name && sameDir(sess.work, path) {
			return true
		}
	}
	return false
}

// sameDir reports two paths in one directory, links resolved.
func sameDir(a, b string) bool {
	da, errA := filepath.EvalSymlinks(filepath.Dir(a))
	db, errB := filepath.EvalSymlinks(filepath.Dir(b))
	return errA == nil && errB == nil && da == db
}

// ownStoreRoot is the root of a store of this server: the write store, or
// a store whose root lies below the stores folder of the data directory
// and that config.json does not attach. A store of the old product is
// refused (A7).
func (s *Server) ownStoreRoot(name string) (string, error) {
	notOwn := failure("the store of the task is not a store of this server")
	if s.writes.writer != nil && s.writes.writer.Name() == name {
		return s.writes.writer.Root(), nil
	}
	st, err := s.rp.sync()
	if err != nil {
		return "", failed("the stores cannot be read", err)
	}
	if slices.ContainsFunc(st.configs, func(sc config.StoreConfig) bool { return sc.Name == name }) {
		return "", notOwn
	}
	root, found, err := s.blobStoreRoot(name)
	if err != nil {
		return "", failed("the stores cannot be read", err)
	}
	if !found || root == "" {
		return "", failure("the store of the task does not exist")
	}
	base, errBase := filepath.EvalSymlinks(filepath.Join(s.rp.cfg.DataDir, adminrules.StoresDir))
	real, errRoot := filepath.EvalSymlinks(root)
	if errBase != nil || errRoot != nil {
		return "", notOwn
	}
	rel, err := filepath.Rel(base, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", notOwn
	}
	return real, nil
}
