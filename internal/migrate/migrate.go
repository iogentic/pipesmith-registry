// Package migrate runs the numbered schema steps of a SQLite file of the
// data directory, as state.sqlite and catalog.sqlite have them.
//
// The version of a file is the row schema of its table meta, the table
// index.sqlite has as well; a file without any table has version 0. Steps
// run forward only, each in one transaction that also sets the schema row.
// A file of a newer version than the last step is refused, and the refusal
// names both versions. On request Run first writes the copy <file>.v<old>
// with VACUUM INTO: the way back to a binary of the old version.
//
// The connection should begin its transactions with BEGIN IMMEDIATE (the
// parameter _txlock=immediate of the driver). Each step reads the version
// again inside its transaction, so two processes that open one file at the
// same time apply each step once.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Meta is the table of the version, the one index.sqlite has. Run creates it
// in the transaction of step 1.
const Meta = "CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL) WITHOUT ROWID"

// Step takes a file from version Version-1 to Version. Apply runs inside the
// transaction of the step, and Run sets the schema row in the same one.
type Step struct {
	Version int
	Apply   func(tx *sql.Tx) error
}

// Options say what Run may do beyond the steps.
type Options struct {
	// Kind names the file in the refusal of a file without the meta table,
	// as "state file".
	Kind string
	// Create lets a file without any table take every step from 1, after
	// it is put into WAL mode. Without Create such a file is refused.
	Create bool
	// Backup writes the copy <file>.v<old>, mode 0600, before the first step
	// of a file that has a version. An existing copy is never replaced.
	Backup bool
}

// Result is what Run found and did.
type Result struct {
	From int    // the version Run found
	To   int    // the version of the file when Run returned
	Copy string // the copy written before the first step, empty when none
}

// NewerError is the refusal of a file of a newer schema than the last step.
type NewerError struct {
	File  string
	Found int // the version of the file
	Knows int // the version of the last step
}

func (e *NewerError) Error() string {
	return fmt.Sprintf("%s has schema %d, this binary knows schema %d", e.File, e.Found, e.Knows)
}

var bg = context.Background()

// queryer is a connection pool or a transaction.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Run brings the file db has open from its version to the version of the
// last step. file is the path db has open: Run names it in every refusal,
// and the copy lies beside it.
func Run(db *sql.DB, file string, steps []Step, opts Options) (Result, error) {
	if err := check(steps); err != nil {
		return Result{}, err
	}
	last := len(steps)
	from, err := version(db, file, opts.Kind)
	if err != nil {
		return Result{}, err
	}
	res := Result{From: from, To: from}
	switch {
	case from > last:
		return res, &NewerError{File: file, Found: from, Knows: last}
	case from == last:
		return res, nil
	case from == 0 && !opts.Create:
		return res, noMeta(file, opts.Kind)
	case from == 0:
		if err := walMode(db, file); err != nil {
			return res, err
		}
	case opts.Backup:
		if res.Copy, err = writeCopy(db, file, from); err != nil {
			return res, err
		}
	}
	for _, step := range steps[from:] {
		now, err := apply(db, file, opts.Kind, step, last)
		if err != nil {
			if res.To == from && res.Copy != "" {
				_ = os.Remove(res.Copy) // the file is as it was, so the copy is no way back; the step error counts
				res.Copy = ""
			}
			return res, err
		}
		res.To = now
	}
	return res, nil
}

// walTries and walPause bound the retries of walMode, about one second.
const (
	walTries = 100
	walPause = 10 * time.Millisecond
)

// walMode puts a new file into WAL mode. When two processes switch one file
// at the same moment, SQLite answers one of them busy at once, without its
// busy timeout; that one tries again and finds the mode the other set.
func walMode(db *sql.DB, file string) error {
	var err error
	for range walTries {
		if _, err = db.ExecContext(bg, "PRAGMA journal_mode = WAL"); err == nil {
			return nil
		}
		time.Sleep(walPause)
	}
	return fmt.Errorf("%s: WAL mode: %w", file, err)
}

// check refuses a step list that is not numbered 1, 2, 3 and so on.
func check(steps []Step) error {
	if len(steps) == 0 {
		return errors.New("migrate: no schema step")
	}
	for i, step := range steps {
		if step.Version != i+1 {
			return fmt.Errorf("migrate: step %d of the list has version %d; the steps count from 1 without a gap", i+1, step.Version)
		}
		if step.Apply == nil {
			return fmt.Errorf("migrate: step %d has no Apply", step.Version)
		}
	}
	return nil
}

