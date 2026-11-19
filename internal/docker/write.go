package docker

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"hash"
	"regexp"
	"strconv"
	"strings"
)

// The grammar of the push and delete calls of the registry protocol (OCI
// distribution spec v1.1.1, sections Push and Content Management, read
// 2026-11-18): upload sessions, the Content-Range of a chunk, and the
// manifests a registry takes. Nothing here reads or writes a store.

// Manifest media types besides those of the read path in internal/stores.
const (
	DockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	DockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	DockerConfig       = "application/vnd.docker.container.image.v1+json"
	Schema1            = "application/vnd.docker.distribution.manifest.v1+json"
	OCIManifest        = "application/vnd.oci.image.manifest.v1+json"
	OCIIndex           = "application/vnd.oci.image.index.v1+json"
	UploadUUIDHeader   = "Docker-Upload-UUID"
)

const uploadsMark = "/blobs/uploads"

var (
	uploadID   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	chunkRange = regexp.MustCompile(`^([0-9]+)-([0-9]+)$`)
)

// ParseWrite parses the path after "v2/" of a push or delete. An upload is
// <name>/blobs/uploads/ (Kind "upload", no Reference: a new session) or
// <name>/blobs/uploads/<id> (Reference: the session). Every other path is
// one of ParseV2. A session id outside the form this server gives is no
// error here: no session has it.
func ParseWrite(path string) (*V2Path, error) {
	head, id, found := cutLast(path, uploadsMark+"/")
	if !found && strings.HasSuffix(path, uploadsMark) {
		head, id, found = strings.TrimSuffix(path, uploadsMark), "", true
	}
	if found && !strings.Contains(id, "/") {
		if err := checkName(head); err != nil {
			return nil, err
		}
		return &V2Path{Kind: "upload", Name: head, Reference: id}, nil
	}
	return ParseV2(path)
}

func cutLast(s, sep string) (string, string, bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// IsUploadID is a session id of this server: a lower case UUID.
func IsUploadID(id string) bool {
	return uploadID.MatchString(id)
}

// ParseRange reads the Content-Range of a chunk: "<first>-<last>", inclusive
// on both ends.
func ParseRange(value string) (first, last int64, ok bool) {
	m := chunkRange.FindStringSubmatch(value)
	if m == nil {
		return 0, 0, false
	}
	a, errA := strconv.ParseInt(m[1], 10, 64)
	b, errB := strconv.ParseInt(m[2], 10, 64)
	if errA != nil || errB != nil || b < a {
		return 0, 0, false
	}
	return a, b, true
}

// RangeHeader is the Range of an upload of size bytes: 0-<last byte>, and
// 0-0 for an empty upload, as the distribution registry writes it.
func RangeHeader(size int64) string {
	return "0-" + strconv.FormatInt(max(size-1, 0), 10)
}

// NewDigest is the hash of a digest algorithm, nil for another one.
func NewDigest(digest string) hash.Hash {
	switch algorithm, _, _ := strings.Cut(digest, ":"); algorithm {
	case "sha256":
		return sha256.New()
	case "sha512":
		return sha512.New()
	}
	return nil
}

// DigestParts are the algorithm and the hex of a digest.
func DigestParts(digest string) (algorithm, hexValue string) {
	algorithm, hexValue, _ = strings.Cut(digest, ":")
	return algorithm, hexValue
}

// SumOf is "<algorithm>:<hex>" of a hash.
func SumOf(algorithm string, h hash.Hash) string {
	return algorithm + ":" + hex.EncodeToString(h.Sum(nil))
}
