package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// The registry calls of a Docker proxy that store nothing (task 12 of the
// phase 5 plan, docker): HEAD of an uncached manifest, and the tag list of
// an image name, which the proxy keeps in memory for the metadata age.

// maxTagListBytes bounds the tag list the remote answers.
const maxTagListBytes = 4 << 20

// The bounds of the tag lists kept in memory, over every Docker proxy of the
// process: at most maxTagLists names and maxTagListTotal bytes of tags. A
// list past them pushes out the one asked longest ago, which the next
// request of its name asks the remote for again.
const (
	maxTagLists     = 4096
	maxTagListTotal = 64 << 20
)

// Peek is the answer of the remote to a HEAD.
type Peek struct {
	// Found is a 200 of the remote; Header holds its headers.
	Found bool
	// Failed is a remote that failed. Neither is set for a 404, another
	// answer below 500, or a proxy that may not ask its remote.
	Failed bool
	Header http.Header
}

// Head asks the remote of a proxy with HEAD for a path and stores nothing.
// The rules of Get hold: a refused or blocked proxy, a path that may not go
// out, a not found mark, a missing secret and auto block send nothing, and a
// request takes a slot of the repository and one of the process. It runs
// outside the flights, as it writes nothing, and ends with ctx.
func (p *Proxies) Head(ctx context.Context, q Query) (Peek, error) {
	settings, err := p.settingsOf(q.Repo)
	if err != nil || settings.Blocked || !Fetches(q.Repo.Format) || !fetchable(q.Repo, q.Path) ||
		marked(p.stateOf(q.Repo.Name, q.Path), settings, p.nowMS()) {
		return Peek{}, nil
	}
	r, client, err := p.remoteOf(q.Repo.Name, q.Repo.Format, settings, q.Limit)
	if err != nil {
		p.warn("proxy %s serves its cache only and asks no remote: %v", q.Repo.Name, err)
		return Peek{}, nil
	}
	if p.needs(q.Repo.Name, r) != "" || p.waiting(r, settings) {
		return Peek{}, nil
	}
	probe, admitted := p.admit(r, settings)
	if !admitted {
		return Peek{}, nil
	}
	v := unjudged
	defer func() { p.report(q.Repo.Name, r, settings, v, probe) }()
	// the slots, as long as the client waits (finding 5-2 of review 5)
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return Peek{}, ctx.Err()
	}
	defer func() { <-r.slots }()
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return Peek{}, ctx.Err()
	}
	defer func() { <-p.slots }()
	header := http.Header{}
	for key, values := range q.Header {
		header[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
	resp, err := client.Do(ctx, http.MethodHead, q.remote(), "", header)
	if err != nil {
		if ctx.Err() != nil {
			return Peek{}, ctx.Err() // the client left
		}
		v = judge(err)
		p.warn("proxy %s: HEAD %q: %q", q.Repo.Name, q.Path, err)
		return Peek{Failed: true}, nil
	}
	defer resp.Body.Close()
	v = answered
	switch status := resp.StatusCode; {
	case status == http.StatusOK:
		return Peek{Found: true, Header: resp.Header.Clone()}, nil
	case status == http.StatusUnauthorized && settings.Auth.Built() && credentialsRefused(resp):
		p.refuse(q.Repo.Name, r)
		return Peek{}, nil
	case status < 500:
		return Peek{}, nil
	}
	v = failure
	p.warn("proxy %s: HEAD %q: the remote answered %d", q.Repo.Name, q.Path, resp.StatusCode)
	return Peek{Failed: true}, nil
}

// tagList is the tag list of one image name of a proxy, kept in memory.
type tagList struct {
	tags    []string
	found   bool      // the remote answered 200; false after its 404
	checked time.Time // the time of that answer
}

// size is what a list holds in memory, about: its tags and a word each.
func (l *tagList) size() int {
	n := 64
	for _, tag := range l.tags {
		n += len(tag) + 16
	}
	return n
}

// keepTags keeps the list of key within the bounds of the tag lists.
func (p *Proxies) keepTags(key tagKey, list *tagList) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if old := p.tagLists[key]; old != nil {
		p.tagBytes -= old.size()
	}
	p.tagLists[key] = list
	p.tagBytes += list.size()
	for len(p.tagLists) > maxTagLists || p.tagBytes > maxTagListTotal && len(p.tagLists) > 1 {
		var oldest tagKey
		var at time.Time
		first := true
		for k, l := range p.tagLists {
			if k != key && (first || l.checked.Before(at)) {
				oldest, at, first = k, l.checked, false
			}
		}
		p.tagBytes -= p.tagLists[oldest].size()
		delete(p.tagLists, oldest)
	}
}

