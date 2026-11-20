// Package proxy holds what a proxy repository needs to ask its remote
// (phase 5). This leg builds the outbound client: one http.Client per
// repository from the typed settings of internal/repos, with the limits of
// decision 5.11 and of section Hardening of docs/plans/phases.md. Leg 5b
// adds the fetch, freshness, the not found cache and the states of a proxy.
package proxy

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrPath is the refusal of a request path or query that cannot go to the
// remote: it would leave the base path, or it cannot be read.
var ErrPath = errors.New("the request path cannot go to the remote")

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// Join is the address of a request to the remote (decision 5.11): the remote
// address of the repository plus the request path, and never anything else.
// path is the escaped path of the request below the repository, with or
// without its leading slash; query is the escaped query to send, empty for
// none.
//
// Each segment of path is decoded and escaped again on its own, so an
// encoded slash stays encoded inside its segment. A segment that is . or ..,
// or that decodes to text holding such a part between slashes or
// backslashes, an empty segment other than the last one, an escape that does
// not decode, and a query with a fragment or a control character are
// refused with ErrPath. Scheme, host and port are those of remote, which the
// path cannot change, and the query of remote comes before the query of the
// request. remote is not changed.
func Join(remote *url.URL, path, query string) (*url.URL, error) {
	if strings.Contains(query, "#") || strings.ContainsFunc(query, isControl) {
		return nil, fmt.Errorf("%w: the query holds a fragment or a control character", ErrPath)
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	escaped := make([]string, len(segments))
	for i, segment := range segments {
		if segment == "" && i != len(segments)-1 {
			return nil, fmt.Errorf("%w: an empty segment", ErrPath)
		}
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return nil, fmt.Errorf("%w: a segment does not decode", ErrPath)
		}
		for _, part := range strings.FieldsFunc(decoded, func(r rune) bool { return r == '/' || r == '\\' }) {
			if part == "." || part == ".." {
				return nil, fmt.Errorf("%w: a segment . or ..", ErrPath)
			}
		}
		escaped[i] = url.PathEscape(decoded)
	}
	out := *remote
	out.User, out.Fragment, out.RawFragment, out.ForceQuery = nil, "", "", false
	out.RawPath = strings.TrimSuffix(remote.EscapedPath(), "/") + "/" + strings.Join(escaped, "/")
	decoded, err := url.PathUnescape(out.RawPath)
	if err != nil {
		return nil, fmt.Errorf("%w: the joined path does not decode", ErrPath)
	}
	out.Path = decoded
	if out.EscapedPath() != out.RawPath {
		return nil, fmt.Errorf("%w: the joined path has no stable form", ErrPath)
	}
	switch {
	case remote.RawQuery != "" && query != "":
		out.RawQuery = remote.RawQuery + "&" + query
	case query != "":
		out.RawQuery = query
	}
	return &out, nil
}
