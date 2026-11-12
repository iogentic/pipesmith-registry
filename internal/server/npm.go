package server

import (
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mirosso25/pipesmith-registry/internal/index"
	"github.com/mirosso25/pipesmith-registry/internal/maven"
	"github.com/mirosso25/pipesmith-registry/internal/npm"
	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/pystr"
	"github.com/mirosso25/pipesmith-registry/internal/repos"
)

// The npm read path of server.py: package documents of a repository or a
// group, documents built from stored tarballs, the endpoints under "-" and
// the npm part of /api/asset.

const (
	npmNotFoundBody = `{"error":"not found"}`
	tgz             = ".tgz"
)

// Bounds of the Python stack in the request thread of the reference
// (recursion limit 1000), measured on 2026-11-12 against its application
// with documents of growing depth, twice with the same result; the cases at
// each bound are in testdata/vectors/server_formats.json. json.loads of a
// stored document reads at most maxDocumentNesting nested arrays and
// objects. copy.deepcopy recurses in Python frames and raises RecursionError
// above a copy height of maxMergeCopy in merge_documents and maxRewriteCopy
// in rewrite_tarballs, which is called one frame higher.
const (
	maxDocumentNesting = 9993
	maxMergeCopy       = 495
	maxRewriteCopy     = 496
)

var errCopyDepth = &pyjson.Error{Class: "RecursionError", Message: "maximum recursion depth exceeded"}

// copyHeight is the recursion depth of copy.deepcopy over a decoded value,
// in levels of two frames: one for a scalar or an empty container, one more
// per level of an object or an array. A key counts as a scalar.
func copyHeight(v any) int {
	height := 0
	switch x := v.(type) {
	case *pyjson.Object:
		for _, value := range x.All() {
			height = max(height, copyHeight(value))
		}
	case []any:
		for _, item := range x {
			height = max(height, copyHeight(item))
		}
	default:
		return 1
	}
	return 1 + height
}

// tooDeep is the kind of the first container, in document order, nested
// deeper than limit, as the scanner of json.loads meets it.
func tooDeep(v any, depth, limit int) (string, bool) {
	var children []any
	kind := ""
	switch x := v.(type) {
	case *pyjson.Object:
		kind = "object"
		for _, value := range x.All() {
			children = append(children, value)
		}
	case []any:
		kind, children = "array", x
	default:
		return "", false
	}
	if depth > limit {
		return kind, true
	}
	for _, child := range children {
		if found, deep := tooDeep(child, depth+1, limit); deep {
			return found, true
		}
	}
	return "", false
}

// mergeCopies raises the RecursionError of merge_documents: it copies the
// first document whole, and of each later document every value of
// versions, dist-tags and time whose key the merged document lacks.
func mergeCopies(documents []*pyjson.Object) error {
	if copyHeight(documents[0]) > maxMergeCopy {
		return errCopyDepth
	}
	have := map[string]map[string]bool{}
	for _, key := range npm.MergedKeys {
		have[key] = map[string]bool{}
		if base, ok := field(documents[0], key).(*pyjson.Object); ok {
			for inner := range base.All() {
				have[key][inner] = true
			}
		}
	}
	for _, document := range documents[1:] {
		for _, key := range npm.MergedKeys {
			extra, ok := field(document, key).(*pyjson.Object)
			if !ok {
				continue
			}
			for inner, value := range extra.All() {
				if have[key][inner] {
					continue
				}
				if copyHeight(value) > maxMergeCopy {
					return errCopyDepth
				}
				have[key][inner] = true
			}
		}
	}
	return nil
}

func field(o *pyjson.Object, key string) any {
	v, _ := o.Get(key)
	return v
}

// npmDash is _npm_dash: "-" and every path under it, where npm puts its own
// endpoints.
func npmDash(t *target) bool {
	return t.path == npm.TarballDirectory || strings.HasPrefix(t.path, npm.TarballDirectory+"/")
}

// npmJSON is _npm_json: a JSON answer of the npm read path.
func npmJSON(w http.ResponseWriter, req *request, status int, data []byte) {
	writeBody(w, req, status, [][2]string{{"Content-Type", jsonType}, {"X-Content-Type-Options", "nosniff"}}, data)
}

// npmNotFound is _npm_not_found: the 404 an npm client expects, JSON
// whatever the method was.
func npmNotFound(w http.ResponseWriter, req *request) {
	npmJSON(w, req, http.StatusNotFound, []byte(npmNotFoundBody))
}

// baseURL is _base_url: the origin of the request, taken from the Host
// header, which the host guard requires.
func baseURL(req *request) string {
	return req.scheme + "://" + latin1Text(req.r.Host)
}

// npmBase is _npm_base: the prefix of every tarball URL of an answer.
func npmBase(req *request, repo string) string {
	return baseURL(req) + repositoryPrefix + quote(repo, "") + "/"
}

