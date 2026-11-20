package release

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// UnitFacts are the settings of the server unit that the update compares
// with update.json: its environment, the arguments of ExecStart and its
// stop timeout (0 when unknown or infinite).
type UnitFacts struct {
	Env         map[string]string
	Args        []string
	StopTimeout time.Duration
}

// UnitReader is a ServiceManager that reads the settings of a unit.
// update and rollback compare them with update.json when the ServiceManager
// has it; Systemctl has it.
type UnitReader interface {
	UnitFacts(ctx context.Context, unit string) (UnitFacts, error)
}

// UnitFacts runs systemctl show for Environment, ExecStart and
// TimeoutStopUSec of unit.
func (Systemctl) UnitFacts(ctx context.Context, unit string) (UnitFacts, error) {
	if !ValidUnit(unit) {
		return UnitFacts{}, fmt.Errorf("unit %q is not a plain service unit name", unit)
	}
	var out, errOut capped
	cmd := exec.CommandContext(ctx, "systemctl", "show", "--property=Environment", "--property=ExecStart", "--property=TimeoutStopUSec", "--", unit) // #nosec G204 -- a fixed program, fixed arguments and a checked unit name, no shell.
	cmd.Stdout, cmd.Stderr = &out, &errOut
	ownGroup(cmd)
	if err := cmd.Run(); err != nil {
		return UnitFacts{}, fmt.Errorf("systemctl show %s: %v: %s", unit, err, strings.TrimSpace(errOut.String()))
	}
	return parseShow(out.String())
}

// parseShow reads the answer of systemctl show: one key=value per line.
func parseShow(text string) (UnitFacts, error) {
	facts := UnitFacts{Env: map[string]string{}}
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "Environment":
			for _, word := range splitWords(value) {
				if k, v, ok := strings.Cut(word, "="); ok {
					facts.Env[k] = v
				}
			}
		case "ExecStart":
			if _, rest, ok := strings.Cut(value, "argv[]="); ok {
				argv, _, _ := strings.Cut(rest, " ;")
				facts.Args = splitWords(argv)
			}
		case "TimeoutStopUSec":
			d, err := parseSpan(value)
			if err != nil {
				return UnitFacts{}, fmt.Errorf("TimeoutStopUSec %q: %v", value, err)
			}
			facts.StopTimeout = d
		}
	}
	return facts, nil
}

// splitWords splits at spaces outside double quotes and drops the quotes.
func splitWords(text string) []string {
	var words []string
	var cur strings.Builder
	quoted, seen := false, false
	for _, r := range text {
		switch {
		case r == '"':
			quoted, seen = !quoted, true
		case r == ' ' && !quoted:
			if seen {
				words = append(words, cur.String())
			}
			cur.Reset()
			seen = false
		default:
			cur.WriteRune(r)
			seen = true
		}
	}
	if seen {
		words = append(words, cur.String())
	}
	return words
}

var spanPart = regexp.MustCompile(`^([0-9]+)(us|ms|s|min|h|d)$`)

// parseSpan reads a time span as systemctl show prints it, such as
// "5min 30s"; "infinity" and "" are 0.
func parseSpan(text string) (time.Duration, error) {
	if text == "" || text == "infinity" {
		return 0, nil
	}
	units := map[string]time.Duration{"us": time.Microsecond, "ms": time.Millisecond, "s": time.Second, "min": time.Minute,
		"h": time.Hour, "d": 24 * time.Hour}
	var total time.Duration
	for _, part := range strings.Fields(text) {
		m := spanPart.FindStringSubmatch(part)
		if m == nil {
			return 0, errors.New("not a time span")
		}
		n, err := strconv.ParseInt(m[1], 10, 32)
		if err != nil {
			return 0, err
		}
		total += time.Duration(n) * units[m[2]]
	}
	return total, nil
}

