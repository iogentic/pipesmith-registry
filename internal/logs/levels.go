package logs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
)

// LevelsKey is the key of the logger levels in the table settings of
// state.sqlite (L10): a JSON object of logger name and level, for the
// loggers whose level is not the default.
const LevelsKey = "log_levels"

// defaultLevel is the level of every logger without a setting.
const defaultLevel = slog.LevelInfo

// Loggers are the logger names of this server whose lines reach server.log
// and the log targets, sorted. The lines of requests.log, audit.log and
// tasks.log are written apart from them and take no level.
var Loggers = []string{
	"pipesmith_registry",
	"pipesmith_registry.catalog",
	"pipesmith_registry.listeners",
	"pipesmith_registry.log_target",
	"pipesmith_registry.logs",
	"pipesmith_registry.logship",
	"pipesmith_registry.manifest",
	"pipesmith_registry.metrics",
	"pipesmith_registry.proxy",
	"pipesmith_registry.security",
	"pipesmith_registry.server",
	"pipesmith_registry.stores",
	"pipesmith_registry.tasks",
	"pipesmith_registry.update",
}

// ErrUnknownLogger refuses a name that is not in Loggers; no logger is added.
var ErrUnknownLogger = errors.New("this server has no logger of that name")

// ErrLevel refuses a level that is not one of the four.
var ErrLevel = errors.New("the level is DEBUG, INFO, WARN or ERROR")

// LoggerLevel is one row of the logger list.
type LoggerLevel struct {
	Name    string
	Level   string
	Default string
}

// Levels are the levels of the loggers. Enabled reads without a lock; a
// change is saved before it applies.
type Levels struct {
	mu   sync.Mutex // serializes the changes and their saves
	set  atomic.Pointer[map[string]slog.Level]
	save func(map[string]string) error
}

// NewLevels holds every logger at INFO. save stores the levels that differ
// from the default after each change; nil stores nothing.
func NewLevels(save func(map[string]string) error) *Levels {
	l := &Levels{save: save}
	l.set.Store(&map[string]slog.Level{})
	return l
}

// parseLevel reads one of the four names of the contract.
func parseLevel(name string) (slog.Level, error) {
	switch name {
	case "DEBUG", "INFO", "WARN", "ERROR":
		return levelOf(name), nil
	}
	return 0, ErrLevel
}

// Load applies the setting log_levels; a name or level it does not know is
// left out with one warning each.
func (l *Levels) Load(data []byte) []string {
	var doc map[string]any
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil || doc == nil {
		return []string{"the setting log_levels is not a JSON object; every logger is at INFO"}
	}
	set := map[string]slog.Level{}
	var warnings []string
	for _, name := range slices.Sorted(maps.Keys(doc)) {
		text, _ := doc[name].(string)
		level, err := parseLevel(text)
		switch {
		case !slices.Contains(Loggers, name):
			warnings = append(warnings, fmt.Sprintf("the setting log_levels: this server has no logger %q; it is left out", name))
		case err != nil:
			warnings = append(warnings, fmt.Sprintf("the setting log_levels: the level of %s is not DEBUG, INFO, WARN or ERROR; it stays at INFO", name))
		case level != defaultLevel:
			set[name] = level
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.set.Store(&set)
	return warnings
}

// Enabled reports whether a record of logger at level is written. A logger
// outside Loggers is at INFO; nil writes every record.
func (l *Levels) Enabled(logger string, level slog.Level) bool {
	if l == nil {
		return true
	}
	min, ok := (*l.set.Load())[logger]
	if !ok {
		min = defaultLevel
	}
	return level >= min
}

// List is every logger with its level and default, in the order of Loggers.
func (l *Levels) List() []LoggerLevel {
	set := *l.set.Load()
	out := make([]LoggerLevel, 0, len(Loggers))
	for _, name := range Loggers {
		level, ok := set[name]
		if !ok {
			level = defaultLevel
		}
		out = append(out, LoggerLevel{Name: name, Level: levelName(level), Default: levelName(defaultLevel)})
	}
	return out
}

// Set gives one logger a level.
func (l *Levels) Set(name, level string) error {
	if !slices.Contains(Loggers, name) {
		return ErrUnknownLogger
	}
	value, err := parseLevel(level)
	if err != nil {
		return err
	}
	return l.change(func(set map[string]slog.Level) {
		if value == defaultLevel {
			delete(set, name)
		} else {
			set[name] = value
		}
	})
}

// Remove puts one logger back to its default.
func (l *Levels) Remove(name string) error {
	if !slices.Contains(Loggers, name) {
		return ErrUnknownLogger
	}
	return l.change(func(set map[string]slog.Level) { delete(set, name) })
}

// Reset puts every logger back to its default.
func (l *Levels) Reset() error {
	return l.change(func(set map[string]slog.Level) { clear(set) })
}

// change saves the levels fn makes of a copy, then applies them; a failed
// save changes nothing.
func (l *Levels) change(fn func(map[string]slog.Level)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	next := maps.Clone(*l.set.Load())
	fn(next)
	if l.save != nil {
		stored := make(map[string]string, len(next))
		for name, level := range next {
			stored[name] = levelName(level)
		}
		if err := l.save(stored); err != nil {
			return err
		}
	}
	l.set.Store(&next)
	return nil
}

// HandlerAt is the slog.Handler of server.log that drops a record below the
// level of its logger; nil levels drop none, as Handler.
func (f *Files) HandlerAt(l *Levels) slog.Handler { return &handler{files: f, levels: l} }
