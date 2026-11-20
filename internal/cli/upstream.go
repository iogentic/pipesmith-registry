package cli

import (
	"errors"
	"os"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// upstream is the value upstream of serve (decision 5.1 of the phase 5
// plan), resolved as writes is: the option, then the environment, then
// config.json.
func (c *command) upstream(cfg *config.Config) (bool, error) {
	on, err := config.UpstreamFrom(os.Getenv, cfg.Upstream)
	if err != nil {
		return false, err
	}
	return on || c.ns.values["upstream"] == true, nil
}

// routingOf is the routing rules of state.sqlite and the rule each
// repository names (ruling P5-4 of the phase 5 session), nil without the
// file. A file without the tables of step 3 holds none.
func routingOf(dataDir string) (*model.Routing, error) {
	st, err := state.Open(dataDir)
	if errors.Is(err, state.ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer st.Close()
	rules, err := st.Routing()
	if err != nil {
		return nil, err
	}
	return &rules, nil
}
