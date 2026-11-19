package export

// The OCI image layout specification, version 1.1, page image-layout.md of
// github.com/opencontainers/image-spec, read on 2026-11-19: an image layout
// holds the file oci-layout with imageLayoutVersion, the file index.json,
// an image index, and the blobs below blobs/<alg>/<encoded>, each matching
// its digest; org.opencontainers.image.ref.name on a descriptor of
// index.json names the reference. Task V2.1 of docs/plans/browse-tools.md
// found that Docker 29.9.3 resolves the loaded image by its name only when
// io.containerd.image.name holds the full name, so the writer adds it.

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func blobOf(data []byte) Blob {
	return Blob{Digest: sha(data), Size: int64(len(data)), Open: fixed(string(data))}
}

func testImage() (Image, map[string][]byte) {
	config := []byte(`{"architecture": "amd64", "os": "linux"}`)
	layers := [][]byte{[]byte("layer one"), []byte("layer two, longer")}
	manifest := []byte(fmt.Sprintf(`{"schemaVersion": 2, "mediaType": %q, "config": {"digest": %q, "size": %d}, "layers": [{"digest": %q, "size": %d}, {"digest": %q, "size": %d}]}`,
		"application/vnd.docker.distribution.manifest.v2+json", sha(config), len(config), sha(layers[0]), len(layers[0]), sha(layers[1]), len(layers[1])))
	want := map[string][]byte{sha(config): config, sha(layers[0]): layers[0], sha(layers[1]): layers[1], sha(manifest): manifest}
	return Image{Manifest: manifest, MediaType: "application/vnd.docker.distribution.manifest.v2+json", Name: "demo/app:1.0",
		Blobs: []Blob{blobOf(config), blobOf(layers[0]), blobOf(layers[1])}}, want
}

type tarRow struct {
	name string
	data []byte
}

func readTar(t *testing.T, data []byte) ([]tarRow, error) {
	t.Helper()
	r := tar.NewReader(bytes.NewReader(data))
	var rows []tarRow
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return rows, err
		}
		content, err := io.ReadAll(r)
		if err != nil {
			return rows, err
		}
		rows = append(rows, tarRow{h.Name, content})
	}
}

func TestWriteOCILayout(t *testing.T) {
	image, want := testImage()
	var out bytes.Buffer
	if err := WriteOCI(&out, image); err != nil {
		t.Fatal(err)
	}
	rows, err := readTar(t, out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, row := range rows {
		names = append(names, row.name)
	}
	manifestDigest := sha(image.Manifest)
	order := []string{"blobs/sha256/" + strings.TrimPrefix(image.Blobs[0].Digest, "sha256:"),
		"blobs/sha256/" + strings.TrimPrefix(image.Blobs[1].Digest, "sha256:"),
		"blobs/sha256/" + strings.TrimPrefix(image.Blobs[2].Digest, "sha256:"),
		"blobs/sha256/" + strings.TrimPrefix(manifestDigest, "sha256:"), "oci-layout", "index.json"}
	if strings.Join(names, " ") != strings.Join(order, " ") {
		t.Fatalf("members %q, want %q", names, order)
	}
	for _, row := range rows[:4] {
		digest := "sha256:" + strings.TrimPrefix(row.name, "blobs/sha256/")
		if sha(row.data) != digest || !bytes.Equal(row.data, want[digest]) {
			t.Errorf("%s does not hold the bytes of its digest", row.name)
		}
	}
	var layout struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	if err := json.Unmarshal(rows[4].data, &layout); err != nil || layout.ImageLayoutVersion != "1.0.0" {
		t.Fatalf("oci-layout %q", rows[4].data)
	}
	var index struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Manifests     []struct {
			MediaType   string            `json:"mediaType"`
			Digest      string            `json:"digest"`
			Size        int64             `json:"size"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(rows[5].data, &index); err != nil {
		t.Fatal(err)
	}
	if index.SchemaVersion != 2 || index.MediaType != "application/vnd.oci.image.index.v1+json" || len(index.Manifests) != 1 {
		t.Fatalf("index.json %s", rows[5].data)
	}
	d := index.Manifests[0]
	if d.Digest != manifestDigest || d.Size != int64(len(image.Manifest)) || d.MediaType != image.MediaType {
		t.Fatalf("descriptor %+v", d)
	}
	if d.Annotations["org.opencontainers.image.ref.name"] != "demo/app:1.0" || d.Annotations["io.containerd.image.name"] != "docker.io/demo/app:1.0" {
		t.Fatalf("annotations %v", d.Annotations)
	}
	var again bytes.Buffer
	if err := WriteOCI(&again, image); err != nil || !bytes.Equal(again.Bytes(), out.Bytes()) {
		t.Fatalf("two runs differ: %v", err)
	}
}

func TestWriteOCIWithoutName(t *testing.T) {
	image, _ := testImage()
	image.Name = ""
	var out bytes.Buffer
	if err := WriteOCI(&out, image); err != nil {
		t.Fatal(err)
	}
	rows, err := readTar(t, out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if last := rows[len(rows)-1]; last.name != "index.json" || bytes.Contains(last.data, []byte("annotations")) {
		t.Fatalf("an export by digest names an image: %s", last.data)
	}
}

func TestWriteOCIRefusesABlobThatDoesNotHashToItsDigest(t *testing.T) {
	image, _ := testImage()
	image.Blobs[2].Open = fixed("layer two, LONGER") // the same size, other bytes
	var out bytes.Buffer
	var mismatch *DigestError
	if err := WriteOCI(&out, image); !errors.As(err, &mismatch) || mismatch.Digest != image.Blobs[2].Digest {
		t.Fatalf("err %v, want a DigestError of the second layer", err)
	}
	rows, _ := readTar(t, out.Bytes())
	for _, row := range rows {
		if row.name == "index.json" || row.name == "oci-layout" {
			t.Fatalf("%s written after a blob that failed its digest", row.name)
		}
	}
	// the positive twin: the same input with the right bytes ends whole
	image, _ = testImage()
	out.Reset()
	if err := WriteOCI(&out, image); err != nil {
		t.Fatal(err)
	}
	rows, err := readTar(t, out.Bytes())
	if err != nil || rows[len(rows)-1].name != "index.json" {
		t.Fatalf("the whole archive: %v %d rows", err, len(rows))
	}
	// a manifest that does not hash to a digest is impossible: the writer takes its digest from the bytes;
	// a blob of another size than its descriptor fails as well
	image.Blobs[0].Open = sized("short", 5)
	out.Reset()
	var short *SizeError
	if err := WriteOCI(&out, image); !errors.As(err, &short) {
		t.Fatalf("err %v, want a SizeError", err)
	}
}

func TestFullImageName(t *testing.T) {
	for name, want := range map[string]string{
		"demo/app:1.0":                       "docker.io/demo/app:1.0",
		"app:latest":                         "docker.io/library/app:latest",
		"registry.example.com/team/app:2":    "registry.example.com/team/app:2",
		"localhost:5000/app:1":               "localhost:5000/app:1",
		"localhost/app:1":                    "localhost/app:1",
		"demo/tools/cli@sha256:0123456789ab": "docker.io/demo/tools/cli@sha256:0123456789ab",
	} {
		if got := FullImageName(name); got != want {
			t.Errorf("FullImageName(%q) = %q, want %q", name, got, want)
		}
	}
}
