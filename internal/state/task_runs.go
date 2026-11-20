package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TaskRun is a row of task_runs, one run of a task. Times are UTC to the
// millisecond. Ended is zero and Result empty while the run is going.
type TaskRun struct {
	ID      int64
	TaskID  string
	Started time.Time
	Ended   time.Time
	Result  string
	Message string
}

var errNoResult = errors.New("a run ends with a result")

// BeginRun records a run of the task that started at started, without an
// end, and gives its id.
func (st *Store) BeginRun(taskID string, started time.Time) (int64, error) {
	var id int64
	err := st.write(func(tx *sql.Tx) error {
		res, err := tx.ExecContext(bg, "INSERT INTO task_runs (task_id, started_ms, message) VALUES (?, ?, '')",
			taskID, started.UnixMilli())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// EndRun records the end of a run that has none: its time, its result and
// a message, and removes the runs of its task beyond the last KeptRuns
// (ruling SB12). It refuses a run that does not exist or has ended.
func (st *Store) EndRun(runID int64, ended time.Time, result, message string) error {
	if result == "" {
		return errNoResult
	}
	return st.write(func(tx *sql.Tx) error {
		res, err := tx.ExecContext(bg, "UPDATE task_runs SET ended_ms = ?, result = ?, message = ? WHERE id = ? AND ended_ms IS NULL",
			ended.UnixMilli(), result, message, runID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n == 0 {
			err = fmt.Errorf("run %d does not exist or has ended", runID)
		}
		if err != nil {
			return err
		}
		var taskID string
		if err := tx.QueryRowContext(bg, "SELECT task_id FROM task_runs WHERE id = ?", runID).Scan(&taskID); err != nil {
			return err
		}
		_, err = tx.ExecContext(bg, trimTask, taskID, taskID, KeptRuns)
		return err
	})
}

// EndOpenRuns ends every run without an end at ended with result and
// message, removes the runs of each task beyond the last KeptRuns, and gives
// the count of the ended runs.
func (st *Store) EndOpenRuns(ended time.Time, result, message string) (int64, error) {
	if result == "" {
		return 0, errNoResult
	}
	var n int64
	err := st.write(func(tx *sql.Tx) error {
		res, err := tx.ExecContext(bg, "UPDATE task_runs SET ended_ms = ?, result = ?, message = ? WHERE ended_ms IS NULL",
			ended.UnixMilli(), result, message)
		if err != nil {
			return err
		}
		if n, err = res.RowsAffected(); err != nil {
			return err
		}
		_, err = tx.ExecContext(bg, trimAll, KeptRuns)
		return err
	})
	return n, err
}

// TaskRuns are the runs of a task in the order of their start. The list is
// never nil.
func (st *Store) TaskRuns(taskID string) ([]TaskRun, error) {
	rows, err := st.readers.QueryContext(bg, "SELECT id, task_id, started_ms, ended_ms, result, message FROM task_runs"+
		" WHERE task_id = ? ORDER BY started_ms, id", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []TaskRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// LastRuns is the last run of each task that has one, by task id: the run
// with the latest start, of runs of one start the one recorded last. The
// map is never nil.
func (st *Store) LastRuns() (map[string]TaskRun, error) {
	rows, err := st.readers.QueryContext(bg, "SELECT r.id, r.task_id, r.started_ms, r.ended_ms, r.result, r.message FROM task_runs r"+
		" WHERE r.id = (SELECT l.id FROM task_runs l WHERE l.task_id = r.task_id ORDER BY l.started_ms DESC, l.id DESC LIMIT 1)")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	last := map[string]TaskRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		last[r.TaskID] = r
	}
	return last, rows.Err()
}

// scanRun reads one row of the columns id, task_id, started_ms, ended_ms,
// result and message.
func scanRun(rows *sql.Rows) (TaskRun, error) {
	var r TaskRun
	var started int64
	var ended sql.NullInt64
	var result sql.NullString
	if err := rows.Scan(&r.ID, &r.TaskID, &started, &ended, &result, &r.Message); err != nil {
		return TaskRun{}, err
	}
	r.Started = time.UnixMilli(started).UTC()
	if ended.Valid {
		r.Ended = time.UnixMilli(ended.Int64).UTC()
	}
	r.Result = result.String
	return r, nil
}

// KeptRuns is how many runs of each task the table keeps (ruling SB12).
const KeptRuns = 1000

const lastRunQuery = "SELECT id, task_id, started_ms, ended_ms, result, message FROM task_runs WHERE task_id = ? ORDER BY started_ms DESC, id DESC LIMIT 1"

// LastRun is the last run of the task, as LastRuns gives it, read for that
// task alone through the index on task and start; false when it has none.
func (st *Store) LastRun(taskID string) (TaskRun, bool, error) {
	rows, err := st.readers.QueryContext(bg, lastRunQuery, taskID)
	if err != nil {
		return TaskRun{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return TaskRun{}, false, rows.Err()
	}
	r, err := scanRun(rows)
	return r, err == nil, err
}

// trimTask removes the runs of the task beyond the last KeptRuns.
const trimTask = "DELETE FROM task_runs WHERE task_id = ? AND id NOT IN (SELECT id FROM task_runs WHERE task_id = ?" +
	" ORDER BY started_ms DESC, id DESC LIMIT ?)"

// trimAll removes the runs of every task beyond its last KeptRuns.
const trimAll = "DELETE FROM task_runs WHERE id IN (SELECT id FROM (SELECT id, ROW_NUMBER() OVER" +
	" (PARTITION BY task_id ORDER BY started_ms DESC, id DESC) AS n FROM task_runs) WHERE n > ?)"