func noMeta(file, kind string) error {
	return fmt.Errorf("%s has no meta table, so it is no %s", file, kind)
}

// version reads the schema row of the file. A file without any table has
// version 0; a file with tables and no meta table is refused.
func version(q queryer, file, kind string) (int, error) {
	var tables, meta int
	if err := q.QueryRowContext(bg, "SELECT count(*) FROM sqlite_master WHERE type = 'table'").Scan(&tables); err != nil {
		return 0, fmt.Errorf("%s is not a SQLite file: %w", file, err)
	}
	if err := q.QueryRowContext(bg, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'meta'").
		Scan(&meta); err != nil {
		return 0, fmt.Errorf("%s: tables: %w", file, err)
	}
	if meta == 0 {
		if tables > 0 {
			return 0, noMeta(file, kind)
		}
		return 0, nil
	}
	var v int
	if err := q.QueryRowContext(bg, "SELECT value FROM meta WHERE key = 'schema'").Scan(&v); err != nil {
		return 0, fmt.Errorf("%s has no schema row: %w", file, err)
	}
	if v < 1 {
		return 0, fmt.Errorf("%s has schema %d; schemas count from 1", file, v)
	}
	return v, nil
}

// apply runs one step in one transaction and returns the version after it.
// A step that another process applied meanwhile is not run again.
func apply(db *sql.DB, file, kind string, step Step, last int) (int, error) {
	tx, err := db.BeginTx(bg, nil)
	if err != nil {
		return 0, fmt.Errorf("%s: begin of schema step %d: %w", file, step.Version, err)
	}
	defer func() { _ = tx.Rollback() }() // after Commit there is nothing to roll back
	now, err := version(tx, file, kind)
	switch {
	case err != nil:
		return 0, fmt.Errorf("schema step %d: %w", step.Version, err)
	case now > last:
		return now, &NewerError{File: file, Found: now, Knows: last}
	case now >= step.Version:
		return now, nil
	case now < step.Version-1:
		return now, fmt.Errorf("%s went back to schema %d during its migration to %d", file, now, step.Version)
	}
	if step.Version == 1 {
		if _, err := tx.ExecContext(bg, Meta); err != nil {
			return now, fmt.Errorf("%s: meta table: %w", file, err)
		}
	}
	if err := step.Apply(tx); err != nil {
		return now, fmt.Errorf("%s: schema step %d: %w", file, step.Version, err)
	}
	if _, err := tx.ExecContext(bg, "INSERT INTO meta VALUES ('schema', ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value",
		step.Version); err != nil {
		return now, fmt.Errorf("%s: schema row of step %d: %w", file, step.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return now, fmt.Errorf("%s: commit of schema step %d: %w", file, step.Version, err)
	}
	return step.Version, nil
}

// writeCopy writes <file>.v<old> with VACUUM INTO into a new file of mode
// 0600 and syncs it. It refuses a copy that exists and leaves it as it is.
func writeCopy(db *sql.DB, file string, old int) (string, error) {
	target, err := filepath.Abs(file + ".v" + strconv.Itoa(old))
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the copy beside the file the caller opened.
	if errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("the copy %s exists, from an earlier migration of %s or from another process that migrates it now; "+
			"move the copy away while no other process uses the file, then start again", target, file)
	}
	if err != nil {
		return "", err
	}
	err = f.Close()
	if err == nil {
		// VACUUM INTO writes into a file that exists only when it is empty
		_, err = db.ExecContext(bg, "VACUUM INTO ?", target)
	}
	if err == nil {
		err = syncFile(target)
	}
	if err != nil {
		_ = os.Remove(target) // a part copy is no way back; the copy error counts
		return "", fmt.Errorf("copy of %s to %s: %w", file, target, err)
	}
	return target, nil
}

// syncFile puts the copy on the disk before the first step changes the file.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- the copy writeCopy made.
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close() // the sync error counts
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path)) // #nosec G304 -- the folder of that copy.
	if err != nil {
		return err
	}
	_ = dir.Sync() // some file systems refuse to sync a folder; the copy itself is synced
	return dir.Close()
}
