package rest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/index"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// listPage is the number of items of one page of GET /v1/assets and
// GET /v1/components. A variable, so a test can page over a small fixture.
// The old product answers 10 per page; that number is not taken from a
// source here (unverified).
var listPage = 10

// listDigest ties a token to its listing and repository, so a token of one
// listing is refused by another (R6).
func listDigest(kind, repo string) string {
	sum := sha256.Sum256([]byte(kind + "\x00repository=" + repo))
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}

// listed is the repository a listing names: the answer to send instead
// when the query names none (422) or one the subject may not browse or
// that does not exist (404, alike, so a forbidden name tells nothing).
func (env *Env) listed(values url.Values) (repos.Repo, *Answer) {
	name := values.Get("repository")
	if name == "" {
		a, _ := refuse(http.StatusUnprocessableEntity, "the query parameter repository is required")
		return repos.Repo{}, &a
	}
	r, found := env.Repositories.Get(name)
	if !found || !env.mayBrowse(r) {
		a, _ := refuse(http.StatusNotFound, "repository not found")
		return repos.Repo{}, &a
	}
	if env.Catalog == nil {
		a, _ := refuse(http.StatusServiceUnavailable, "the listing is not ready")
		return repos.Repo{}, &a
	}
	return r, nil
}

// listedAsset is an asset of a page with the place of its member in the
// group, 0 for a repository that is no group.
type listedAsset struct {
	key    [2]string
	member int
	asset  index.Asset
}

// Assets is GET /v1/assets?repository=<name>: the assets of a repository a
// page at a time, ordered by directory and file name, the key order of the
// index, so a page reads only the rows after its token (R6). Of a group
// each path is listed once with the asset of the first member that holds
// it, as on the read path (R12); id and downloadUrl name the group,
// repository names the member.
func Assets(env *Env, values url.Values) (Answer, error) {
	r, refusal := env.listed(values)
	if refusal != nil {
		return *refusal, nil
	}
	digest := listDigest("assets", r.Name)
	var after *[2]string
	if token, given := values["continuationToken"]; given {
		key, valid := decodeKey(token[0], digest, 2)
		if !valid {
			return badRequest("the continuationToken does not belong to this listing")
		}
		after = &[2]string{key[0], key[1]}
	}
	var page []listedAsset // sorted by key, at most listPage+1
	add := func(item listedAsset) {
		i := sort.Search(len(page), func(i int) bool { return !less(page[i].key, item.key) })
		if i < len(page) && page[i].key == item.key {
			if item.member < page[i].member {
				page[i] = item
			}
			return
		}
		if len(page) > listPage && i == len(page) {
			return
		}
		page = append(page, listedAsset{})
		copy(page[i+1:], page[i:])
		page[i] = item
		if len(page) > listPage+1 {
			page = page[:listPage+1]
		}
	}
	for m, member := range env.Repositories.Resolve(r.Name) {
		read := 0
		err := env.Catalog.walk(env.Context, member.Name, prefilter{after: after}, env.Index.Lookup, func(a index.Asset) error {
			add(listedAsset{key: splitPath(a.Path), member: m, asset: a})
			if read++; read > listPage {
				return errStop // every later row of this member sorts after the page
			}
			return nil
		})
		if err != nil {
			return Answer{}, err
		}
	}
	out := struct {
		Items             []assetXO `json:"items"`
		ContinuationToken *string   `json:"continuationToken"`
	}{Items: []assetXO{}}
	for i, item := range page {
		if i == listPage {
			token := encodeKey(page[i-1].key[:], digest)
			out.ContinuationToken = &token
			break
		}
		xo, err := env.assetXO(r.Name, r.Format, &item.asset)
		if err != nil {
			return Answer{}, err
		}
		out.Items = append(out.Items, xo)
	}
	return ok(out)
}

// Components is GET /v1/components?repository=<name>: the components of a
// repository a page at a time, in the order and shape of the search, each
// with its assets.
func Components(env *Env, values url.Values) (Answer, error) {
	r, refusal := env.listed(values)
	if refusal != nil {
		return *refusal, nil
	}
	q := &searchQuery{fields: map[string]pattern{}, repository: r.Name, hasRepository: true, timeout: searchTimeout,
		digest: listDigest("components", r.Name)}
	if token, given := values["continuationToken"]; given {
		key, valid := decodeKey(token[0], q.digest, 4)
		if !valid {
			return badRequest("the continuationToken does not belong to this listing")
		}
		q.token = &[4]string{key[0], key[1], key[2], key[3]}
	}
	return env.componentPage(q, listPage)
}

// encodeKey is a token: base64url of the key parts as JSON, a dot and the
// digest of the query.
func encodeKey(parts []string, digest string) string {
	data, _ := json.Marshal(parts) // strings always encode
	return base64.RawURLEncoding.EncodeToString(data) + "." + digest
}

// decodeKey is the key of a token of the query of digest with n parts.
func decodeKey(token, digest string, n int) ([]string, bool) {
	head, tail, found := strings.Cut(token, ".")
	if !found || tail != digest {
		return nil, false
	}
	data, err := base64.RawURLEncoding.DecodeString(head)
	if err != nil {
		return nil, false
	}
	var parts []string
	if err := json.Unmarshal(data, &parts); err != nil || len(parts) != n {
		return nil, false
	}
	return parts, true
}

// ComponentSource gives the asset paths of the components of a repository
// whose coordinates match (catalog.Catalog.ComponentPaths).
type ComponentSource interface {
	ComponentPaths(repo string, match func(namespace, name, version string) bool) ([]string, error)
}

// ComponentByID is GET /v1/components/{id}: the component with the assets
// the read path serves, 422 for an id this server cannot have made, 404 for
// a component no hosted repository holds. The server checks the browse
// permission of the repository of the id first.
func ComponentByID(env *Env, id string) (Answer, error) {
	if _, _, valid := DecodeComponentID(id); !valid {
		return refuse(http.StatusUnprocessableEntity, "the component id is malformed")
	}
	behind, err := env.catalogBehind()
	if err != nil {
		return Answer{}, err
	}
	if behind != "" {
		// R10: no answer from a catalog that lacks rows of the index
		return refuse(http.StatusServiceUnavailable, "catalog.sqlite is behind the index for store "+behind+"; a scan of the store brings it up to date")
	}
	c, err := FindComponent(env, id)
	if err != nil {
		return Answer{}, err
	}
	if c == nil {
		return refuse(http.StatusNotFound, "component not found")
	}
	xo := componentXO{ID: id, Repository: c.Repository, Format: c.Format, Group: optional(c.Group, c.HasGroup), Name: c.Name,
		Version: optional(c.Version, c.HasVersion), Assets: []assetXO{}}
	for _, path := range c.Paths {
		a, err := env.Index.Lookup(c.Repository, path)
		if err != nil {
			return Answer{}, err
		}
		if a == nil {
			continue
		}
		asset, err := env.assetXO(c.Repository, c.Format, a)
		if err != nil {
			return Answer{}, err
		}
		xo.Assets = append(xo.Assets, asset)
	}
	if len(xo.Assets) == 0 {
		return refuse(http.StatusNotFound, "component not found")
	}
	return ok(xo)
}
