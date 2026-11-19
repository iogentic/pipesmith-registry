package channel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/release"
)

// The list route of the forge: GET {server}/api/v1/packages/{owner} with
// type, q, page (1-based) and limit; the header Link names rel="next" while
// a page follows (Forgejo swagger 17.0.2, operation listPackages, and
// services/context/api.go SetLinkHeader, read 2026-11-18).
const (
	pageSize  = 50
	maxPages  = 100
	listBytes = 1 << 20
)

type entry struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (s *stage) apiURL(path string) string {
	u := *s.set.Server
	u.Path += path
	return u.String()
}

func (s *stage) fileURL(version, file string) string {
	return s.apiURL("/api/packages/" + url.PathEscape(s.set.Owner) + "/generic/" + Package + "/v" + version + "/" + file)
}

// list reads every page and returns the versions of the package, highest
// first, each once.
func (f *Fetcher) list(s *stage) ([]release.Version, error) {
	seen := map[release.Version]bool{}
	var out []release.Version
	pages := 0
	for page := 1; ; page++ {
		if page > maxPages {
			return nil, fmt.Errorf("%w: more than %d pages at %s", ErrList, maxPages, s.set.Server.Redacted())
		}
		q := url.Values{"type": {"generic"}, "q": {Package}, "page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(pageSize)}}
		body, header, err := s.c.fetch(s.apiURL("/api/v1/packages/"+url.PathEscape(s.set.Owner))+"?"+q.Encode(), listBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: page %d: %w", ErrList, page, err)
		}
		pages++
		var entries []entry
		if err := json.Unmarshal(body, &entries); err != nil {
			return nil, fmt.Errorf("%w: page %d is no JSON list of packages", ErrList, page)
		}
		for _, e := range entries {
			if e.Type != "generic" || e.Name != Package {
				continue
			}
			text, ok := strings.CutPrefix(e.Version, "v")
			if !ok {
				continue
			}
			v, err := release.ParseVersion(text)
			if err != nil || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
		if len(entries) == 0 || !hasNext(header) {
			break
		}
	}
	slices.SortFunc(out, func(a, b release.Version) int { return b.Compare(a) })
	f.step("listed: %d versions of %s at %s on %d pages", len(out), Package, s.set.Server.Redacted(), pages)
	return out, nil
}

// hasNext reports whether a Link header names rel="next".
func hasNext(h http.Header) bool {
	for _, value := range h.Values("Link") {
		for _, link := range strings.Split(value, ",") {
			for _, param := range strings.Split(link, ";")[1:] {
				if strings.EqualFold(strings.ReplaceAll(strings.TrimSpace(param), " ", ""), `rel="next"`) {
					return true
				}
			}
		}
	}
	return false
}

// complete reports whether cosign.pub of version name answers 200. Its body
// is never read.
func (f *Fetcher) complete(s *stage, name string) (bool, error) {
	status, err := s.c.status(s.fileURL(name, release.PublicKeyFile))
	if err != nil {
		return false, fmt.Errorf("%w: %s of v%s: %w", ErrList, release.PublicKeyFile, name, err)
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("%w: %s of v%s answered %d", ErrList, release.PublicKeyFile, name, status)
}
