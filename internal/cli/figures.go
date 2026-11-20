package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/logship"
	"github.com/iogentic/pipesmith-registry/internal/metrics"
	"github.com/iogentic/pipesmith-registry/internal/server"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// EnvMetrics switches the samples of metrics.sqlite: on or off. It wins over
// the setting metrics of state.sqlite; without both they are on.
const EnvMetrics = "PIPESMITH_REGISTRY_METRICS"

// metricsKey is the setting of state.sqlite that switches the samples, the
// JSON text "on" or "off".
const metricsKey = "metrics"

// shipping holds the manager of the log targets once logFiles started it.
type shipping struct {
	atomic.Pointer[logship.Manager]
}

func (s *shipping) stats() []logship.Stats {
	if m := s.Load(); m != nil {
		return m.Stats()
	}
	return nil
}

// figures sets opts.Figures: the counters of serve with those of the log
// targets, the daily counts of decisions L11 and L12 in catalog.sqlite, and
// while the switch metrics is on a sample every minute in metrics.sqlite.
// The function it returns writes what waits and closes what it opened.
func (c *command) figures(cfg *config.Config, opts *server.ServeOptions) func() {
	usage := &lazyCatalog{dataDir: cfg.DataDir}
	if opts.CatalogFile != nil {
		usage.cat, usage.shared = opts.CatalogFile, true // writes or upstream opened it already
	}
	r := metrics.NewRecorder(metrics.Options{Usage: usage, Logger: slog.Default(), Targets: c.shipping.stats})
	opts.Figures = r
	var store *metrics.Store
	var sampler *metrics.Sampler
	// The samples start where Serve opens the log files: once its refusals
	// have passed, so a serve that cannot listen makes no metrics.sqlite.
	openLogs := opts.Logs
	opts.Logs = func() func(logs.Request) {
		if on, warning := metricsOn(cfg.DataDir, os.Getenv); !on {
			if warning != "" {
				slog.Warn(warning, "logger", metrics.Logger)
			}
		} else if s, err := metrics.Open(cfg.DataDir); err != nil {
			slog.Warn("the samples of the figures are off: "+err.Error(), "logger", metrics.Logger)
		} else {
			store = s
			sampler = metrics.StartSampler(r, store, nil, nil, slog.Default())
			r.AttachSamples(store)
		}
		if openLogs == nil {
			return nil
		}
		return openLogs()
	}
	return func() {
		if sampler != nil {
			sampler.Stop()
			_ = store.Close() // every sample is written when Add returns
		}
		_ = r.Close() // a refused write is counted and logged by the recorder
		usage.close()
	}
}

// metricsOn reads the switch: the variable, else the setting, else on. A
// value it does not know keeps the samples on with a warning.
func metricsOn(dataDir string, getenv func(string) string) (bool, string) {
	value := getenv(EnvMetrics)
	if value == "" {
		st, err := state.Open(dataDir)
		if err == nil {
			raw, found, serr := st.Setting(metricsKey)
			_ = st.Close() // read only; nothing to keep
			if serr != nil {
				return true, "the setting metrics is not read; the samples stay on: " + serr.Error()
			}
			if found && json.Unmarshal(raw, &value) != nil {
				return true, "the setting metrics is not \"on\" or \"off\"; the samples stay on"
			}
		} else if !errors.Is(err, state.ErrAbsent) {
			return true, "the setting metrics is not read; the samples stay on: " + err.Error()
		}
	}
	switch value {
	case "", "on":
		return true, ""
	case "off":
		return false, ""
	}
	return true, "metrics is " + strconv.Quote(value) + ", not on or off; the samples stay on"
}

// lazyCatalog opens catalog.sqlite at the first write of counts, so a start
// that is refused or serves nothing makes no file. A catalog it cannot open
// turns the daily counts off with one warning; the counters still run.
type lazyCatalog struct {
	dataDir string
	mu      sync.Mutex
	cat     *catalog.Catalog
	shared  bool
	off     bool
}

func (l *lazyCatalog) AddUsage(downloads []catalog.Download, days []catalog.RequestDay) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.off {
		return nil
	}
	if l.cat == nil {
		cat, err := catalog.Open(l.dataDir)
		if err != nil {
			l.off = true
			slog.Warn("the download counts are off: "+err.Error(), "logger", metrics.Logger)
			return nil
		}
		l.cat = cat
	}
	return l.cat.AddUsage(downloads, days)
}

// Downloads and RequestDays read catalog.sqlite once it exists; before that
// there is no row.
func (l *lazyCatalog) Downloads(from, to string) ([]catalog.Download, error) {
	cat, err := l.reading()
	if cat == nil || err != nil {
		return nil, err
	}
	return cat.Downloads(from, to)
}

func (l *lazyCatalog) RequestDays(from, to string) ([]catalog.RequestDay, error) {
	cat, err := l.reading()
	if cat == nil || err != nil {
		return nil, err
	}
	return cat.RequestDays(from, to)
}

// reading is the open catalog, opened when the file exists; nil without it.
func (l *lazyCatalog) reading() (*catalog.Catalog, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cat != nil || l.off {
		return l.cat, nil
	}
	if _, err := os.Stat(catalog.Path(l.dataDir)); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	cat, err := catalog.Open(l.dataDir)
	if err != nil {
		return nil, err
	}
	l.cat = cat
	return cat, nil
}

func (l *lazyCatalog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cat != nil && !l.shared {
		_ = l.cat.Close() // the counts are written; nothing else is kept
	}
}
