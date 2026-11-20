package tasks

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Host is what the built kinds call: the server they run in. Each method
// honours its context, runs as the subject system from the address
// internal (ruling KN5), and returns an error whose text names no path and
// no property value (ruling KN9).
type Host interface {
	// RebuildMaven writes the maven metadata of a hosted maven2 repository
	// again within scope.
	RebuildMaven(ctx context.Context, repository string, scope MavenScope) (MavenCounts, error)
	// Rescan scans a store again through the scan job of POST /api/scan
	// (ruling KN4).
	Rescan(ctx context.Context, store string) (ScanCounts, error)
	// PurgeUploads ends the Docker upload sessions of a repository, * for
	// all, without a request for longer than age.
	PurgeUploads(ctx context.Context, repository string, age time.Duration) (PurgeCounts, error)
	// BackupState copies state.sqlite into a new file in the directory
	// location (ruling KN6).
	BackupState(ctx context.Context, location string) (BackupFile, error)
	// RemoveWorkFiles removes the work files of a store of this server
	// older than age that no running upload holds (ruling KN7).
	RemoveWorkFiles(ctx context.Context, store string, age time.Duration) (WorkFileCounts, error)
}

// MavenScope is the part of a repository a maven metadata rebuild takes:
// all of it, a group, an artifact or a version.
type MavenScope struct{ GroupID, ArtifactID, BaseVersion string }

// MavenCounts are the documents a rebuild wrote, found unchanged, removed
// as stale, and kept although stale because a read only store holds them.
type MavenCounts struct{ Written, Unchanged, Removed, Kept int }

// ScanCounts are the counts of a scan of a store.
type ScanCounts struct{ Live, Deleted, Copies, NoBytes, Unparsable int }

// PurgeCounts are the upload sessions a purge ended and kept.
type PurgeCounts struct{ Ended, Kept int }

// BackupFile is the base name and the size of a backup.
type BackupFile struct {
	Name  string
	Bytes int64
}

// WorkFileCounts are the work files a removal removed and kept.
type WorkFileCounts struct{ Removed, Kept int }

// Failure is an error of a host whose text a run message shows: a fixed
// text with counts, no path, no record name and no text of the system
// (ruling KN13). A run shows every other error of a host as a fixed text.
// Cause is the error it stands for, for errors.Is, nil for none; no
// message shows it.
type Failure struct {
	Text  string
	Cause error
}

func (f *Failure) Error() string { return f.Text }

// Unwrap is the cause.
func (f *Failure) Unwrap() error { return f.Cause }

// ErrNoHost is the failure of a run before the server is set.
var ErrNoHost = errors.New("the server is not started yet, so the task does not run")

// HostRef is the late bound host of the kinds: the registry loads before
// the server exists, and the server sets itself once it is built. Its
// methods are safe for use by several goroutines.
type HostRef struct {
	mu   sync.Mutex
	host Host
}

// Set sets the host; nil takes it away again.
func (r *HostRef) Set(h Host) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.host = h
}

// Host is the host, ErrNoHost while none is set.
func (r *HostRef) Host() (Host, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.host == nil {
		return nil, ErrNoHost
	}
	return r.host, nil
}
