package logs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// timeLayout is RFC 3339 in UTC with exactly three decimals.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// The logger of each file; server.log takes the logger attribute of a
// record, else loggerDefault.
const (
	loggerDefault  = "pipesmith_registry"
	loggerRequests = "pipesmith_registry.requests"
	loggerAudit    = "pipesmith_registry.audit"
	loggerTasks    = "pipesmith_registry.tasks"
)

// head holds the common fields of every line, in their order.
type head struct {
	Time   string `json:"time"`
	Level  string `json:"level"`
	Logger string `json:"logger"`
	Msg    string `json:"msg"`
}

func newHead(t time.Time, level slog.Level, logger, msg string) head {
	return head{Time: t.UTC().Format(timeLayout), Level: levelName(level), Logger: logger, Msg: msg}
}

func levelName(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARN"
	case level >= slog.LevelInfo:
		return "INFO"
	}
	return "DEBUG"
}

// encode is one line of v as encoding/json writes it, HTML characters kept,
// with 0x7f and U+0080 to U+009F as \u00HH (decision P7-24).
// A value it cannot encode is written as its text.
func encode(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return encode(fmt.Sprint(v))
	}
	return jsonControls(b.Bytes())
}

// value is one JSON value, without the line break of encode.
func value(v any) []byte { return bytes.TrimSuffix(encode(v), []byte{'\n'}) }

// durationMS is a duration in milliseconds with microsecond precision.
func durationMS(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

type requestLine struct {
	head
	RequestID     string  `json:"request_id"`
	Client        string  `json:"client"`
	Subject       string  `json:"subject"`
	Realm         string  `json:"realm"`
	Method        string  `json:"method"`
	Path          string  `json:"path"`
	Query         string  `json:"query"`
	Protocol      string  `json:"protocol"`
	Status        int     `json:"status"`
	RequestBytes  int64   `json:"request_bytes"`
	ResponseBytes int64   `json:"response_bytes"`
	DurationMS    float64 `json:"duration_ms"`
	UserAgent     string  `json:"user_agent"`
	Repository    string  `json:"repository"`
	Format        string  `json:"format"`
	Port          int     `json:"port"`
}

func (r Request) line(now time.Time) []byte {
	r = r.capped()
	t := r.Time
	if t.IsZero() {
		t = now
	}
	return encode(requestLine{head: newHead(t, slog.LevelInfo, loggerRequests, r.Method+" "+r.Path+" "+strconv.Itoa(r.Status)),
		RequestID: r.ID, Client: r.Client, Subject: r.Subject, Realm: r.Realm, Method: r.Method, Path: r.Path, Query: r.Query,
		Protocol: r.Protocol, Status: r.Status, RequestBytes: r.RequestBytes, ResponseBytes: r.ResponseBytes,
		DurationMS: durationMS(r.Duration), UserAgent: r.UserAgent, Repository: r.Repository, Format: r.Format, Port: r.Port})
}

type auditLine struct {
	head
	RequestID  string   `json:"request_id"`
	Subject    string   `json:"subject"`
	Realm      string   `json:"realm"`
	Client     string   `json:"client"`
	Action     string   `json:"action"`
	ObjectType string   `json:"object_type"`
	ObjectName string   `json:"object_name"`
	Result     string   `json:"result"`
	Fields     []string `json:"fields"`
	Reason     string   `json:"reason,omitempty"`
}

func (e AuditEvent) line(now time.Time) []byte {
	fields := e.Fields
	if fields == nil {
		fields = []string{}
	}
	return encode(auditLine{head: newHead(now, slog.LevelInfo, loggerAudit, e.Action+" "+e.Result), RequestID: e.RequestID,
		Subject: e.Subject, Realm: e.Realm, Client: e.Client, Action: e.Action, ObjectType: e.ObjectType,
		ObjectName: e.ObjectName, Result: e.Result, Fields: fields, Reason: e.Reason})
}

type taskLine struct {
	head
	RunID      string           `json:"run_id"`
	TaskID     string           `json:"task_id"`
	TaskType   string           `json:"task_type"`
	Event      string           `json:"event"`
	Result     string           `json:"result"`
	Counts     map[string]int64 `json:"counts"`
	DurationMS float64          `json:"duration_ms"`
}

func (e TaskEvent) line(now time.Time) []byte {
	counts := e.Counts
	if counts == nil {
		counts = map[string]int64{}
	}
	return encode(taskLine{head: newHead(now, slog.LevelInfo, loggerTasks, e.TaskType+" "+e.Event), RunID: e.RunID,
		TaskID: e.TaskID, TaskType: e.TaskType, Event: e.Event, Result: e.Result, Counts: counts,
		DurationMS: durationMS(e.Duration)})
}

// commonKeys are the keys an attribute of a server record cannot take; it
// is written as attr.<key>.
var commonKeys = map[string]bool{"time": true, "level": true, "logger": true, "msg": true}

// serverLine is the line of one record: the common fields, then the
// attributes, those of the handler first.
func serverLine(t time.Time, level slog.Level, logger, msg string, attrs []byte) []byte {
	line := bytes.TrimSuffix(encode(newHead(t, level, logger, msg)), []byte("}\n"))
	line = append(line, attrs...)
	return append(line, "}\n"...)
}

// appendAttr appends `,"key":value` for one attribute; an empty attribute
// and an empty group add nothing, a group without a key adds its members.
func appendAttr(buf []byte, a slog.Attr, top bool) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return buf
	}
	key := a.Key
	if top && commonKeys[key] {
		key = "attr." + key
	}
	if a.Value.Kind() == slog.KindGroup {
		members := a.Value.Group()
		if key == "" {
			for _, m := range members {
				buf = appendAttr(buf, m, top)
			}
			return buf
		}
		var inner []byte
		for _, m := range members {
			inner = appendAttr(inner, m, false)
		}
		if len(inner) == 0 {
			return buf
		}
		buf = append(append(append(buf, ','), value(key)...), ":{"...)
		return append(append(buf, inner[1:]...), '}')
	}
	buf = append(append(append(buf, ','), value(key)...), ':')
	return append(buf, attrValue(a.Value)...)
}

// attrValue is one value as slog's JSON handler writes it: a duration in
// nanoseconds, an error as its text.
func attrValue(v slog.Value) []byte {
	switch v.Kind() {
	case slog.KindString:
		return value(v.String())
	case slog.KindInt64:
		return strconv.AppendInt(nil, v.Int64(), 10)
	case slog.KindUint64:
		return strconv.AppendUint(nil, v.Uint64(), 10)
	case slog.KindFloat64:
		return value(v.Float64())
	case slog.KindBool:
		return strconv.AppendBool(nil, v.Bool())
	case slog.KindDuration:
		return strconv.AppendInt(nil, int64(v.Duration()), 10)
	case slog.KindTime:
		return value(v.Time())
	}
	a := v.Any()
	if err, ok := a.(error); ok {
		if _, marshals := a.(json.Marshaler); !marshals {
			return value(err.Error())
		}
	}
	return value(a)
}