// npmShasum is _npm_shasum: the dist.shasum of a version object in lower
// case, "" when it has none.
func npmShasum(entry *pyjson.Object) string {
	dist, ok := field(entry, "dist").(*pyjson.Object)
	if !ok {
		return ""
	}
	if shasum, ok := field(dist, "shasum").(string); ok && shasum != "" {
		return maven.Lower(shasum)
	}
	return ""
}

// readBounded is `handle.read(limit + 1) if size <= limit else b""` of an
// open bytes file, which it closes.
func readBounded(f *os.File, size, limit int64) ([]byte, error) {
	if size > limit {
		f.Close()
		return nil, nil
	}
	return readUpTo(f, limit+1)
}

// npm is _npm: the npm answer of a request; false when the path is served
// as a stored file. It writes nothing when it returns an error.
func (s *Server) npm(w http.ResponseWriter, req *request, st *state, repo repos.Repo, members []repos.Repo, t *target) (bool, error) {
	if npmDash(t) {
		if t.path == npm.TarballDirectory+"/ping" && !t.directory {
			npmJSON(w, req, http.StatusOK, []byte("{}"))
		} else {
			npmNotFound(w, req)
		}
		return true, nil
	}
	if t.directory {
		return false, nil
	}
	call, ok := npm.ParsePath(t.path)
	if !ok {
		return false, nil
	}
	document, err := s.rp.npmMerged(st, members, call)
	if err != nil || document == nil {
		return false, err // no document and no tarball: a stored file may lie at this path
	}
	if document.Len() == 0 {
		npmNotFound(w, req)
		return true, nil
	}
	if copyHeight(document) > maxRewriteCopy {
		return false, errCopyDepth
	}
	if document, err = npm.RewriteTarballs(document, npmBase(req, repo.Name)); err != nil {
		return false, err
	}
	if call.Version != "" {
		if document = npm.VersionObject(document, call.Version); document == nil {
			npmNotFound(w, req)
			return true, nil
		}
	}
	data, err := pyjson.Dumps(document)
	if err != nil {
		return false, err
	}
	// the bytes carry the host of the request, so the ETag covers them and
	// no Last-Modified is sent
	return true, send(w, req, &body{size: int64(len(data)), etag: strongEtag(sha1Hex(data)), contentType: jsonType, data: data}, nil)
}

// chosenVersion is one version a stored document answers: the position of
// the document and the path of its tarball.
type chosenVersion struct {
	version  string
	position int
	path     string
}

// orEmpty is `select_versions(...) or {}`.
func orEmpty(document *pyjson.Object, err error) (*pyjson.Object, error) {
	if err != nil {
		return nil, err
	}
	if document == nil {
		return pyjson.NewObject(), nil
	}
	return document, nil
}

func copyObject(value any) *pyjson.Object {
	out := pyjson.NewObject()
	if o, ok := value.(*pyjson.Object); ok {
		for k, v := range o.All() {
			out.Set(k, v)
		}
	}
	return out
}

// npmMerged is _npm_merged: the document of a package with the versions
// whose tarball lies in a member. A version comes from the first document
// whose tarball is the one served and whose shasum describes those bytes;
// every stored tarball no such version covers builds its own version. An
// empty object says the package has a document but no version with a
// stored tarball, nil that neither a document nor a tarball is there.
func (rp *readPath) npmMerged(st *state, members []repos.Repo, call npm.PackagePath) (*pyjson.Object, error) {
	stored, err := rp.npmDocuments(st, members, call.Name)
	if err != nil {
		return nil, err
	}
	for _, document := range stored {
		document.Set("name", call.Name)
	}
	chosen, err := rp.npmChoose(members, stored)
	if err != nil {
		return nil, err
	}
	skip, picked := map[string]bool{}, map[string]bool{}
	for _, c := range chosen {
		skip[c.path], picked[c.version] = true, true
	}
	tarballs, err := rp.npmTarballs(st, members, call.Name, skip)
	if err != nil {
		return nil, err
	}
	built, err := npm.DocumentFromTarballs(call.Name, tarballs)
	if err != nil {
		return nil, err
	}
	reachable, err := rp.npmReachable(members, built)
	if err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		if built == nil {
			return nil, nil
		}
		return orEmpty(npm.SelectVersions(built, reachable))
	}
	if err := mergeCopies(stored); err != nil {
		return nil, err
	}
	merged := npm.MergeDocuments(stored)
	// every version of the documents stays in the object, so the answer
	// drops the ones without a tarball with their tags and times; keep holds
	// the versions that stay
	versions, times := copyObject(field(merged, "versions")), copyObject(field(merged, "time"))
	var keep []string
	for _, c := range chosen {
		versions.Set(c.version, field(field(stored[c.position], "versions").(*pyjson.Object), c.version))
		keep = append(keep, c.version)
	}
	if built != nil {
		builtVersions := field(built, "versions").(*pyjson.Object)
		builtTimes := field(built, "time").(*pyjson.Object)
		for _, version := range reachable {
			if picked[version] {
				continue
			}
			versions.Set(version, field(builtVersions, version))
			keep = append(keep, version)
			if stamp := field(builtTimes, version); stamp != nil && !times.Has(version) {
				times.Set(version, stamp)
			}
		}
	}
	merged.Set("versions", versions)
	merged.Set("time", times)
	return orEmpty(npm.SelectVersions(merged, keep))
}

