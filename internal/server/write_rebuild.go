package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The metadata rebuild of a hosted maven2 repository (task 10 and W15 of the
// phase 4 plan): internal/maven computes the documents from the files the
// index holds, and only the documents whose bytes differ from the stored
// ones are written, through ingest, so a second run writes no blob. The
// command maven rebuild-metadata calls it, and so does the REST delete of a
// Maven component of leg 4d.

// RebuildReport lists the paths a metadata rebuild wrote, found unchanged,
// removed as stale, and kept although stale because a read only store holds
// them.
type RebuildReport struct {
	Written, Unchanged, Removed, Kept []string
}

// RebuildMavenMetadata writes maven-metadata.xml and its checksum files of
// the hosted maven2 repository repoName within scope again from the files
// it holds, as subject from address. It needs writes on. A document that
// no file below its directory supports any more is removed through erase.
func (s *Server) RebuildMavenMetadata(repoName string, scope maven.Scope, subject *model.Subject, address string) (*RebuildReport, error) {
	return s.rebuildMaven(rebuildRun{ctx: context.Background(), subject: subject, address: address}, repoName, scope)
}

// rebuildRun is how one metadata rebuild runs: its context, the subject and
// address of its writes, the unit of work each write and each removal
// takes (nil for none), and a hook after each of them (nil for none).
type rebuildRun struct {
	ctx     context.Context
	subject *model.Subject
	address string
	unit    func(ctx context.Context) (end func(), err error)
	after   func(path string)
}

// rebuildMaven is the one metadata rebuild of a hosted maven2 repository
// (ruling KN18): RebuildMavenMetadata and the task kind call it. It stops
// at the next file, document or removal once ctx ends.
func (s *Server) rebuildMaven(run rebuildRun, repoName string, scope maven.Scope) (*RebuildReport, error) {
	ctx := run.ctx
	if s.writes.writer == nil {
		return nil, errors.New("the rebuild writes blobs, and writes are off")
	}
	st, err := s.rp.sync()
	if err != nil {
		return nil, err
	}
	repo, ok := st.repositories.Get(repoName)
	if !ok {
		return nil, fmt.Errorf("no repository %s", repoName)
	}
	if repo.Format != "maven2" || repo.Type != "hosted" {
		return nil, fmt.Errorf("repository %s is a %s %s repository; the rebuild takes a hosted maven2 repository", repo.Name, repo.Format, repo.Type)
	}
	if !repo.TakesWrites() {
		return nil, fmt.Errorf("repository %s takes no write: its stored record says %s and online %t; the rebuild takes a hosted maven2 repository that is online",
			repo.Name, repo.Record.Type, repo.Record.Online)
	}
	dir, err := scope.Dir()
	if err != nil {
		return nil, err
	}
	leave := s.writes.metadata.enter(repo.Name, dir, true)
	defer leave()
	files, err := s.filesBelow(ctx, repo.Name, dir)
	if err != nil {
		return nil, err
	}
	open := func(path string) (maven.Content, int64, error) {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		asset, f, size, err := s.rp.open(st, repo.Name, path)
		if err != nil || asset == nil {
			return nil, 0, err
		}
		return f, size, nil
	}
	rebuilt, err := maven.Rebuild(maven.Source{Files: files, Open: open}, scope)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	report := &RebuildReport{}
	for _, d := range rebuilt.Documents {
		end, err := run.begin()
		if err != nil {
			return report, err
		}
		same, err := s.holds(st, repo.Name, d)
		if err == nil && !same {
			_, err = s.ingest(ingestRequest{repo: repo, path: d.Path, body: bytes.NewReader(d.Data), size: int64(len(d.Data)),
				exempt: true, subject: run.subject, address: run.address})
			if err != nil {
				err = fmt.Errorf("%s of repository %s: %w", d.Path, repo.Name, err)
			}
		}
		end()
		switch {
		case err != nil:
			return report, err
		case same:
			report.Unchanged = append(report.Unchanged, d.Path)
		default:
			report.Written = append(report.Written, d.Path)
		}
		run.done(d.Path)
	}
	for _, path := range rebuilt.Stale {
		end, err := run.begin()
		if err != nil {
			return report, err
		}
		removed, err := s.erase(repo, path)
		end()
		run.done(path)
		var refusal *writeRefusal
		switch {
		case errors.As(err, &refusal) && refusal.rule == ruleReadOnlyStore:
			report.Kept = append(report.Kept, path)
		case err != nil:
			return nil, fmt.Errorf("%s of repository %s: %w", path, repo.Name, err)
		case removed != nil:
			report.Removed = append(report.Removed, path)
		}
	}
	return report, nil
}

// begin starts the unit of one write or removal.
func (run rebuildRun) begin() (func(), error) {
	if err := run.ctx.Err(); err != nil {
		return nil, err
	}
	if run.unit == nil {
		return func() {}, nil
	}
	return run.unit(run.ctx)
}

// done calls the hook after a write or a removal.
func (run rebuildRun) done(path string) {
	if run.after != nil {
		run.after(path)
	}
}

// holds reports whether the repository serves exactly the bytes of d at its
// path now.
func (s *Server) holds(st *state, repo string, d maven.Document) (bool, error) {
	asset, f, size, err := s.rp.open(st, repo, d.Path)
	if err != nil || asset == nil {
		return false, err
	}
	if size != int64(len(d.Data)) {
		_ = f.Close() // read only, nothing was read
		return false, nil
	}
	data, err := readUpTo(f, size)
	if err != nil {
		return false, err
	}
	return bytes.Equal(data, d.Data), nil
}

// filesBelow are the paths of a repository below a directory, "" for all,
// with the creation time of their winning rows.
func (s *Server) filesBelow(ctx context.Context, repo, dir string) ([]maven.File, error) {
	var files []maven.File
	pending := []string{dir}
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		children, err := s.rp.allChildren(repo, current)
		if err != nil {
			return nil, err
		}
		for _, c := range children {
			path := c.Name
			if current != "" {
				path = current + "/" + c.Name
			}
			switch {
			case c.IsDir:
				pending = append(pending, path)
			case c.CreatedMS != nil:
				files = append(files, maven.File{Path: path, CreatedMS: *c.CreatedMS})
			}
		}
	}
	return files, nil
}

