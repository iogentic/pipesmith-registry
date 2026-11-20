package docker

import (
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/pystr"
)

// The subject of a manifest and the referrers route (OCI distribution spec
// v1.1.1, sections Pushing Manifests with Subject and Listing Referrers,
// read 2026-11-20).

const (
	SubjectHeader = "OCI-Subject"
	FiltersHeader = "OCI-Filters-Applied"
	referrersMark = "/referrers/"
)

// ParseReferrers parses <name>/referrers/<digest>, the path after "v2/". It
// returns nil and no error for another path, and a *PathError for a name
// or a digest outside the grammar.
func ParseReferrers(path string) (*V2Path, error) {
	name, digest, found := cutLast(path, referrersMark)
	if !found || digest == "" || strings.Contains(digest, "/") {
		return nil, nil
	}
	if err := checkName(name); err != nil {
		return nil, err
	}
	if !IsDigest(digest) {
		return nil, &PathError{Code: "DIGEST_INVALID", Message: pystr.Repr(digest) + " is not a digest"}
	}
	return &V2Path{Kind: "referrers", Name: name, Reference: digest}, nil
}

// SubjectOf is the digest the subject descriptor of a manifest names, ""
// for a manifest without one.
func SubjectOf(data []byte) string {
	subject, _ := Referrer(data, "", "")
	return subject
}

// Referrer reads a stored manifest for the referrers list: the digest of its
// subject ("" for none) and its descriptor with mediaType, digest, size,
// artifactType and annotations. The artifactType is the field of the
// manifest, else for an image manifest the mediaType of its config; an
// index without one has none. mediaType is the stored type, used when the
// document names none.
func Referrer(data []byte, mediaType, digest string) (string, *pyjson.Object) {
	value, err := pyjson.Decode(data)
	if err != nil {
		return "", nil
	}
	document, ok := value.(*pyjson.Object)
	if !ok {
		return "", nil
	}
	subject, _ := get(document, "subject").(*pyjson.Object)
	if subject == nil {
		return "", nil
	}
	target, _ := get(subject, "digest").(string)
	if !IsDigest(target) {
		return "", nil
	}
	if field, ok := get(document, "mediaType").(string); ok && field != "" {
		mediaType = field
	}
	descriptor := pyjson.NewObject().Set("mediaType", mediaType).Set("digest", digest).Set("size", int64(len(data)))
	artifactType, _ := get(document, "artifactType").(string)
	if artifactType == "" && mediaType != OCIIndex && mediaType != DockerManifestList {
		if config, ok := get(document, "config").(*pyjson.Object); ok {
			artifactType, _ = get(config, "mediaType").(string)
		}
	}
	if artifactType != "" {
		descriptor.Set("artifactType", artifactType)
	}
	if annotations, ok := get(document, "annotations").(*pyjson.Object); ok && annotations.Len() > 0 {
		descriptor.Set("annotations", annotations)
	}
	return target, descriptor
}
