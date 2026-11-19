package release

import (
	"encoding/json"
	"fmt"
)

// ScheduledSwitch reads scheduled_switch of update.json: true lets
// `update --fetch --scheduled` run the switch stage on the version it
// staged. Without the key it is false.
func (c *UpdateConfig) ScheduledSwitch() (bool, error) {
	raw, ok := c.members["scheduled_switch"]
	if !ok {
		return false, nil
	}
	var on *bool
	if err := json.Unmarshal(raw, &on); err != nil || on == nil {
		return false, fmt.Errorf("%w: %s: scheduled_switch is %s, not true or false", ErrConfig, c.Path, raw)
	}
	return *on, nil
}