// npmReachable is _npm_reachable: the versions of a built document whose
// tarball lies at the path its URL names.
func (rp *readPath) npmReachable(members []repos.Repo, built *pyjson.Object) ([]string, error) {
	if built == nil {
		return nil, nil
	}
	var out []string
	for _, vp := range npm.TarballPaths(built) {
		row, err := rp.npmTarballRow(members, vp.Path)
		if err != nil {
			return nil, err
		}
		if row != nil {
			out = append(out, vp.Version)
		}
	}
	return out, nil
}

// npmChoose is _npm_choose: the version a stored document answers, with the
// document and the tarball path, for every version whose tarball the first
// member that holds it serves. A document whose shasum names other bytes is
// passed over.
func (rp *readPath) npmChoose(members []repos.Repo, documents []*pyjson.Object) ([]chosenVersion, error) {
	var chosen []chosenVersion
	picked := map[string]bool{}
	for position, document := range documents {
		versions, ok := field(document, "versions").(*pyjson.Object)
		if !ok {
			continue
		}
		for _, vp := range npm.TarballPaths(document) {
			entry, isObject := field(versions, vp.Version).(*pyjson.Object)
			if picked[vp.Version] || !isObject {
				continue
			}
			asset, err := rp.npmTarballRow(members, vp.Path)
			if err != nil {
				return nil, err
			}
			if asset == nil {
				continue
			}
			if shasum := npmShasum(entry); asset.SHA1 != nil && *asset.SHA1 != "" && shasum != "" {
				if served := maven.Lower(*asset.SHA1); served != shasum {
					name, _ := field(document, "name").(string)
					logAt(slogWarn, serverLogger, "%s of %s names shasum %s, the served %s/%s has %s; the version comes from elsewhere",
						vp.Version, name, shasum, asset.Repo, vp.Path, served)
					continue
				}
			}
			picked[vp.Version] = true
			chosen = append(chosen, chosenVersion{vp.Version, position, vp.Path})
		}
	}
	return chosen, nil
}

// npmDocuments is _npm_documents: the stored document of the package in
// every member that holds one, in member order.
func (rp *readPath) npmDocuments(st *state, members []repos.Repo, name string) ([]*pyjson.Object, error) {
	var documents []*pyjson.Object
	for _, member := range members {
		asset, f, size, err := rp.open(st, member.Name, name)
		if err != nil {
			return nil, err
		}
		if asset == nil {
			continue
		}
		data, err := readBounded(f, size, maxDocumentBytes)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 || int64(len(data)) > maxDocumentBytes {
			logAt(slogWarn, serverLogger, "%s/%s in store %s has %d bytes, not read as a document", asset.Repo, name, asset.Store, size)
			continue
		}
		value, err := pyjson.Decode(data)
		if err == nil {
			if kind, deep := tooDeep(value, 1, maxDocumentNesting); deep {
				err = &pyjson.Error{Class: "RecursionError",
					Message: "maximum recursion depth exceeded while decoding a JSON " + kind + " from a unicode string"}
			}
		}
		if err != nil {
			if !pyjson.IsValueError(err) && PyClass(err) != "RecursionError" {
				return nil, err
			}
			logAt(slogWarn, serverLogger, "%s/%s in store %s is not a package document: %s", asset.Repo, name, asset.Store, PyText(err))
			continue
		}
		if document, ok := value.(*pyjson.Object); ok {
			documents = append(documents, document)
		}
	}
	return documents, nil
}

