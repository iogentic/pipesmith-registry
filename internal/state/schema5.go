package state

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// schema5 is step 5 of state.sqlite (decision E3), the tables of phase 6:
//
//   - task_runs, one row per run of a task: its start in UTC milliseconds,
//     and its end and result, NULL while it runs;
//   - cleanup_policies, tasks and capabilities, each row with its origin,
//     import for a row of the import and local for one an administrator
//     made (ruling S2). Criteria, schedules, properties and the imported
//     state are JSON (ruling S3); imported_state is NULL for a local row.
//
// Once listed the step never changes; a further table is step 5.
func schema5(tx *sql.Tx) error {
	const origin = " origin TEXT NOT NULL CHECK (origin IN ('import', 'local'))"
	for _, statement := range []string{
		"CREATE TABLE IF NOT EXISTS task_runs (id INTEGER PRIMARY KEY, task_id TEXT NOT NULL, started_ms INTEGER NOT NULL," +
			" ended_ms INTEGER, result TEXT, message TEXT NOT NULL)",
		"CREATE INDEX IF NOT EXISTS task_runs_task_started ON task_runs (task_id, started_ms)",
		"CREATE TABLE IF NOT EXISTS cleanup_policies (name TEXT PRIMARY KEY, format TEXT NOT NULL, notes TEXT NOT NULL," +
			" criteria TEXT NOT NULL," + origin + ") WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS tasks (id TEXT PRIMARY KEY, name TEXT NOT NULL, type_id TEXT NOT NULL," +
			" enabled INTEGER NOT NULL, schedule TEXT NOT NULL, properties TEXT NOT NULL, imported_state TEXT," + origin +
			") WITHOUT ROWID",
		"CREATE TABLE IF NOT EXISTS capabilities (id TEXT PRIMARY KEY, type_id TEXT NOT NULL, enabled INTEGER NOT NULL," +
			" notes TEXT NOT NULL, properties TEXT NOT NULL," + origin + ") WITHOUT ROWID",
	} {
		if _, err := tx.ExecContext(bg, statement); err != nil {
			return err
		}
	}
	return nil
}

// errUnchanged ends an Update that finds nothing to change, so nothing is
// written and the generation stays.
var errUnchanged = errors.New("nothing to change")

// checkOrigin refuses an origin other than import and local, before the
// CHECK of the table does.
func checkOrigin(what, origin string) error {
	if origin != model.OriginImport && origin != model.OriginLocal {
		return fmt.Errorf("%s has the origin %q; the origins are %s and %s", what, origin, model.OriginImport,
			model.OriginLocal)
	}
	return nil
}

// change runs fn on the records in an Update of kind. fn reports whether it
// changed a record; when it did not, nothing is written.
func (st *Store) change(kind Kind, fn func(snap *model.Snapshot) (bool, error)) (bool, error) {
	err := st.Update([]Kind{kind}, func(snap *model.Snapshot) error {
		changed, err := fn(snap)
		if err == nil && !changed {
			err = errUnchanged
		}
		return err
	})
	if errors.Is(err, errUnchanged) {
		return false, nil
	}
	return err == nil, err
}

// textMap is the JSON of a map of strings, {} for nil.
func textMap(m map[string]string) (string, error) {
	if m == nil {
		m = map[string]string{}
	}
	return encode(m)
}

// stringMap decodes the JSON of a map of strings; the map is never nil.
func stringMap(text string) (map[string]string, error) {
	m := map[string]string{}
	if err := decode(text, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}

// readOne reads the records of one kind with its loader in a read
// transaction.
func (st *Store) readOne(load func(*sql.Tx, *model.Snapshot) error) (*model.Snapshot, error) {
	tx, err := st.readers.BeginTx(bg, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // a read transaction has nothing to keep
	snap := &model.Snapshot{}
	if err := load(tx, snap); err != nil {
		return nil, err
	}
	return snap, nil
}
