package server

import (
	"crypto/md5"  // #nosec G501 -- the .md5 checksum of Maven metadata, not a security use.
	"crypto/sha1" // #nosec G505 -- the .sha1 checksum and the ETag form of the old product, not a security use.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"hash"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/maven"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// The Maven parts of the read path: helpers of package maven, the asset
// record of coordinates and the checksums of merged metadata.

func lastSegment(path string) string {
	return path[strings.LastIndex(path, "/")+1:]
}

// coordinatesObject is asdict(coordinates) of the asset answer.
func coordinatesObject(c *maven.Coordinates) *pyjson.Object {
	var classifier any
	if c.Classifier != nil {
		classifier = *c.Classifier
	}
	return pyjson.NewObject().Set("group_id", c.GroupID).Set("artifact_id", c.ArtifactID).Set("version", c.Version).
		Set("classifier", classifier).Set("extension", c.Extension)
}

// hexDigest is hashlib.new(name, data).hexdigest() for a checksum suffix of
// maven.ChecksumSuffixes.
func hexDigest(name string, data []byte) string {
	var h hash.Hash
	switch name {
	case "md5":
		h = md5.New() // #nosec G401 -- the .md5 checksum file of Maven.
	case "sha1":
		h = sha1.New() // #nosec G401 -- the .sha1 checksum file of Maven.
	case "sha256":
		h = sha256.New()
	default:
		h = sha512.New()
	}
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// mergedEtag is _merged_etag: the ETag of merged group metadata. Maven
// Resolver reads the SHA1 from this form and skips the .sha1 request.
func mergedEtag(sha1 string) string {
	return `"{SHA1{` + sha1 + `}}"`
}
