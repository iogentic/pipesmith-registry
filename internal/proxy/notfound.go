package proxy

import (
	"errors"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// The not found cache (section Settings of the phase 5 plan): a 404 of the
// remote for a path the cache lacks is remembered in proxy_state for the
// time to live of the proxy, and a miss within that time answers 404 with no
// request. A 404 for a cached path, a failed remote, a timeout and every
// other status are not remembered. With the not found cache off every miss
// goes to the remote.

// marked reports whether a 404 of the remote holds for the path at nowMS.
func marked(st catalog.ProxyState, settings repos.ProxySettings, nowMS int64) bool {
	return settings.NotFoundCache && st.Row != nil && st.Row.NotFoundUntilMS != nil && nowMS < *st.Row.NotFoundUntilMS
}

// remember writes the not found mark of a path that the remote answered 404.
func (p *Proxies) remember(repo, path string, settings repos.ProxySettings) {
	if !settings.NotFoundCache {
		return
	}
	now := p.nowMS()
	until := now + settings.NotFoundTTL*60_000
	p.putRow(catalog.ProxyRow{Repo: repo, Path: path, CheckedMS: &now, NotFoundUntilMS: &until})
}

// errNoState is the refusal of Invalidate without the table proxy_state.
var errNoState = errors.New("the proxies keep no state: catalog.sqlite is not open")

// Invalidate clears the not found marks of a proxy repository and makes
// every cached path of it stale (decision 5.17 of the phase 5 plan): the
// next request of each path asks the remote, a cached copy with its stored
// validators; the tag lists of a Docker proxy are forgotten. The REST
// operation that calls it is leg 5c.
func (p *Proxies) Invalidate(repo string) error {
	if p.o.State == nil {
		return errNoState
	}
	p.mu.Lock()
	for key, list := range p.tagLists {
		if key.repo == repo {
			p.tagBytes -= list.size()
			delete(p.tagLists, key)
		}
	}
	p.mu.Unlock()
	return p.o.State.ResetProxyState(repo, p.nowMS())
}