// metadataGate keeps a metadata rebuild apart from a client write of a
// metadata path within its scope (note N5 of the 0.2.0 plan): the rebuild
// reads the files and then writes the documents, so a maven-metadata.xml
// that a deploy sends in between would be overwritten by documents that do
// not know its version. A rebuild waits for the metadata writes and the
// rebuilds that run within its scope; a metadata write waits for the
// rebuilds whose scope holds its path. Writes of metadata do not wait for
// each other; the lock of a path keeps two writes of one path apart.
type metadataGate struct {
	mu       sync.Mutex
	changed  *sync.Cond
	rebuilds map[string][]string // repository: the directories of the rebuilds that run
	writes   map[string][]string // repository: the paths of the metadata writes that run
}

// within reports a path at or below dir; every path is within "".
func within(path, dir string) bool {
	return dir == "" || path == dir || strings.HasPrefix(path, dir+"/")
}

func (g *metadataGate) blocked(repo, at string, rebuild bool) bool {
	for _, dir := range g.rebuilds[repo] {
		if within(at, dir) || rebuild && within(dir, at) {
			return true
		}
	}
	if rebuild {
		for _, path := range g.writes[repo] {
			if within(path, at) {
				return true
			}
		}
	}
	return false
}

// enter waits until a rebuild of the directory at, or a metadata write of
// the path at, may run, and holds its place until leave.
func (g *metadataGate) enter(repo, at string, rebuild bool) (leave func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.changed == nil {
		g.changed = sync.NewCond(&g.mu)
		g.rebuilds, g.writes = map[string][]string{}, map[string][]string{}
	}
	for g.blocked(repo, at, rebuild) {
		g.changed.Wait()
	}
	held := g.writes
	if rebuild {
		held = g.rebuilds
	}
	held[repo] = append(held[repo], at)
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		list := held[repo]
		i := slices.Index(list, at)
		held[repo] = slices.Delete(list, i, i+1)
		if len(held[repo]) == 0 {
			delete(held, repo)
		}
		g.changed.Broadcast()
	}
}
