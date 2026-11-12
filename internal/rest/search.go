package rest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/maven"
	"github.com/mirosso25/pipesmith-registry/internal/npm"
	"github.com/mirosso25/pipesmith-registry/internal/repos"
)

// searchPage is the number of components of one page. A variable, so a
// test can page over a small fixture. The page size of the old product is
// not taken from a source here (unverified).
var searchPage = 50

// searchTimeout bounds one page of the search.
const searchTimeout = 2 * time.Minute

// maxValue bounds the length of one search value.
const maxValue = 1024

// served are the search parameters this server answers. format and
// repository choose the repositories, the others are matched per asset.
var served = map[string]bool{
	"q": true, "repository": true, "format": true, "group": true, "name": true, "version": true, "sha1": true,
	"maven.groupId": true, "maven.artifactId": true, "maven.baseVersion": true, "maven.extension": true, "maven.classifier": true,
	"docker.imageName": true, "docker.imageTag": true, "npm.scope": true, "continuationToken": true,
}

// deferred are parameters of the old product this server cannot answer
// yet: digests it does not hold, npm document fields, Docker layers and
// the sort order. They are refused with 501 instead of matching nothing.
var deferred = map[string]bool{
	"md5": true, "sha256": true, "sha512": true, "npm.author": true, "npm.description": true, "npm.keywords": true, "npm.license": true,
	"docker.layerId": true, "docker.contentDigest": true, "sort": true, "direction": true, "prerelease": true, "timeout": true,
	"gavec": true, "lastBlobUpdated": true,
}

// formatOf names the format a format bound parameter needs.
var formatOf = map[string]string{"maven.": "maven2", "docker.": "docker", "npm.": "npm"}

// coordinates are the component of an asset: group and version are empty
// where the component has none. extension and classifier are those of a
// Maven file.
type coordinates struct {
	group, name, version  string
	hasGroup, hasVersion  bool
	extension, classifier string
}

// componentOf places a path in a component by the rules of its format;
// false for a path that belongs to none: Maven metadata, an npm package
// document, a Docker blob.
func componentOf(format, path string) (coordinates, bool) {
	switch format {
	case "maven2":
		base, suffix := maven.SplitChecksum(path)
		c := maven.CoordinatesOf(base)
		if c == nil {
			return coordinates{}, false
		}
		out := coordinates{group: c.GroupID, hasGroup: true, name: c.ArtifactID, version: c.Version, hasVersion: true, extension: c.Extension}
		if suffix != "" {
			out.extension += "." + suffix
		}
		if c.Classifier != nil {
			out.classifier = *c.Classifier
		}
		return out, true
	case "npm":
		separator := "/" + npm.TarballDirectory + "/"
		i := strings.LastIndex(path, separator)
		if i < 0 {
			return coordinates{}, false
		}
		head, file := path[:i], path[i+len(separator):]
		name, found := npm.PackageName(head)
		if !found || strings.Contains(file, "/") || !strings.HasSuffix(file, ".tgz") {
			return coordinates{}, false
		}
		unscoped := npm.Unscoped(name)
		stem := strings.TrimSuffix(file, ".tgz")
		if !strings.HasPrefix(stem, unscoped+"-") || len(stem) == len(unscoped)+1 {
			return coordinates{}, false
		}
		out := coordinates{name: unscoped, version: stem[len(unscoped)+1:], hasVersion: true}
		if strings.HasPrefix(name, "@") {
			out.group, out.hasGroup = name[1:strings.Index(name, "/")], true
		}
		return out, true
	case "docker":
		rest, found := strings.CutPrefix(path, "v2/")
		if !found {
			return coordinates{}, false
		}
		i := strings.LastIndex(rest, "/manifests/")
		if i <= 0 || strings.HasPrefix(rest, "-/") {
			return coordinates{}, false
		}
		reference := rest[i+len("/manifests/"):]
		if reference == "" || strings.Contains(reference, "/") {
			return coordinates{}, false
		}
		return coordinates{name: rest[:i], version: reference, hasVersion: true}, true
	default:
		dir := "/"
		if i := strings.LastIndex(path, "/"); i >= 0 {
			dir = "/" + path[:i]
		}
		return coordinates{group: dir, hasGroup: true, name: path}, true
	}
}

