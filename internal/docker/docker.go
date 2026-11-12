// Package docker holds the grammar of the Docker registry read path: request
// paths, digests, tag lists, error bodies and the digests a manifest names.
//
// The rules follow the OCI distribution spec v1.1.1. A name may contain
// slashes, so a request splits at its last "/manifests/", "/blobs/" or
// "/tags/list". Nothing here reads a store.
package docker

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"hash"
	"regexp"
	"slices"
	"strings"

	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/pystr"
)

const (
	APIVersionHeader = "Docker-Distribution-API-Version"
	APIVersion       = "registry/2.0"
	DigestHeader     = "Docker-Content-Digest"
	Schema1Signed    = "application/vnd.docker.distribution.manifest.v1+prettyjws"
	MaxNameLength    = 255
	MaxManifestBytes = 4 * 1024 * 1024
)

const component = `[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*`

// The patterns are anchored at both ends, as re.fullmatch. They hold only
// ASCII, so matching the bytes of a Go string equals matching a Python str.
var (
	namePattern   = regexp.MustCompile(`^` + component + `(?:/` + component + `)*$`)
	tagPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestPattern = regexp.MustCompile(`^(?:sha256:[a-f0-9]{64}|sha512:[a-f0-9]{128})$`)
)

const (
	manifestsMark = "/manifests/"
	blobsMark     = "/blobs/"
	tagsMark      = "/tags/list"
)

// PathError is V2PathError: a name or reference outside the grammar. Code is
// the error code of the answer. It counts as a ValueError.
type PathError struct {
	Code    string
	Message string
}

func (e *PathError) Error() string { return e.Message }

// Unwrap makes a PathError a ValueError for errors.As and
// pyjson.IsValueError, as V2PathError subclasses ValueError.
func (e *PathError) Unwrap() error {
	return &pyjson.Error{Class: "ValueError", Message: e.Message}
}

// V2Path is one registry request. Kind is "base", "manifest", "blob" or
// "tags".
type V2Path struct {
	Kind      string
	Name      string
	Reference string
}

// ParseV2 parses the path after "v2/". It returns nil and no error for a
// path that is no registry call, and a *PathError for a name, tag or digest
// outside the grammar.
func ParseV2(path string) (*V2Path, error) {
	if path == "" {
		return &V2Path{Kind: "base"}, nil
	}
	tags := -1
	if strings.HasSuffix(path, tagsMark) {
		tags = len(path) - len(tagsMark)
	}
	manifests := strings.LastIndex(path, manifestsMark)
	blobs := strings.LastIndex(path, blobsMark)
	last := max(tags, manifests, blobs)
	if last < 0 {
		return nil, nil
	}
	name := path[:last]
	if err := checkName(name); err != nil {
		return nil, err
	}
	switch last {
	case tags:
		return &V2Path{Kind: "tags", Name: name}, nil
	case manifests:
		reference := path[last+len(manifestsMark):]
		if !IsDigest(reference) && !tagPattern.MatchString(reference) {
			return nil, &PathError{Code: "MANIFEST_INVALID", Message: pystr.Repr(reference) + " is neither a tag nor a digest"}
		}
		return &V2Path{Kind: "manifest", Name: name, Reference: reference}, nil
	}
	reference := path[last+len(blobsMark):]
	if !IsDigest(reference) {
		return nil, &PathError{Code: "DIGEST_INVALID", Message: pystr.Repr(reference) + " is not a digest"}
	}
	return &V2Path{Kind: "blob", Name: name, Reference: reference}, nil
}

func checkName(name string) error {
	// A name that matches is ASCII, so its length in bytes is its length in
	// code points, as Python counts it.
	if name == "" || len(name) > MaxNameLength || !namePattern.MatchString(name) {
		return &PathError{Code: "NAME_INVALID", Message: pystr.Repr(name) + " is not a repository name"}
	}
	return nil
}

// IsDigest is true for sha256:<64 hex> and sha512:<128 hex>, lower case.
func IsDigest(text string) bool {
	return digestPattern.MatchString(text)
}

// DigestOf is "sha256:<hex>" of data.
func DigestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// DigestOfAlgorithm is digest_of(data, algorithm) for sha256 and sha512.
func DigestOfAlgorithm(data []byte, algorithm string) (string, error) {
	var h hash.Hash
	switch algorithm {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return "", &pyjson.Error{Class: "ValueError", Message: "unsupported hash type " + algorithm}
	}
	h.Write(data)
	return algorithm + ":" + hex.EncodeToString(h.Sum(nil)), nil
}

// TagNames returns the tags among the children of manifests/: digests and
// other names left out, each once, in code point order. It is never nil.
func TagNames(names []string) []string {
	tags := []string{}
	for _, name := range names {
		if tagPattern.MatchString(name) {
			tags = append(tags, name)
		}
	}
	slices.Sort(tags) // tags are ASCII: byte order is code point order
	return slices.Compact(tags)
}

// ErrorBody is the error document of the spec with one error.
func ErrorBody(code, message string) []byte {
	doc := pyjson.NewObject().Set("errors", []any{pyjson.NewObject().Set("code", code).Set("message", message)})
	body, err := pyjson.Dumps(doc)
	if err != nil {
		panic(err) // strings and objects always encode
	}
	return body
}

func get(o *pyjson.Object, key string) any {
	v, _ := o.Get(key)
	return v
}

// ReferencedDigests returns the config, layers and child manifests a
// manifest or an index names, each once, in the order of Python: config,
// layers, manifests, fsLayers (Docker schema 1). The error is a ValueError
// for a document that is not a JSON object. It is never nil without error.
func ReferencedDigests(manifest []byte) ([]string, error) {
	value, err := pyjson.Decode(manifest)
	if err != nil {
		return nil, &pyjson.Error{Class: "ValueError", Message: "the manifest is not JSON: " + err.Error()}
	}
	document, ok := value.(*pyjson.Object)
	if !ok {
		return nil, &pyjson.Error{Class: "ValueError", Message: "the manifest is not a JSON object"}
	}
	found := []string{}
	add := func(v any) {
		if s, ok := v.(string); ok && IsDigest(s) && !slices.Contains(found, s) {
			found = append(found, s)
		}
	}
	if config, ok := get(document, "config").(*pyjson.Object); ok {
		add(get(config, "digest"))
	}
	for _, pair := range [][2]string{{"layers", "digest"}, {"manifests", "digest"}, {"fsLayers", "blobSum"}} {
		items, ok := get(document, pair[0]).([]any)
		if !ok {
			continue
		}
		for _, item := range items {
			if obj, ok := item.(*pyjson.Object); ok {
				add(get(obj, pair[1]))
			}
		}
	}
	return found, nil
}

// IsSchema1Signed reports a Docker schema 1 signed manifest: by its stored
// type (nil for none), else by a document with schemaVersion 1, an integer,
// and a list of signatures.
func IsSchema1Signed(data []byte, contentType *string) bool {
	if contentType != nil {
		first, _, _ := strings.Cut(*contentType, ";")
		if pystr.Strip(first) == Schema1Signed {
			return true
		}
	}
	value, err := pyjson.Decode(data)
	if err != nil {
		return false
	}
	document, ok := value.(*pyjson.Object)
	if !ok {
		return false
	}
	version, isInt := get(document, "schemaVersion").(int64)
	_, isList := get(document, "signatures").([]any)
	return isInt && version == 1 && isList
}
