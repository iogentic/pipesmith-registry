package server

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// The log routes of task 7.24 of the phase 7 plan, sections S30 to S32 of
// docs/ui/API.md: the list of the log files, the end of one file or the
// whole file, and the levels of the loggers (L10). The permissions are the
// ones the contract names.

const (
	loggingRead   = "nexus:logging:read"
	loggingUpdate = "nexus:logging:update"
	logsBase      = "/api/logs"
	loggersBase   = "/api/logging/loggers"
)

// tailSizes are the ends of a file the viewer asks for; the first is the
// default.
var tailSizes = []int64{25600, 51200, 102400}

// maxLevelBody bounds the body of a change of level.
const maxLevelBody = 4096

// LogAccess is what the log routes read: the log directory, empty while
// the files are off, and the levels of the loggers.
type LogAccess struct {
	Dir    string
	Levels *logs.Levels
}

// logView holds the LogAccess of the server; nil answers the log routes
// with 503.
type logView struct{ p atomic.Pointer[LogAccess] }

func (v *logView) set(a *LogAccess) { v.p.Store(a) }

var logRoutes = []route{
	get(loggersBase, perm(loggingRead), (*Server).loggers),
	entry(exact(loggersBase+"/"+segment), []string{"PUT"}, perm(loggingUpdate), (*Server).setLogger),
	entry(exact(loggersBase+"/"+segment), []string{"DELETE"}, perm(loggingUpdate), (*Server).removeLogger),
	entry(exact("/api/logging/reset"), []string{"POST"}, perm(loggingUpdate), (*Server).resetLoggers),
	get(logsBase, perm(loggingRead), (*Server).logFileList),
	get(logsBase+"/(.+)", perm(loggingRead), (*Server).logFile),
}

// logAccess is the LogAccess of the server, or nil after the 503.
func (s *Server) logAccess(w *recorder, req *request, levels bool) *LogAccess {
	a := s.logging.p.Load()
	if a == nil || (levels && a.Levels == nil) {
		s.message(w, req, http.StatusServiceUnavailable, "the log files and levels of this server are not open")
		return nil
	}
	return a
}

type loggerXO struct {
	Name    string `json:"name"`
	Level   string `json:"level"`
	Default string `json:"default"`
}

func (s *Server) loggers(w *recorder, req *request, _ []string) error {
	a := s.logAccess(w, req, true)
	if a == nil {
		return nil
	}
	out := []loggerXO{}
	for _, l := range a.Levels.List() {
		out = append(out, loggerXO{Name: l.Name, Level: l.Level, Default: l.Default})
	}
	return s.writeAnswer(w, req, rest.Answer{Status: http.StatusOK, Body: out})
}

// levelChanged answers a change of level: 204, or 400 at the field the
// refusal names.
func (s *Server) levelChanged(w *recorder, req *request, err error) error {
	switch {
	case err == nil:
		writeBody(w, req, http.StatusNoContent, [][2]string{{"Cache-Control", "no-store"}}, nil)
		return nil
	case errors.Is(err, logs.ErrUnknownLogger):
		return s.writeAnswer(w, req, rest.Invalid(rest.FieldError{ID: "name", Message: err.Error()}))
	case errors.Is(err, logs.ErrLevel):
		return s.writeAnswer(w, req, rest.Invalid(rest.FieldError{ID: "level", Message: err.Error()}))
	}
	return err
}

func (s *Server) setLogger(w *recorder, req *request, m []string) error {
	a := s.logAccess(w, req, true)
	if a == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(req.r.Body, maxLevelBody+1))
	if err != nil {
		return err
	}
	var body struct {
		Level *string `json:"level"`
	}
	if len(data) > maxLevelBody || json.Unmarshal(data, &body) != nil {
		return s.writeAnswer(w, req, rest.Invalid(rest.FieldError{ID: "*", Message: `the body is a JSON object {"level"} of at most 4096 bytes`}))
	}
	level := ""
	if body.Level != nil {
		level = *body.Level
	}
	return s.levelChanged(w, req, a.Levels.Set(m[1], level))
}

