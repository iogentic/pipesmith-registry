package docker

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/pystr"
)

// The manifests a registry takes (OCI distribution spec v1.1.1, section
// Pushing Manifests, read 2026-11-18).

// Reference is one descriptor a manifest names: a blob (config or layer)
// or a child manifest of an index.
type Reference struct {
	Digest   string
	Manifest bool
}

// ManifestError is a manifest this server does not take; Code is the error
// code of the answer.
type ManifestError struct {
	Code    string
	Message string
}

func (e *ManifestError) Error() string { return e.Message }

func invalid(format string, args ...any) *ManifestError {
	return &ManifestError{Code: "MANIFEST_INVALID", Message: fmt.Sprintf(format, args...)}
}

// mediaTypeOf is a Content-Type without parameters, in lower case.
func mediaTypeOf(value string) string {
	first, _, _ := strings.Cut(value, ";")
	return strings.ToLower(strings.TrimSpace(first))
}

// generic are Content-Type values that name no manifest type; the type then
// comes from the document.
var generic = map[string]bool{"": true, "application/json": true, "application/octet-stream": true}

// Manifest checks a pushed manifest: a JSON object of one of the four
// types this server takes (Docker schema 2 image and list, OCI image
// manifest and index), schemaVersion 2, and digests in the descriptors. The
// type comes from the Content-Type of the request, else from the mediaType
// of the document, else from its shape; both given must agree. A Docker
// schema 1 manifest, signed or not, is refused (W11 of the phase 4 plan).
// It returns the media type to store and the descriptors to look up; the
// subject of a manifest is not looked up, as the specification asks.
func Manifest(data []byte, contentType string) (string, []Reference, *ManifestError) {
	header := mediaTypeOf(contentType)
	if header == Schema1 || header == Schema1Signed {
		return "", nil, invalid("Docker schema 1 manifests are not taken; push a schema 2 or OCI manifest")
	}
	value, err := pyjson.Decode(data)
	if err != nil {
		return "", nil, invalid("the manifest is not JSON")
	}
	document, ok := value.(*pyjson.Object)
	if !ok {
		return "", nil, invalid("the manifest is not a JSON object")
	}
	version, _ := get(document, "schemaVersion").(int64)
	if version == 1 {
		return "", nil, invalid("Docker schema 1 manifests are not taken; push a schema 2 or OCI manifest")
	}
	field, hasField := get(document, "mediaType").(string)
	if hasField {
		field = mediaTypeOf(field)
	}
	mediaType := header
	switch {
	case !generic[header] && hasField && field != header:
		return "", nil, invalid("the Content-Type %s differs from the mediaType %s of the manifest", header, field)
	case generic[header] && hasField:
		mediaType = field
	case generic[header]:
		mediaType = shapeOf(document)
	}
	switch mediaType {
	case DockerManifest, DockerManifestList, OCIManifest, OCIIndex:
	default:
		return "", nil, invalid("manifests of type %s are not taken", pystr.Repr(mediaType))
	}
	if version != 2 {
		return "", nil, invalid("a manifest of type %s has schemaVersion 2", mediaType)
	}
	refs, bad := references(document, mediaType == DockerManifestList || mediaType == OCIIndex)
	if bad != nil {
		return "", nil, bad
	}
	return mediaType, refs, nil
}

// shapeOf is the type of a document without a type of its own: an index
// lists manifests, an image has a config.
func shapeOf(document *pyjson.Object) string {
	if _, ok := get(document, "manifests").([]any); ok {
		return OCIIndex
	}
	if config, ok := get(document, "config").(*pyjson.Object); ok {
		if mediaType, _ := get(config, "mediaType").(string); mediaType == DockerConfig {
			return DockerManifest
		}
		return OCIManifest
	}
	return ""
}

// references are the descriptors of the config and the layers of an image,
// or of the manifests of an index, each once. A layer with urls lies
// elsewhere and is not looked up.
func references(document *pyjson.Object, index bool) ([]Reference, *ManifestError) {
	var refs []Reference
	seen := map[string]bool{}
	add := func(item any, manifest bool, what string) *ManifestError {
		descriptor, ok := item.(*pyjson.Object)
		if !ok {
			return invalid("%s is not a descriptor", what)
		}
		digest, _ := get(descriptor, "digest").(string)
		if !IsDigest(digest) {
			return invalid("%s has no digest of the form sha256:<64 hex> or sha512:<128 hex>", what)
		}
		if urls, ok := get(descriptor, "urls").([]any); ok && len(urls) > 0 {
			return nil
		}
		if !seen[digest] {
			seen[digest] = true
			refs = append(refs, Reference{Digest: digest, Manifest: manifest})
		}
		return nil
	}
	if index {
		items, ok := get(document, "manifests").([]any)
		if !ok {
			return nil, invalid("an index lists its manifests")
		}
		for i, item := range items {
			if bad := add(item, true, "manifests["+strconv.Itoa(i)+"]"); bad != nil {
				return nil, bad
			}
		}
		return refs, nil
	}
	config, ok := document.Get("config")
	if !ok {
		return nil, invalid("an image manifest names its config")
	}
	if bad := add(config, false, "config"); bad != nil {
		return nil, bad
	}
	layers, ok := get(document, "layers").([]any)
	if !ok {
		return nil, invalid("an image manifest lists its layers")
	}
	for i, item := range layers {
		if bad := add(item, false, "layers["+strconv.Itoa(i)+"]"); bad != nil {
			return nil, bad
		}
	}
	return refs, nil
}
