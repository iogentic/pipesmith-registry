package release

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// DefaultDrainSeconds is the drain time of serve without the key
// drain_seconds, and MaxDrainSeconds its highest value.
const (
	DefaultDrainSeconds = 5
	MaxDrainSeconds     = 3600
)

var drainNumber = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)

// readDrain reads drain_seconds of update.json: the drain time the unit
// gives serve, a whole number from 1 to MaxDrainSeconds.
func (c *UpdateConfig) readDrain(values map[string]json.RawMessage) error {
	c.DrainSeconds = DefaultDrainSeconds
	raw, ok := values["drain_seconds"]
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(string(raw))
	if !drainNumber.Match(raw) || err != nil || n > MaxDrainSeconds {
		return fmt.Errorf("drain_seconds is %s, not a whole number from 1 to %d", raw, MaxDrainSeconds)
	}
	c.DrainSeconds, c.drainGiven = n, true
	return nil
}

// restartTimeout is the time a restart or a stop may take: the drain time of
// the running server plus DefaultRestartTimeout. Without drain_seconds the
// unit may set any drain time, so the highest one counts.
func (c *UpdateConfig) restartTimeout() time.Duration {
	drain := MaxDrainSeconds
	if c.drainGiven {
		drain = c.DrainSeconds
	}
	return time.Duration(drain)*time.Second + DefaultRestartTimeout
}