func (s *Server) removeLogger(w *recorder, req *request, m []string) error {
	a := s.logAccess(w, req, true)
	if a == nil {
		return nil
	}
	return s.levelChanged(w, req, a.Levels.Remove(m[1]))
}

func (s *Server) resetLoggers(w *recorder, req *request, _ []string) error {
	a := s.logAccess(w, req, true)
	if a == nil {
		return nil
	}
	return s.levelChanged(w, req, a.Levels.Reset())
}

type logFileXO struct {
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	LastModified string `json:"lastModified"`
}

// isoMillis is an ISO 8601 time in UTC with milliseconds.
const isoMillis = "2006-01-02T15:04:05.000Z"

func (s *Server) logFileList(w *recorder, req *request, _ []string) error {
	a := s.logAccess(w, req, false)
	if a == nil {
		return nil
	}
	files, err := logs.ListFiles(a.Dir)
	if err != nil {
		return err
	}
	out := []logFileXO{}
	for _, f := range files {
		out = append(out, logFileXO{Name: f.Name, Size: f.Size, LastModified: f.Modified.UTC().Format(isoMillis)})
	}
	return s.writeAnswer(w, req, rest.Answer{Status: http.StatusOK, Body: out})
}

// encodedSeparator is a slash or backslash written as %2F or %5C in the
// raw path.
func encodedSeparator(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c")
}

// logQuery reads bytes and download of the query: the end of the file to
// answer, or download true. Any other value is an error.
func logQuery(query string) (tail int64, download bool, err error) {
	q, err := url.ParseQuery(query)
	if err != nil {
		return 0, false, err
	}
	if d, ok := q["download"]; ok {
		if len(d) != 1 || d[0] != "1" {
			return 0, false, errors.New("download is 1")
		}
		return 0, true, nil
	}
	b, ok := q["bytes"]
	if !ok {
		return tailSizes[0], false, nil
	}
	if len(b) == 1 {
		if n, err := strconv.ParseInt(b[0], 10, 64); err == nil {
			for _, size := range tailSizes {
				if n == size {
					return n, false, nil
				}
			}
		}
	}
	return 0, false, errors.New("bytes is 25600, 51200 or 102400")
}

func (s *Server) logFile(w *recorder, req *request, m []string) error {
	a := s.logAccess(w, req, false)
	if a == nil {
		return nil
	}
	if encodedSeparator(req.raw) {
		s.message(w, req, http.StatusBadRequest, logs.ErrName.Error())
		return nil
	}
	tail, download, err := logQuery(req.query)
	if err != nil {
		s.message(w, req, http.StatusBadRequest, err.Error())
		return nil
	}
	f, err := logs.OpenFile(a.Dir, m[1])
	switch {
	case errors.Is(err, logs.ErrName):
		s.message(w, req, http.StatusBadRequest, err.Error())
		return nil
	case errors.Is(err, fs.ErrNotExist):
		s.message(w, req, http.StatusNotFound, "no log file of that name")
		return nil
	case err != nil:
		return err
	}
	defer func() { _ = f.Close() }() // read only
	text := [][2]string{{"Content-Type", "text/plain; charset=utf-8"}, {"X-Content-Type-Options", "nosniff"}, {"Cache-Control", "no-store"}}
	if !download {
		data, err := f.Tail(tail)
		if err != nil {
			return err
		}
		writeBody(w, req, http.StatusOK, text, data)
		return nil
	}
	h := w.Header()
	for _, kv := range text {
		h.Set(kv[0], kv[1])
	}
	if f.Compressed {
		h.Set("Content-Type", "application/gzip")
	}
	h.Set("Content-Disposition", `attachment; filename="`+m[1]+`"`)
	h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f.Reader()) // a client that goes away ends the copy
	return nil
}
