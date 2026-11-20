package proxy

import (
	"fmt"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// Status is the state of a proxy (decision 5.2 of the phase 5 plan). Every
// status but Ready serves the cache only.
type Status string

const (
	// Ready asks the remote.
	Ready Status = "ready"
	// Blocked is the attribute blocked of the repository.
	Blocked Status = "blocked"
	// AutoBlocked waits after failures of the remote (decision 5.3).
	AutoBlocked Status = "auto blocked"
	// NeedsSecret waits for the secret of its sign in (decisions 5.12 and
	// 5.13).
	NeedsSecret Status = "needs secret"
	// Refused has settings the typed reader refuses.
	Refused Status = "settings refused"
)

// State is the state of a proxy, for the pages of phase 6.
type State struct {
	Status Status
	// NextProbe is, while AutoBlocked, the time from which the next request
	// probes the remote; zero otherwise.
	NextProbe time.Time
	// Reason says why the proxy is not ready, empty while it is. It names
	// repository and key of a refused setting, never its value, and never a
	// secret.
	Reason string
}

// State is the state of a proxy now.
func (p *Proxies) State(repo repos.Repo) State {
	settings, err := repo.ProxySettings()
	switch {
	case err != nil:
		return State{Status: Refused, Reason: err.Error()}
	case settings.Blocked:
		return State{Status: Blocked, Reason: "the attribute blocked of the repository is on"}
	}
	r, _, err := p.remoteOf(repo.Name, repo.Format, settings, repo.WriteSettings().MaxBodyBytes)
	if err != nil {
		return State{Status: Refused, Reason: err.Error()}
	}
	if need := p.needs(repo.Name, r); need != "" {
		return State{Status: NeedsSecret, Reason: need}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if settings.AutoBlock && !r.block.until.IsZero() {
		return State{Status: AutoBlocked, NextProbe: r.block.until,
			Reason: fmt.Sprintf("the remote failed %d times in a row", r.block.failures)}
	}
	return State{Status: Ready}
}

func (p *Proxies) info(format string, args ...any) {
	p.logger().Info(fmt.Sprintf(format, args...), "logger", logger)
}
