package proxy

import (
	"net/http"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// Freshness (decisions 5.4, 5.5 and 5.6 of the phase 5 plan). A cached
// copy is fresh while less than its age has passed since its last check: the
// time of the row in proxy_state, else the creation time of its blob. -1
// never asks again, 0 asks on every request. A row checked before the last
// bulk reset of its repository is stale whatever the age says, and so is a
// copy without a row whose blob was made before it (decision 5.17).

// stale reports whether a cached copy created at createdMS must be checked
// at the remote at nowMS, for an age in minutes.
func stale(st catalog.ProxyState, createdMS, nowMS, age int64) bool {
	last := createdMS
	if st.Row != nil {
		if st.Row.CheckedMS == nil {
			return true
		}
		last = *st.Row.CheckedMS
	}
	if st.ResetMS != nil && last < *st.ResetMS {
		return true
	}
	switch age {
	case repos.AgeNever:
		return false
	case repos.AgeAlways:
		return true
	}
	return nowMS-last >= age*60_000
}

// validators are the headers of a conditional request for a stale copy:
// the entity tag and the last modified value the remote sent last.
func validators(row *catalog.ProxyRow) http.Header {
	header := http.Header{}
	if row == nil {
		return header
	}
	if row.ETag != "" {
		header.Set("If-None-Match", row.ETag)
	}
	if row.LastModified != "" {
		header.Set("If-Modified-Since", row.LastModified)
	}
	return header
}

func (p *Proxies) nowMS() int64 { return p.now().UnixMilli() }

// stateOf is the row of a path and the time of the last reset of its
// repository. The table is side data: without it, or when it cannot be read,
// every copy counts by its creation time and nothing is remembered.
func (p *Proxies) stateOf(repo, path string) catalog.ProxyState {
	if p.o.State == nil {
		return catalog.ProxyState{}
	}
	st, err := p.o.State.ProxyState(repo, path)
	if err != nil {
		p.warn("proxy %s: %q: the row of proxy_state is not read: %q", repo, path, err)
		return catalog.ProxyState{}
	}
	return st
}

// putRow writes the row of a path; a row that is not written is logged and
// fails nothing.
func (p *Proxies) putRow(row catalog.ProxyRow) {
	if p.o.State == nil {
		return
	}
	if err := p.o.State.PutProxyRow(row); err != nil {
		p.warn("proxy %s: %q: the row of proxy_state is not written: %q", row.Repo, row.Path, err)
	}
}

// checked is the row of a path the remote answered for now, with the
// validators of the answer; a validator the answer lacks is taken from the
// row before, as a 304 may leave it out.
func checked(repo, path string, before *catalog.ProxyRow, nowMS int64, answer http.Header) catalog.ProxyRow {
	row := catalog.ProxyRow{Repo: repo, Path: path, CheckedMS: &nowMS}
	if before != nil {
		row.ETag, row.LastModified = before.ETag, before.LastModified
	}
	if etag := answer.Get("ETag"); etag != "" {
		row.ETag = etag
	}
	if modified := answer.Get("Last-Modified"); modified != "" {
		row.LastModified = modified
	}
	return row
}
