package state

import "database/sql"

// schema4 creates task_runs, one row per run of a task: its start in UTC
// milliseconds, and its end and result, NULL while it runs. The step joins
// the list steps after the steps before it.
func schema4(tx *sql.Tx) error {
	for _, statement := range []string{
		"CREATE TABLE IF NOT EXISTS task_runs (id INTEGER PRIMARY KEY, task_id TEXT NOT NULL, started_ms INTEGER NOT NULL," +
			" ended_ms INTEGER, result TEXT, message TEXT NOT NULL)",
		"CREATE INDEX IF NOT EXISTS task_runs_task_started ON task_runs (task_id, started_ms)",
	} {
		if _, err := tx.ExecContext(bg, statement); err != nil {
			return err
		}
	}
	return nil
}
