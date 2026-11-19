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
// a message. It refuses a run that does not exist or has ended.
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
		return err
	})
}

// EndOpenRuns ends every run without an end at ended with result and
// message, and gives their count.
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
		n, err = res.RowsAffected()
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
		var r TaskRun
		var started int64
		var ended sql.NullInt64
		var result sql.NullString
		if err := rows.Scan(&r.ID, &r.TaskID, &started, &ended, &result, &r.Message); err != nil {
			return nil, err
		}
		r.Started = time.UnixMilli(started).UTC()
		if ended.Valid {
			r.Ended = time.UnixMilli(ended.Int64).UTC()
		}
		r.Result = result.String
		runs = append(runs, r)
	}
	return runs, rows.Err()
}