// pattern is one search value: "*" matches any run of characters, the
// rest literally, without regard to the case of ASCII letters as SQLite's
// LIKE compares.
type pattern struct {
	re   *regexp.Regexp
	like string // the value as a LIKE pattern that matches anywhere in a path
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func likeOf(value string) string {
	var b strings.Builder
	b.WriteByte('%')
	for _, r := range value {
		switch r {
		case '*':
			b.WriteByte('%')
		case '%', '_', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('%')
	return b.String()
}

// newPattern is a whole value pattern; within is true for a term of q,
// which may match a part of a field.
func newPattern(value string, within bool) pattern {
	parts := strings.Split(asciiLower(value), "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	expr := strings.Join(parts, ".*")
	if !within {
		expr = "^" + expr + "$"
	}
	return pattern{re: regexp.MustCompile("(?s)" + expr), like: likeOf(value)}
}

func (p pattern) match(s string) bool { return p.re.MatchString(asciiLower(s)) }

// likes are the LIKE patterns of a value in the path: as given, with the
// dots of a Maven group as slashes, and without the leading slash of a raw
// group.
func (p pattern) likes() []string {
	out := []string{p.like}
	if dotted := strings.ReplaceAll(p.like, ".", "/"); dotted != p.like {
		out = append(out, dotted)
	}
	if trimmed := strings.Replace(p.like, "%/", "%", 1); trimmed != p.like {
		out = append(out, trimmed)
	}
	return out
}

// searchQuery is a parsed search.
type searchQuery struct {
	terms              []pattern // q, each term somewhere in group, name, version or path
	fields             map[string]pattern
	sha1               string
	repository, format string
	needFormat         string // a format bound parameter limits the search to its format
	token              *[4]string
	digest             string // of the query without the token, ties a token to its query
	hasRepository      bool
}

func badRequest(message string) (Answer, error) { return refuse(http.StatusBadRequest, message) }

func parseQuery(q url.Values) (*searchQuery, *Answer) {
	out := &searchQuery{fields: map[string]pattern{}}
	var canonical []string
	for name, values := range q {
		if deferred[name] {
			a := NotImplemented("search", "the search parameter "+name)
			return nil, &a
		}
		if !served[name] {
			a, _ := badRequest("unknown search parameter " + name)
			return nil, &a
		}
		if len(values) != 1 {
			a, _ := badRequest("the search parameter " + name + " is given more than once")
			return nil, &a
		}
		value := values[0]
		if len(value) > maxValue {
			a, _ := badRequest("the search parameter " + name + " is longer than 1024 bytes")
			return nil, &a
		}
		if name != "continuationToken" {
			canonical = append(canonical, name+"="+value)
		}
		for prefix, format := range formatOf {
			if strings.HasPrefix(name, prefix) {
				if out.needFormat != "" && out.needFormat != format {
					out.needFormat = "\x00" // two formats: nothing can match
				} else {
					out.needFormat = format
				}
			}
		}
		switch name {
		case "q":
			for _, term := range strings.Fields(value) {
				out.terms = append(out.terms, newPattern(term, true))
			}
		case "repository":
			out.repository, out.hasRepository = value, true
		case "format":
			out.format = value
		case "sha1":
			out.sha1 = strings.ToLower(value)
		case "npm.scope":
			out.fields[name] = newPattern(strings.TrimPrefix(value, "@"), false)
		case "continuationToken":
		default:
			out.fields[name] = newPattern(value, false)
		}
	}
	sort.Strings(canonical)
	sum := sha256.Sum256([]byte(strings.Join(canonical, "\x00")))
	out.digest = base64.RawURLEncoding.EncodeToString(sum[:8])
	if token, given := q["continuationToken"]; given {
		key, valid := decodeToken(token[0], out.digest)
		if !valid {
			a, _ := badRequest("the continuationToken does not belong to this search")
			return nil, &a
		}
		out.token = &key
	}
	return out, nil
}

func encodeToken(key [4]string, digest string) string {
	data, _ := json.Marshal(key[:]) // four strings always encode
	return base64.RawURLEncoding.EncodeToString(data) + "." + digest
}

func decodeToken(token, digest string) ([4]string, bool) {
	var key [4]string
	head, tail, found := strings.Cut(token, ".")
	if !found || tail != digest {
		return key, false
	}
	data, err := base64.RawURLEncoding.DecodeString(head)
	if err != nil {
		return key, false
	}
	var parts []string
	if err := json.Unmarshal(data, &parts); err != nil || len(parts) != 4 {
		return key, false
	}
	copy(key[:], parts)
	return key, true
}

// fieldOf is the coordinate a field parameter matches.
func fieldOf(name string, c coordinates) (string, bool) {
	switch name {
	case "group", "maven.groupId", "npm.scope":
		return c.group, c.hasGroup
	case "name", "maven.artifactId", "docker.imageName":
		return c.name, true
	case "version", "maven.baseVersion", "docker.imageTag":
		return c.version, c.hasVersion
	case "maven.extension":
		return c.extension, true
	case "maven.classifier":
		return c.classifier, c.classifier != ""
	}
	return "", false
}

func (q *searchQuery) matches(c coordinates, path string) bool {
	for name, p := range q.fields {
		value, has := fieldOf(name, c)
		if !has || !p.match(value) {
			return false
		}
	}
	for _, term := range q.terms {
		if !term.match(c.group) && !term.match(c.name) && !term.match(c.version) && !term.match(path) {
			return false
		}
	}
	return true
}

// prefilter is the part of the query SQL can check on the path. The
// extension and classifier of a Maven file are left to matches.
func (q *searchQuery) prefilter() prefilter {
	pre := prefilter{sha1: q.sha1}
	for name, p := range q.fields {
		if name != "maven.extension" && name != "maven.classifier" {
			pre.groups = append(pre.groups, p.likes())
		}
	}
	for _, term := range q.terms {
		pre.groups = append(pre.groups, term.likes())
	}
	return pre
}

// componentXO is ComponentXO.
type componentXO struct {
	ID         string    `json:"id"`
	Repository string    `json:"repository"`
	Format     string    `json:"format"`
	Group      *string   `json:"group"`
	Name       string    `json:"name"`
	Version    *string   `json:"version"`
	Assets     []assetXO `json:"assets"`
}

type pageXO struct {
	Items             []componentXO `json:"items"`
	ContinuationToken *string       `json:"continuationToken"`
}

// hit is a component found by the search, with its key of the order.
type hit struct {
	key [4]string // repository, group, name, version
	xo  componentXO
}

func keyLess(a, b [4]string) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// window keeps the smallest keys after the token, at most limit of them.
// Once it is full its largest key only falls, so a key it drops never
// comes back with another asset.
type window struct {
	limit int
	hits  []*hit
}

func (w *window) full() bool { return len(w.hits) >= w.limit }

func (w *window) add(key [4]string, build func() componentXO, asset assetXO) {
	i := sort.Search(len(w.hits), func(i int) bool { return !keyLess(w.hits[i].key, key) })
	if i < len(w.hits) && w.hits[i].key == key {
		w.hits[i].xo.Assets = append(w.hits[i].xo.Assets, asset)
		return
	}
	if w.full() && i == len(w.hits) {
		return
	}
	h := &hit{key: key, xo: build()}
	h.xo.Assets = []assetXO{asset}
	w.hits = append(w.hits, nil)
	copy(w.hits[i+1:], w.hits[i:])
	w.hits[i] = h
	if len(w.hits) > w.limit {
		w.hits = w.hits[:w.limit]
	}
}

func optional(value string, has bool) *string {
	if !has {
		return nil
	}
	return &value
}

// componentID is the id of a component: base64url of the repository and
// the first 128 bits of the SHA-256 of its coordinates.
func componentID(key [4]string) string {
	sum := sha256.Sum256([]byte(strings.Join(key[1:], "\x00")))
	return base64.RawURLEncoding.EncodeToString(append([]byte(key[0]+":"), sum[:16]...))
}

// searched are the repositories of a search with the format their assets
// are read in: the members of a named group under the permission of the
// group, else every repository that is no group and that the subject may
// browse. They are sorted by name, the first part of the order.
func (env *Env) searched(q *searchQuery) []repos.Repo {
	var leaves []repos.Repo
	if q.hasRepository {
		r, found := env.Repositories.Get(q.repository)
		if !found || !env.mayBrowse(r) {
			return nil
		}
		leaves = env.Repositories.Resolve(r.Name)
	} else {
		for _, r := range env.Repositories.All() {
			if r.Type != "group" && env.mayBrowse(r) {
				leaves = append(leaves, r)
			}
		}
	}
	var out []repos.Repo
	for _, r := range leaves {
		if (q.format == "" || r.Format == q.format) && (q.needFormat == "" || r.Format == q.needFormat) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Search is GET /v1/search: the components that match, ordered by
// repository, group, name and version, a page at a time. A component
// lists the assets that match. The parameters are matched per asset: q
// splits into terms of which each must occur in group, name, version or
// path; the other values must equal their field, "*" standing for any run
// of characters, without regard to the case of ASCII letters.
func Search(env *Env, values url.Values) (Answer, error) {
	q, refusal := parseQuery(values)
	if refusal != nil {
		return *refusal, nil
	}
	if env.Catalog == nil {
		return refuse(http.StatusServiceUnavailable, "the search is not ready")
	}
	ctx, cancel := context.WithTimeout(env.Context, searchTimeout)
	defer cancel()
	w := &window{limit: searchPage + 1}
	pre := q.prefilter()
	for _, r := range env.searched(q) {
		if q.token != nil && r.Name < q.token[0] {
			continue
		}
		if w.full() && w.hits[len(w.hits)-1].key[0] < r.Name {
			break // every key of this and later repositories sorts after the page
		}
		format := r.Format
		err := env.Catalog.walk(ctx, r.Name, pre, env.Index.Lookup, func(a index.Asset) error {
			c, found := componentOf(format, a.Path)
			if !found || !q.matches(c, a.Path) {
				return nil
			}
			key := [4]string{r.Name, c.group, c.name, c.version}
			if q.token != nil && !keyLess(*q.token, key) {
				return nil
			}
			asset := env.assetXO(r.Name, format, &a)
			w.add(key, func() componentXO {
				return componentXO{ID: componentID(key), Repository: r.Name, Format: format, Group: optional(c.group, c.hasGroup),
					Name: c.name, Version: optional(c.version, c.hasVersion)}
			}, asset)
			return nil
		})
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && env.Context.Err() == nil {
				return refuse(http.StatusServiceUnavailable, "the search took longer than "+searchTimeout.String())
			}
			return Answer{}, err
		}
	}
	page := pageXO{Items: []componentXO{}}
	for i, h := range w.hits {
		if i == searchPage {
			token := encodeToken(w.hits[i-1].key, q.digest)
			page.ContinuationToken = &token
			break
		}
		sort.Slice(h.xo.Assets, func(a, b int) bool { return h.xo.Assets[a].Path < h.xo.Assets[b].Path })
		page.Items = append(page.Items, h.xo)
	}
	return ok(page)
}