// unitRule compares the unit with update.json when the ServiceManager reads
// units. It refuses before anything changes.
func (u *Updater) unitRule(s *session) error {
	reader, ok := u.Service.(UnitReader)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), orDefault(u.RequestTimeout, DefaultRequestTimeout)+10*time.Second)
	defer cancel()
	facts, err := reader.UnitFacts(ctx, s.cfg.Unit)
	if err != nil {
		return fmt.Errorf("%w: the settings of the unit %s are not readable: %v", ErrConfig, s.cfg.Unit, err)
	}
	for _, check := range unitChecks {
		if err := check(s, facts); err != nil {
			return fmt.Errorf("%w: %v; nothing changed", ErrConfig, err)
		}
	}
	return nil
}

// unitChecks are the comparisons of unitRule, in their order.
var unitChecks = []func(*session, UnitFacts) error{dataCheck, drainCheck, stopCheck, portCheck}

// drainOf is the drain time the unit gives serve: PIPESMITH_REGISTRY_DRAIN
// of the unit, else drain_seconds, else the default of serve.
func drainOf(s *session, f UnitFacts) int {
	if v, ok := f.Env["PIPESMITH_REGISTRY_DRAIN"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return s.cfg.DrainSeconds
}

// stopCheck refuses a unit whose stop timeout ends before the drain time of
// serve, so systemd would kill the server during its drain (finding R-15).
func stopCheck(s *session, f UnitFacts) error {
	drain := drainOf(s, f)
	if f.StopTimeout > 0 && f.StopTimeout < time.Duration(drain)*time.Second {
		return fmt.Errorf("the unit %s stops after %s (TimeoutStopSec), before the drain of %d s ends; set TimeoutStopSec above the drain time",
			s.cfg.Unit, f.StopTimeout, drain)
	}
	return nil
}

// drainCheck refuses drain_seconds of update.json that differs from
// PIPESMITH_REGISTRY_DRAIN of the unit (finding A-11).
func drainCheck(s *session, f UnitFacts) error {
	v, ok := f.Env["PIPESMITH_REGISTRY_DRAIN"]
	if !ok || !s.cfg.drainGiven {
		return nil
	}
	if n, err := strconv.Atoi(v); err != nil || n != s.cfg.DrainSeconds {
		return fmt.Errorf("drain_seconds %d of %s and PIPESMITH_REGISTRY_DRAIN=%s of the unit %s differ; set both to the drain time of serve",
			s.cfg.DrainSeconds, s.cfg.Path, v, s.cfg.Unit)
	}
	return nil
}

// dataCheck refuses data_dir of update.json that is not the directory
// PIPESMITH_REGISTRY_DATA of the unit names, after symlinks (finding A-17).
func dataCheck(s *session, f UnitFacts) error {
	v, ok := f.Env["PIPESMITH_REGISTRY_DATA"]
	if !ok {
		return nil
	}
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	if real(v) != real(s.dataDir) {
		return fmt.Errorf("data_dir %s of %s is not PIPESMITH_REGISTRY_DATA=%s of the unit %s; set data_dir to the data directory of the unit",
			s.dataDir, s.cfg.Path, v, s.cfg.Unit)
	}
	return nil
}

// portOf is the port the unit gives serve: --port of ExecStart, else
// PIPESMITH_REGISTRY_PORT, else "".
func portOf(f UnitFacts) string {
	for i, a := range f.Args {
		if v, ok := strings.CutPrefix(a, "--port="); ok {
			return v
		}
		if a == "--port" && i+1 < len(f.Args) {
			return f.Args[i+1]
		}
	}
	return f.Env["PIPESMITH_REGISTRY_PORT"]
}

// portCheck refuses a health wait on another port than the unit gives serve,
// so the wait cannot ask another server (finding R-11).
func portCheck(s *session, f UnitFacts) error {
	want := portOf(f)
	if want == "" {
		return nil
	}
	base, err := url.Parse(s.base)
	if err != nil {
		return err
	}
	if base.Port() != want {
		return fmt.Errorf("the health wait asks %s, port %s, and the unit %s gives serve port %s; set health_address of %s to the address of the unit",
			s.base, base.Port(), s.cfg.Unit, want, s.cfg.Path)
	}
	return nil
}
