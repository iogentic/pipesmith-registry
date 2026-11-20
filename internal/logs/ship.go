package logs

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"time"
)

// Line is one JSON line of server.log, requests.log, audit.log or tasks.log
// as the tap of Options.Ship gets it.
type Line struct {
	Kind    string     // server, requests, audit or tasks
	Time    time.Time  // the time field, in milliseconds
	Level   slog.Level // the level field
	Logger  string
	Message string // the msg field
	JSON    []byte // the line without its line break; the tap may keep it, nothing writes to it later
}

// ship hands one line of a file of kind, line break included, to the tap.
// The writers call it before they take a lock; without a tap it returns at
// once, and requests-classic.log is not shipped.
func (f *Files) ship(kind string, line []byte) {
	if f.tap != nil && len(line) > 0 && kind != kindClassic {
		f.tap(lineOf(kind, line))
	}
}

// lineOf reads the head of one JSON line of this package.
func lineOf(kind string, line []byte) Line {
	data := bytes.TrimSuffix(line, []byte{'\n'})
	l := Line{Kind: kind, JSON: data[:len(data):len(data)]}
	h, ok := headOf(data)
	if !ok {
		return l
	}
	l.Time, _ = time.Parse(timeLayout, h.Time) // a line of this package holds the layout
	l.Level, l.Logger, l.Message = levelOf(h.Level), h.Logger, h.Msg
	return l
}

// headOf decodes the four fields that every line starts with, and no more of
// the line.
func headOf(data []byte) (head, bool) {
	var h head
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return h, false
	}
	for _, field := range []struct {
		key   string
		value *string
	}{{"time", &h.Time}, {"level", &h.Level}, {"logger", &h.Logger}, {"msg", &h.Msg}} {
		key, err := dec.Token()
		if err != nil || key != field.key {
			return h, false
		}
		value, err := dec.Token()
		text, isText := value.(string)
		if err != nil || !isText {
			return h, false
		}
		*field.value = text
	}
	return h, true
}

// levelOf is the level of a name of levelName.
func levelOf(name string) slog.Level {
	switch name {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	}
	return slog.LevelInfo
}
