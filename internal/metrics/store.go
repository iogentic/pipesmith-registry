package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/migrate"

	_ "modernc.org/sqlite" // the driver "sqlite"
)

// FileName is the file of the samples in the data directory.
const FileName = "metrics.sqlite"

// SchemaVersion is the version of the last step of metrics.sqlite.
const SchemaVersion = 1

// The two resolutions of a sample row, in seconds, and how long a row of
// each is kept: rows of 1 minute for 48 hours, then rows of 15 minutes for
// 35 days.
const (
	Minute        = 60
	Quarter       = 900
	MinuteKeep    = 48 * time.Hour
	QuarterKeep   = 35 * 24 * time.Hour
	busyTimeoutMS = 30_000
)

var bg = context.Background()

// steps of metrics.sqlite. A sample holds the counts of its interval
// (requests, bytes, CPU time) and the gauges at its end (memory); at_ms is
// the start of the interval.
var steps = []migrate.Step{
	{Version: 1, Apply: func(tx *sql.Tx) error {
		_, err := tx.ExecContext(bg, "CREATE TABLE IF NOT EXISTS samples (resolution INTEGER NOT NULL, at_ms INTEGER NOT NULL,"+
			" requests INTEGER NOT NULL, bytes_sent INTEGER NOT NULL, bytes_received INTEGER NOT NULL, cpu_ms INTEGER NOT NULL,"+
			" memory_bytes INTEGER NOT NULL, PRIMARY KEY (resolution, at_ms)) WITHOUT ROWID")
		return err
	}},
}

// Sample is one row of metrics.sqlite.
type Sample struct {
	Resolution    int // Minute or Quarter
	At            time.Time
	Requests      int64
	BytesSent     int64
	BytesReceived int64
	CPU           time.Duration
	MemoryBytes   int64
}

// Store is metrics.sqlite: one connection, WAL mode.
type Store struct {
	path string
	db   *sql.DB
}

// Path is metrics.sqlite of a data directory.
func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }

// Open opens metrics.sqlite of dataDir, made with mode 0600 when it does
// not exist, and brings it to SchemaVersion.
func Open(dataDir string) (*Store, error) {
	return openWith(Path(dataDir), steps)
}

func openWith(path string, steps []migrate.Step) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- metrics.sqlite of the data directory the operator configured.
	switch {
	case err == nil:
		if err := f.Close(); err != nil {
			return nil, err
		}
	case errors.Is(err, fs.ErrExist):
	default:
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + "?" + url.Values{"mode": {"rw"},
		"_txlock": {"immediate"}, "_pragma": {fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS), "synchronous(NORMAL)"}}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := migrate.Run(db, path, steps, migrate.Options{Kind: "metrics file", Create: true}); err != nil {
		_ = db.Close() // the migration error is returned
		return nil, err
	}
	return &Store{path: path, db: db}, nil
}

// File is the path of the file.
func (s *Store) File() string { return s.path }

// Close closes the file.
func (s *Store) Close() error { return s.db.Close() }

// Add writes one sample; a row of the same resolution and start is
// replaced.
func (s *Store) Add(x Sample) error {
	_, err := s.db.ExecContext(bg, "INSERT OR REPLACE INTO samples VALUES (?, ?, ?, ?, ?, ?, ?)", x.Resolution,
		x.At.UnixMilli(), x.Requests, x.BytesSent, x.BytesReceived, x.CPU.Milliseconds(), x.MemoryBytes)
	return err
}

// Compact keeps the rules of the two resolutions at the time now: the rows
// of 1 minute older than MinuteKeep become rows of 15 minutes (counts
// summed, memory the highest), and rows of 15 minutes older than QuarterKeep
// go. One transaction.
func (s *Store) Compact(now time.Time) error {
	tx, err := s.db.BeginTx(bg, nil)
	if err != nil {
		return err
	}
	edge := now.Add(-MinuteKeep).UnixMilli()
	edge -= edge % (Quarter * 1000) // a quarter is folded whole
	statements := []struct {
		query string
		args  []any
	}{
		{"INSERT INTO samples SELECT ?, (at_ms / ?) * ? AS q, SUM(requests), SUM(bytes_sent), SUM(bytes_received), SUM(cpu_ms)," +
			" MAX(memory_bytes) FROM samples WHERE resolution = ? AND at_ms < ? GROUP BY q" +
			" ON CONFLICT (resolution, at_ms) DO UPDATE SET requests = requests + excluded.requests," +
			" bytes_sent = bytes_sent + excluded.bytes_sent, bytes_received = bytes_received + excluded.bytes_received," +
			" cpu_ms = cpu_ms + excluded.cpu_ms, memory_bytes = MAX(memory_bytes, excluded.memory_bytes)",
			[]any{Quarter, Quarter * 1000, Quarter * 1000, Minute, edge}},
		{"DELETE FROM samples WHERE resolution = ? AND at_ms < ?", []any{Minute, edge}},
		{"DELETE FROM samples WHERE resolution = ? AND at_ms < ?", []any{Quarter, now.Add(-QuarterKeep).UnixMilli()}},
	}
	for _, st := range statements {
		if _, err := tx.ExecContext(bg, st.query, st.args...); err != nil {
			_ = tx.Rollback() // the statement error is returned
			return err
		}
	}
	return tx.Commit()
}

// Point is one step of a series: the counts of its interval and the highest
// memory in it.
type Point struct {
	At            time.Time
	Requests      int64
	BytesSent     int64
	BytesReceived int64
	CPU           time.Duration
	MemoryBytes   int64
}

// Series are the points of step width from from up to before to, rows of
// both resolutions summed into them; a step without a row is left out.
func (s *Store) Series(from, to time.Time, step time.Duration) ([]Point, error) {
	width := step.Milliseconds()
	if width <= 0 {
		return nil, errors.New("metrics: a step of the series must be positive")
	}
	rows, err := s.db.QueryContext(bg, "SELECT (at_ms / ?) * ? AS b, SUM(requests), SUM(bytes_sent), SUM(bytes_received),"+
		" SUM(cpu_ms), MAX(memory_bytes) FROM samples WHERE at_ms >= ? AND at_ms < ? GROUP BY b ORDER BY b",
		width, width, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // read only; the scan error is returned
	var out []Point
	for rows.Next() {
		var p Point
		var at, cpu int64
		if err := rows.Scan(&at, &p.Requests, &p.BytesSent, &p.BytesReceived, &cpu, &p.MemoryBytes); err != nil {
			return nil, err
		}
		p.At, p.CPU = time.UnixMilli(at).UTC(), time.Duration(cpu)*time.Millisecond
		out = append(out, p)
	}
	return out, rows.Err()
}

// Count is the number of rows of a resolution.
func (s *Store) Count(resolution int) (int, error) {
	var n int
	err := s.db.QueryRowContext(bg, "SELECT COUNT(*) FROM samples WHERE resolution = ?", resolution).Scan(&n)
	return n, err
}
