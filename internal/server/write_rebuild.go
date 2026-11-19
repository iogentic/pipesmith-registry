package server

import (
	"bytes"
	"errors"
	"fmt"

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
	files, err := s.filesBelow(repo.Name, dir)
	if err != nil {
		return nil, err
	}
	open := func(path string) (maven.Content, int64, error) {
		asset, f, size, err := s.rp.open(st, repo.Name, path)
		if err != nil || asset == nil {
			return nil, 0, err
		}
		return f, size, nil
	}
	rebuilt, err := maven.Rebuild(maven.Source{Files: files, Open: open}, scope)
	if err != nil {
		return nil, err
	}
	report := &RebuildReport{}
	for _, d := range rebuilt.Documents {
		same, err := s.holds(st, repo.Name, d)
		if err != nil {
			return nil, err
		}
		if same {
			report.Unchanged = append(report.Unchanged, d.Path)
			continue
		}
		_, err = s.ingest(ingestRequest{repo: repo, path: d.Path, body: bytes.NewReader(d.Data), size: int64(len(d.Data)),
			exempt: true, subject: subject, address: address})
		if err != nil {
			return nil, fmt.Errorf("%s of repository %s: %w", d.Path, repo.Name, err)
		}
		report.Written = append(report.Written, d.Path)
	}
	for _, path := range rebuilt.Stale {
		removed, err := s.erase(repo, path)
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
func (s *Server) filesBelow(repo, dir string) ([]maven.File, error) {
	var files []maven.File
	pending := []string{dir}
	for len(pending) > 0 {
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
