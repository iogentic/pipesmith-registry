package catalog

import (
	"database/sql"
	"errors"
	"time"
)

// The tables downloads and request_days of version 4 (decisions L11 and L12
// of the phase 7 plan). A day is YYYY-MM-DD in UTC. A row is added to, never
// replaced, so counts of several flushes of one day sum up.

// DayLayout is the layout of a day in both tables.
const DayLayout = time.DateOnly

// Download is the count and the bytes of the downloads of one repository on
// one day.
type Download struct {
	Repo  string
	Day   string
	Count int64
	Bytes int64
}

// RequestDay is the count of the requests of one day and the bytes of their
// answers.
type RequestDay struct {
	Day       string
	Requests  int64
	BytesSent int64
}

var errUsageKey = errors.New("catalog: a usage row needs a repository and a day of the form YYYY-MM-DD")

func validDay(day string) bool {
	_, err := time.Parse(DayLayout, day)
	return err == nil
}

// AddUsage adds the counts to their rows in one transaction; a row that does
// not exist yet is made.
func (c *Catalog) AddUsage(downloads []Download, days []RequestDay) error {
	for _, d := range downloads {
		if d.Repo == "" || !validDay(d.Day) {
			return errUsageKey
		}
	}
	for _, d := range days {
		if !validDay(d.Day) {
			return errUsageKey
		}
	}
	if len(downloads) == 0 && len(days) == 0 {
		return nil
	}
	return c.write(func(tx *sql.Tx) error {
		for _, d := range downloads {
			if _, err := tx.ExecContext(bg, "INSERT INTO downloads VALUES (?, ?, ?, ?) ON CONFLICT (repo, day) DO UPDATE"+
				" SET count = count + excluded.count, bytes = bytes + excluded.bytes", d.Repo, d.Day, d.Count, d.Bytes); err != nil {
				return err
			}
		}
		for _, d := range days {
			if _, err := tx.ExecContext(bg, "INSERT INTO request_days VALUES (?, ?, ?) ON CONFLICT (day) DO UPDATE"+
				" SET requests = requests + excluded.requests, bytes_sent = bytes_sent + excluded.bytes_sent",
				d.Day, d.Requests, d.BytesSent); err != nil {
				return err
			}
		}
		return nil
	})
}

// Downloads are the rows of the days from to to, both included, ordered by
// day and repository.
func (c *Catalog) Downloads(from, to string) ([]Download, error) {
	var out []Download
	err := c.read(func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(bg, "SELECT repo, day, count, bytes FROM downloads WHERE day >= ? AND day <= ?"+
			" ORDER BY day, repo", from, to)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }() // read only; the scan error is the one returned
		for rows.Next() {
			var d Download
			if err := rows.Scan(&d.Repo, &d.Day, &d.Count, &d.Bytes); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// RequestDays are the rows of the days from to to, both included, ordered by
// day.
func (c *Catalog) RequestDays(from, to string) ([]RequestDay, error) {
	var out []RequestDay
	err := c.read(func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(bg, "SELECT day, requests, bytes_sent FROM request_days WHERE day >= ? AND day <= ?"+
			" ORDER BY day", from, to)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }() // read only; the scan error is the one returned
		for rows.Next() {
			var d RequestDay
			if err := rows.Scan(&d.Day, &d.Requests, &d.BytesSent); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}
