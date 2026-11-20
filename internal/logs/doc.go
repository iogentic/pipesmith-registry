// Package logs writes the log files of serve into one directory, one JSON
// object per line: server.log (the records of log/slog, a second sink of
// the lines on standard error), requests.log (one line per request),
// audit.log and tasks.log. Every line starts with time, level, logger and
// msg; values are encoded by encoding/json, so no control byte reaches a
// file raw.
//
// requests-classic.log holds the same requests as one text line each, in
// the field order of the request log of the old product (classic.go), while
// Settings.Classic is true.
//
// Request lines and server lines below WARN wait in memory and are written
// by Tick, once a second under Start, and by Close. Audit lines and lines
// from WARN upward are written before the call returns. A failed write never
// fails the caller: Errors counts the lines that did not reach their file.
//
// A failed open or write closes the file and turns Health red; the file is
// opened again at most every 5 s of the clock, and the next success turns
// it green. A directory that cannot be made at Open is the same failure:
// Open returns usable Files all the same.
//
// A file rotates before a write would take it past Settings.MaxBytes and at
// the day change in UTC: it is renamed to <kind>-<day>.<n>.log and a new
// file takes its place. A goroutine that runs while there is work compresses
// the renamed files to <kind>-<day>.<n>.log.gz, one at a time and outside
// the locks of the writers; Close waits for it. Open and the hourly run
// compress every rotated file that a crash, a failed compression or an
// operator left uncompressed.
//
// The same goroutine removes a rotated file once its day plus one day plus
// Settings.MaxAge passed, and the oldest rotated files of a kind while the
// kind holds more than Settings.MaxTotalBytes: at Open, after each rotation
// and once per hour of the clock. The active files and files without one of
// the names of this package are never removed.
//
// With Options.Dir empty the files are off (off.go): Open makes no
// directory and opens no file, the writers hand their JSON lines to the tap
// of Options.Ship only, nothing rotates and nothing is kept, and the classic
// line is not made. Without a tap every writer returns at once.
//
// Request is the record of one request: internal/server fills it once the
// answer ended. A request that net/http refuses before the handler runs (a
// head above the size limit, a request line its parser rejects) gets no
// record.
//
// The package imports no other package of this module.
package logs
