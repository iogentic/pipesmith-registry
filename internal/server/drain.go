package server

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"
)

// DrainVariable names the drain time of serve in whole seconds, from 1 to
// MaxDrainSeconds: the time open answers get once the stop began. Unset or
// empty means shutdownGrace.
const DrainVariable = "PIPESMITH_REGISTRY_DRAIN"

// MaxDrainSeconds is the highest drain time.
const MaxDrainSeconds = 3600

var drainText = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)

// drainTime reads DrainVariable.
func drainTime() (time.Duration, error) {
	text := os.Getenv(DrainVariable)
	if text == "" {
		return shutdownGrace, nil
	}
	n, err := strconv.Atoi(text)
	if !drainText.MatchString(text) || err != nil || n > MaxDrainSeconds {
		return 0, fmt.Errorf("%s is %q; give whole seconds from 1 to %d, or leave it unset for %d", DrainVariable, text,
			MaxDrainSeconds, int(shutdownGrace/time.Second))
	}
	return time.Duration(n) * time.Second, nil
}
