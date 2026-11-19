package release

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDrainSecondsKey(t *testing.T) {
	for text, want := range map[string]int{``: 5, `"drain_seconds": 1,`: 1, `"drain_seconds": 300,`: 300, `"drain_seconds": 3600,`: 3600} {
		root := t.TempDir()
		writeConfig(t, root, `{`+text+` "unit": "pipesmith-registry.service"}`)
		cfg, err := LoadUpdateConfig(root)
		if err != nil || cfg.DrainSeconds != want {
			t.Errorf("%s: %+v %v, want %d", text, cfg, err, want)
		}
	}
	for _, text := range []string{`0`, `3601`, `-1`, `1.5`, `5e0`, `"5"`, `null`, `true`, `[5]`} {
		root := t.TempDir()
		writeConfig(t, root, `{"drain_seconds": `+text+`}`)
		if _, err := LoadUpdateConfig(root); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "drain_seconds") {
			t.Errorf("%s: %v", text, err)
		}
	}
}

// deadlines records the time a restart may take.
type deadlines struct {
	*fakeService
	left []time.Duration
}

func (d *deadlines) Restart(ctx context.Context, unit string) error {
	if dl, ok := ctx.Deadline(); ok {
		d.left = append(d.left, time.Until(dl))
	}
	return d.fakeService.Restart(ctx, unit)
}

func TestRestartWaitsForTheDrain(t *testing.T) {
	for text, want := range map[string]time.Duration{"": 3720 * time.Second,
		`, "drain_seconds": 300`: 300*time.Second + DefaultRestartTimeout} {
		r := newRig(t, "0.1.0")
		r.config(text)
		d := &deadlines{fakeService: r.svc}
		r.u.Service = d
		if err := r.u.Update(r.release(versionScript("0.2.0")), false); err != nil {
			t.Fatal(err)
		}
		if len(d.left) != 1 || d.left[0] > want || d.left[0] < want-5*time.Second {
			t.Fatalf("%q: restart time %v, want %v", text, d.left, want)
		}
	}
}

// stopDeadlines records the time the stop of the way back may take.
type stopDeadlines struct {
	*schemaService
	left []time.Duration
}

func (d *stopDeadlines) Stop(ctx context.Context, unit string) error {
	if deadline, ok := ctx.Deadline(); ok {
		d.left = append(d.left, time.Until(deadline))
	}
	return d.schemaService.Stop(ctx, unit)
}

// Without drain_seconds the bound of a restart and of a stop is the highest
// drain time plus two minutes; with the key it is that value plus two.
func TestStopWaitsForTheDrain(t *testing.T) {
	for text, want := range map[string]time.Duration{"": 3720 * time.Second, `, "drain_seconds": 300`: 420 * time.Second} {
		sr := newSchemaRig(t, schemaN+1)
		sr.config(text)
		d := &stopDeadlines{schemaService: sr.svc}
		sr.u.Service = d
		if err := sr.u.Update(sr.dir, false); !errors.Is(err, ErrHealth) {
			t.Fatalf("%q: %v", text, err)
		}
		if len(d.left) != 1 || d.left[0] > want || d.left[0] < want-5*time.Second {
			t.Fatalf("%q: stop time %v, want %v", text, d.left, want)
		}
	}
}