// tagKey names the tag list of an image name of a repository.
type tagKey struct{ repo, name string }

// keptTags is the tag list kept for an image name, nil for none.
func (p *Proxies) keptTags(key tagKey) *tagList {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tagLists[key]
}

// Tags is the tag list of an image name at the remote of a Docker proxy.
// The list is kept in memory per repository and name for the metadata age:
// within it no request goes out, after it the remote is asked again, in one
// flight per name. A failed remote, and a proxy that may not ask, leave the
// last list as it is (decision 5.6); a 404 of the remote is kept as no list
// for the same age. ok is false when no list is kept. A restart and
// Invalidate forget every list. The error is the one of ctx when the
// caller left.
func (p *Proxies) Tags(ctx context.Context, repo repos.Repo, name string) (tags []string, ok bool, err error) {
	key, path := tagKey{repo.Name, name}, "v2/"+name+"/tags/list"
	kept := func() ([]string, bool, error) {
		if list := p.keptTags(key); list != nil && list.found {
			return list.tags, true, nil
		}
		return nil, false, nil
	}
	settings, err := p.settingsOf(repo)
	if err != nil || settings.Blocked || !Fetches(repo.Format) || !fetchable(repo, path) {
		return kept()
	}
	if list := p.keptTags(key); list != nil && !listStale(list.checked, p.now(), settings.MetadataMaxAge) {
		return kept()
	}
	r, client, err := p.remoteOf(repo.Name, repo.Format, settings, repo.WriteSettings().MaxBodyBytes)
	if err != nil {
		p.warn("proxy %s serves its cache only and asks no remote: %v", repo.Name, err)
		return kept()
	}
	if p.needs(repo.Name, r) != "" || p.waiting(r, settings) {
		return kept()
	}
	if _, err := p.flights.do(ctx, flightKey{repo.Name, "\x00tags\x00" + name}, func() result {
		res := p.fetchTags(repo, path, key, r, client, settings)
		if res.err != nil {
			p.warn("proxy %s: %q: %q", repo.Name, path, res.err)
		}
		return res
	}); err != nil {
		return nil, false, err
	}
	return kept()
}

// listStale reports a tag list checked at checked that must be asked for
// again at now, for an age in minutes.
func listStale(checked, now time.Time, age int64) bool {
	switch age {
	case repos.AgeNever:
		return false
	case repos.AgeAlways:
		return true
	}
	return now.Sub(checked) >= time.Duration(age)*time.Minute
}

// fetchTags asks the remote for the tag list of a name and keeps its answer.
func (p *Proxies) fetchTags(repo repos.Repo, path string, key tagKey, r *remote, client *Client, settings repos.ProxySettings) result {
	if list := p.keptTags(key); list != nil && !listStale(list.checked, p.now(), settings.MetadataMaxAge) {
		return result{kind: resultCache} // a flight that ended while this one waited asked
	}
	probe, admitted := p.admit(r, settings)
	if !admitted {
		return result{kind: resultCache}
	}
	v := unjudged
	defer func() { p.report(repo.Name, r, settings, v, probe) }()
	r.slots <- struct{}{}
	defer func() { <-r.slots }()
	p.slots <- struct{}{}
	defer func() { <-p.slots }()
	resp, err := client.Do(context.Background(), http.MethodGet, remotePath(path), "", nil)
	if err != nil {
		v = judge(err)
		return result{kind: resultFailed, err: err}
	}
	defer resp.Body.Close()
	v = answered
	keep := func(list *tagList) { p.keepTags(key, list) }
	switch status := resp.StatusCode; {
	case status == http.StatusOK:
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxTagListBytes+1))
		var answer struct {
			Tags []string `json:"tags"`
		}
		if err != nil || len(data) > maxTagListBytes || json.Unmarshal(data, &answer) != nil {
			v = failure
			return result{kind: resultFailed, err: fmt.Errorf("the tag list of the remote cannot be read")}
		}
		keep(&tagList{tags: answer.Tags, found: true, checked: p.now()})
		return result{kind: resultStored}
	case status == http.StatusNotFound:
		keep(&tagList{checked: p.now()})
		return result{kind: resultMissing}
	case status == http.StatusUnauthorized && settings.Auth.Built() && credentialsRefused(resp):
		p.refuse(repo.Name, r)
		return result{kind: resultMissing}
	case status < 500:
		return result{kind: resultMissing, err: fmt.Errorf("the remote answered %d", status)}
	}
	v = failure
	return result{kind: resultFailed, err: fmt.Errorf("the remote answered %d", resp.StatusCode)}
}
