package export

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const ociIndexType = "application/vnd.oci.image.index.v1+json"

// Blob is one blob of an image: its digest and size from the manifest, and
// its bytes.
type Blob struct {
	Digest string
	Size   int64
	Open   func() (io.ReadCloser, int64, error)
}

// Image is one image of an OCI image layout: the manifest as stored, its
// media type, the reference it was exported by ("" for a digest, which names
// no image), and the blobs the manifest names, config first.
type Image struct {
	Manifest  []byte
	MediaType string
	Name      string
	Blobs     []Blob
}

// DigestError is a blob whose bytes do not hash to its digest.
type DigestError struct {
	Digest, Got string
}

func (e *DigestError) Error() string {
	return fmt.Sprintf("the blob %s hashes to %s", e.Digest, e.Got)
}

// FullImageName is the name with the registry Docker puts before a name
// that has none: docker.io/, and docker.io/library/ for a name of one part.
func FullImageName(name string) string {
	first, _, nested := strings.Cut(name, "/")
	if nested && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return name
	}
	if nested {
		return "docker.io/" + name
	}
	return "docker.io/library/" + name
}

// WriteOCI writes the image as a tar in OCI image layout: every blob below
// blobs/sha256/, the manifest last of them, then manifest.json for docker
// load on an engine without OCI support, then oci-layout, then
// index.json with one descriptor of the manifest. A named image carries its
// reference in org.opencontainers.image.ref.name and its full name in
// io.containerd.image.name. Each blob is hashed while it is copied; a blob
// that fails its digest or its size ends the archive at once, before
// index.json, and that error is returned.
func WriteOCI(w io.Writer, image Image) error {
	tw := tar.NewWriter(w)
	sum := sha256.Sum256(image.Manifest)
	manifestDigest := "sha256:" + hex.EncodeToString(sum[:])
	for _, b := range image.Blobs {
		if err := writeBlob(tw, b); err != nil {
			return err // tw is left without Close: no end of archive
		}
	}
	if err := writeMember(tw, blobPath(manifestDigest), image.Manifest); err != nil {
		return err
	}
	if err := writeDockerManifest(tw, image); err != nil {
		return err
	}
	if err := writeMember(tw, "oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return err
	}
	descriptor := map[string]any{"mediaType": image.MediaType, "digest": manifestDigest, "size": len(image.Manifest)}
	if image.Name != "" {
		descriptor["annotations"] = map[string]string{
			"io.containerd.image.name":          FullImageName(image.Name),
			"org.opencontainers.image.ref.name": image.Name,
		}
	}
	index, err := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": ociIndexType, "manifests": []any{descriptor}})
	if err != nil {
		return err
	}
	if err := writeMember(tw, "index.json", index); err != nil {
		return err
	}
	return tw.Close()
}

// sha256Digest reports whether digest is "sha256:" and 64 digits of lower
// hex.
func sha256Digest(digest string) bool {
	hexPart, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hexPart) != 64 {
		return false
	}
	for i := 0; i < len(hexPart); i++ {
		if c := hexPart[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func blobPath(digest string) string {
	return "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:")
}

func header(name string, size int64) *tar.Header {
	return &tar.Header{Name: name, Size: size, Mode: 0o644, ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
}

func writeMember(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(header(name, int64(len(data)))); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func writeBlob(tw *tar.Writer, b Blob) error {
	if !sha256Digest(b.Digest) {
		return &DigestError{Digest: b.Digest, Got: "no sha256 digest"}
	}
	in, size, err := b.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	if size != b.Size {
		return &SizeError{Name: b.Digest, Want: b.Size, Read: size}
	}
	if err := tw.WriteHeader(header(blobPath(b.Digest), size)); err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tw, hash), io.LimitReader(in, size))
	if err != nil {
		return err
	}
	if n != size {
		return &SizeError{Name: b.Digest, Want: size, Read: n}
	}
	if got := "sha256:" + hex.EncodeToString(hash.Sum(nil)); got != b.Digest {
		return &DigestError{Digest: b.Digest, Got: got}
	}
	return nil
}
