package server

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// UpdateTimer is the timer unit of the scheduled update (release/).
const UpdateTimer = "pipesmith-registry-update.timer"

// timerState is what systemctl show tells of the timer unit.
type timerState struct {
	Readable bool   `json:"readable"`
	Unit     string `json:"unit"`
	Active   string `json:"active"`  // ActiveState: active, inactive, ...
	Enabled  string `json:"enabled"` // UnitFileState: enabled, disabled, ...; empty without a unit file
	Next     string `json:"next"`    // RFC 3339 in UTC, empty when none is planned
	Last     string `json:"last"`
	Message  string `json:"message"`
}

// timerShow runs systemctl show for the timer; tests replace it.
type timerShow func(ctx context.Context, unit string) (string, error)

func systemctlShow(ctx context.Context, unit string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// #nosec G204 -- a fixed program with the fixed timer unit name.
	out, err := exec.CommandContext(ctx, "systemctl", "show", unit, "--timestamp=utc",
		"--property=LoadState,ActiveState,UnitFileState,NextElapseUSecRealtime,LastTriggerUSec").Output()
	return string(out), err
}

// readTimer reads the state of the timer; a host without systemctl, or a
// sandbox that refuses it, is not readable, with the reason.
func readTimer(ctx context.Context, show timerShow) timerState {
	st := timerState{Unit: UpdateTimer}
	if show == nil {
		show = systemctlShow
	}
	out, err := show(ctx, UpdateTimer)
	if err != nil {
		st.Message = "systemctl show " + UpdateTimer + " did not answer: " + err.Error()
		return st
	}
	values := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[k] = v
		}
	}
	if values["LoadState"] == "" {
		st.Message = "systemctl show " + UpdateTimer + " named no LoadState."
		return st
	}
	st.Readable = true
	if values["LoadState"] == "not-found" {
		st.Message = UpdateTimer + " is not installed on this host."
		st.Active = values["ActiveState"]
		return st
	}
	st.Active, st.Enabled = values["ActiveState"], values["UnitFileState"]
	st.Next, st.Last = systemdTime(values["NextElapseUSecRealtime"]), systemdTime(values["LastTriggerUSec"])
	return st
}

// systemdTime turns "Sat 2026-11-21 14:15:00 UTC" into RFC 3339; empty, n/a
// and anything else are "".
func systemdTime(text string) string {
	fields := strings.Fields(text)
	if len(fields) != 4 || fields[3] != "UTC" {
		return ""
	}
	t, err := time.Parse("2006-01-02 15:04:05", fields[1]+" "+fields[2])
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
