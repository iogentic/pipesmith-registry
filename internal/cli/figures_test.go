package cli

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/metrics"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// serve counts the downloads of L11 and the requests of the day, and writes
// them into catalog.sqlite at the latest when it stops.
func TestServeWritesTheDailyCountsAtTheStop(t *testing.T) {
	dir := dataDir(t)
	noLogDirVariable(t)
	tree := demoTree(t, filepath.Dir(dir))
	if code, out, errOut := run(t, "store", "add", "tree", "--kind", "directory", "--root", tree, "--repos", "demo-raw"); code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if code, out, errOut := run(t, "scan", "tree"); code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	done, _, port := servingWith(t)
	for range 2 {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/repository/demo-raw/top.txt", port))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || string(body) != "top level" {
			t.Fatalf("%d %q", resp.StatusCode, body)
		}
	}
	stopServing(t, done)
	c, err := catalog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	from, to := time.Now().UTC().AddDate(0, 0, -1).Format(catalog.DayLayout), time.Now().UTC().AddDate(0, 0, 1).Format(catalog.DayLayout)
	rows, err := c.Downloads(from, to)
	if err != nil {
		t.Fatal(err)
	}
	var count, bytes int64
	for _, r := range rows {
		if r.Repo != "demo-raw" {
			t.Fatalf("row %+v", r)
		}
		count, bytes = count+r.Count, bytes+r.Bytes
	}
	if count != 2 || bytes != 2*int64(len("top level")) {
		t.Fatalf("downloads %v", rows)
	}
	days, err := c.RequestDays(from, to)
	if err != nil {
		t.Fatal(err)
	}
	var requests int64
	for _, d := range days {
		requests += d.Requests
	}
	if requests < 3 {
		t.Fatalf("request days %v: want the 2 downloads and the probe of /api/health", days)
	}
}

// The switch metrics: on without the variable and the setting; the
// variable wins over the setting; a value it does not know keeps the
// samples on with a warning.
func TestMetricsSwitch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	env := func(value string) func(string) string {
		return func(key string) string {
			if key == EnvMetrics {
				return value
			}
			return ""
		}
	}
	if on, warning := metricsOn(dir, env("")); !on || warning != "" {
		t.Fatalf("without state.sqlite: %v %q", on, warning)
	}
	st, err := state.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		setting, variable string
		on, warned        bool
	}{{"", "", true, false}, {"off", "", false, false}, {"on", "", true, false}, {"off", "on", true, false},
		{"on", "off", false, false}, {"sometimes", "", true, true}, {"", "never", true, true}} {
		if c.setting != "" {
			if err := st.PutSetting(metricsKey, c.setting); err != nil {
				t.Fatal(err)
			}
		}
		if on, warning := metricsOn(dir, env(c.variable)); on != c.on || (warning != "") != c.warned {
			t.Errorf("setting %q, variable %q: %v %q", c.setting, c.variable, on, warning)
		}
	}
	_ = st.Close()
}

// serve with the samples on makes metrics.sqlite; with the variable off it
// makes none.
func TestServeMakesMetricsFileOnlyWhenOn(t *testing.T) {
	dir := dataDir(t)
	noLogDirVariable(t)
	t.Setenv(EnvMetrics, "off")
	done, _, _ := servingWith(t)
	stopServing(t, done)
	if _, err := os.Stat(metrics.Path(dir)); err == nil {
		t.Fatal("metrics.sqlite with the samples off")
	}
	t.Setenv(EnvMetrics, "")
	done, _, _ = servingWith(t)
	stopServing(t, done)
	if _, err := os.Stat(metrics.Path(dir)); err != nil {
		t.Fatal(err)
	}
}
