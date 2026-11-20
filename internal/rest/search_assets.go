package rest

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"

	"github.com/iogentic/pipesmith-registry/internal/index"
)

// assetHit is an asset found by the asset search with its key of the
// order: repository, directory, file name.
type assetHit struct {
	key   [3]string
	asset assetXO
}

func key3Less(a, b [3]string) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// assetPage is up to size+1 assets that match q, after its token, in the
// order of their keys; the answer instead when the search cannot run.
func (env *Env) assetPage(q *searchQuery, size int) ([]assetHit, *Answer, error) {
	if env.Catalog == nil {
		a, _ := refuse(http.StatusServiceUnavailable, "the search is not ready")
		return nil, &a, nil
	}
	ctx, cancel := env.searchContext(q.timeout)
	defer cancel()
	var hits []assetHit
	pre := q.prefilter()
	for _, r := range env.searched(q) {
		if q.assetToken != nil && r.Name < q.assetToken[0] {
			continue
		}
		if len(hits) > size && hits[len(hits)-1].key[0] < r.Name {
			break // every key of this and later repositories sorts after the page
		}
		walkPre := pre
		if q.assetToken != nil && r.Name == q.assetToken[0] {
			walkPre.after = &[2]string{q.assetToken[1], q.assetToken[2]}
		}
		format, taken := r.Format, 0
		err := env.Catalog.walk(ctx, r.Name, walkPre, env.Index.Lookup, func(a index.Asset) error {
			c, found := componentOf(format, a.Path)
			if !found || !q.matches(c, a.Path) {
				return nil
			}
			split := splitPath(a.Path)
			key := [3]string{r.Name, split[0], split[1]}
			xo, err := env.assetXO(r.Name, format, &a)
			if err != nil {
				return err
			}
			i := sort.Search(len(hits), func(i int) bool { return !key3Less(hits[i].key, key) })
			hits = append(hits, assetHit{})
			copy(hits[i+1:], hits[i:])
			hits[i] = assetHit{key: key, asset: xo}
			if len(hits) > size+1 {
				hits = hits[:size+1]
			}
			if taken++; taken > size {
				return errStop // the walk gives keys in order: the rest sorts after the page
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && env.Context.Err() == nil {
				a, _ := refuse(http.StatusServiceUnavailable, "the search took longer than "+q.timeout.String())
				return nil, &a, nil
			}
			return nil, nil, err
		}
	}
	return hits, nil, nil
}

// SearchAssets is GET /v1/search/assets: the assets that match, with the
// parameters and refusals of Search, ordered by repository, directory and
// file name, a page at a time. An asset that belongs to no component, such
// as Maven metadata, matches no search, as in Search.
func SearchAssets(env *Env, values url.Values) (Answer, error) {
	q, refusal := parseQuery(values, assetSearch)
	if refusal != nil {
		return *refusal, nil
	}
	hits, refusal, err := env.assetPage(q, searchPage)
	if err != nil || refusal != nil {
		if refusal != nil {
			return *refusal, nil
		}
		return Answer{}, err
	}
	page := struct {
		Items             []assetXO `json:"items"`
		ContinuationToken *string   `json:"continuationToken"`
	}{Items: []assetXO{}}
	for i, h := range hits {
		if i == searchPage {
			token := encodeKey(hits[i-1].key[:], q.digest)
			page.ContinuationToken = &token
			break
		}
		page.Items = append(page.Items, h.asset)
	}
	return ok(page)
}

// SearchAssetsDownload is GET /v1/search/assets/download: 302 to the
// downloadUrl of the one asset that matches, 400 when several match, 404
// when none does. A continuationToken has no place here (400).
func SearchAssetsDownload(env *Env, values url.Values) (Answer, error) {
	if _, given := values["continuationToken"]; given {
		return badRequest("the download search takes no continuationToken")
	}
	q, refusal := parseQuery(values, assetSearch)
	if refusal != nil {
		return *refusal, nil
	}
	hits, refusal, err := env.assetPage(q, 1)
	if err != nil || refusal != nil {
		if refusal != nil {
			return *refusal, nil
		}
		return Answer{}, err
	}
	switch len(hits) {
	case 0:
		return refuse(http.StatusNotFound, "no asset matches the search")
	case 1:
		return Answer{Status: http.StatusFound, Location: hits[0].asset.DownloadURL}, nil
	}
	return badRequest("more than one asset matches the search")
}