// npmTarballs is _npm_tarballs: the stored tarballs of a package, the first
// member of a file name first. A path in skip is left out, so a tarball a
// document already describes is not read. A tarball above maxTarballBytes
// is left out, and the reading stops past maxPackageBytes.
func (rp *readPath) npmTarballs(st *state, members []repos.Repo, name string, skip map[string]bool) ([]npm.Tarball, error) {
	directory := name + "/" + npm.TarballDirectory
	var found []npm.Tarball
	seen := map[string]bool{}
	var total int64
	for _, member := range members {
		cursor := &childCursor{ix: rp.index, repo: member.Name, dir: directory}
		for {
			head, err := cursor.peek()
			if err != nil {
				return nil, err
			}
			if head == nil {
				break
			}
			child := *head
			cursor.pos++
			if child.IsDir || !strings.HasSuffix(child.Name, tgz) || seen[child.Name] {
				continue
			}
			path := directory + "/" + child.Name
			if skip[path] {
				continue
			}
			asset, f, size, err := rp.open(st, member.Name, path)
			if err != nil {
				return nil, err
			}
			if asset == nil {
				continue
			}
			data, err := readBounded(f, size, maxTarballBytes)
			if err != nil {
				return nil, err
			}
			if len(data) == 0 || int64(len(data)) > maxTarballBytes {
				logAt(slogWarn, serverLogger, "%s/%s/%s in store %s has %d bytes, left out of the document",
					asset.Repo, directory, child.Name, asset.Store, size)
				continue
			}
			seen[child.Name] = true
			found = append(found, npm.Tarball{FileName: child.Name, Data: data, CreatedMS: asset.CreatedMS})
			total += int64(len(data))
			if total > maxPackageBytes {
				logAt(slogWarn, serverLogger, "the tarballs of %s/%s pass %d bytes; the document holds the %d read so far",
					member.Name, name, maxPackageBytes, len(found))
				return found, nil
			}
		}
	}
	return found, nil
}

// npmTarballRow is _npm_tarball_row: the row that answers a tarball path,
// from the first member that holds it, as file takes it.
func (rp *readPath) npmTarballRow(members []repos.Repo, path string) (*index.Asset, error) {
	for _, member := range members {
		if err := rp.bindable(member.Name, path); err != nil {
			return nil, err
		}
		found, err := rp.index.Lookup(member.Name, path)
		if err != nil || found != nil {
			return found, err
		}
	}
	return nil, nil
}

// bindable is the UnicodeEncodeError sqlite3 of Python raises when
// Index.lookup binds a repository, parent or name that holds a surrogate. A
// path that is not addressable, or an index without stores, binds nothing.
func (rp *readPath) bindable(repo, path string) error {
	if utf8.ValidString(repo) && utf8.ValidString(path) ||
		path == "" || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return nil
	}
	stats, err := rp.index.StoreStats()
	if err != nil || len(stats) == 0 {
		return err
	}
	parent, name := "", path
	if cut := strings.LastIndexByte(path, '/'); cut >= 0 {
		parent, name = path[:cut], path[cut+1:]
	}
	for _, value := range []string{repo, parent, name} {
		if utf8.ValidString(value) {
			continue
		}
		runes := pystr.Runes(value)
		isSurrogate := func(r rune) bool { return r >= 0xD800 && r <= 0xDFFF }
		start := slices.IndexFunc(runes, isSurrogate)
		end := start
		for end < len(runes) && isSurrogate(runes[end]) {
			end++
		}
		message := fmt.Sprintf("'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed", start, end-1)
		if end-start == 1 {
			message = fmt.Sprintf(`'utf-8' codec can't encode character '\u%04x' in position %d: surrogates not allowed`, runes[start], start)
		}
		return &pyjson.Error{Class: "UnicodeEncodeError", Message: message}
	}
	return nil
}

// npmAsset is _npm_asset: package, version and install command of a
// stored npm path, a document or a tarball; nil for another path.
func npmAsset(path, registry string) *pyjson.Object {
	kind, name, version := "document", "", ""
	if call, ok := npm.ParsePath(path); ok && call.Version == "" {
		name = call.Name
	} else {
		var found bool
		if name, version, found = npmTarball(path); !found {
			return nil
		}
		kind = "tarball"
	}
	install := "npm install " + name
	var versionValue any
	if version != "" {
		install += "@" + version
		versionValue = version
	}
	install += " --registry=" + registry
	return pyjson.NewObject().Set("kind", kind).Set("name", name).Set("version", versionValue).Set("registry", registry).
		Set("install", install)
}

// npmTarball is _npm_tarball: package and version of <name>/-/<file>.tgz;
// the version is empty for another file name.
func npmTarball(path string) (string, string, bool) {
	separator := "/" + npm.TarballDirectory + "/"
	i := strings.LastIndex(path, separator)
	if i < 0 {
		return "", "", false
	}
	head, file := path[:i], path[i+len(separator):]
	if strings.Contains(file, "/") || !strings.HasSuffix(file, tgz) {
		return "", "", false
	}
	name, ok := npm.PackageName(head)
	if !ok {
		return "", "", false
	}
	prefix := npm.Unscoped(name) + "-"
	if stem := strings.TrimSuffix(file, tgz); strings.HasPrefix(stem, prefix) {
		return name, stem[len(prefix):], true
	}
	return name, "", true
}
