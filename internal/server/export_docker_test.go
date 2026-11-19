package server

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/export"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/stores"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

const ociManifestType = "application/vnd.oci.image.manifest.v1+json"

// ociImage is an image manifest stored by digest only, for an index, with
// its config and layers below v2/-/blobs/.
func ociImage(arch string, layers ...[]byte) (map[string]file, []byte) {
	files := map[string]file{}
	config := []byte(fmt.Sprintf(`{"architecture": %q, "os": "linux"}`, arch))
	files["v2/-/blobs/"+digestOf(config)] = file{config, "application/octet-stream"}
	var refs []string
	for _, layer := range layers {
		files["v2/-/blobs/"+digestOf(layer)] = file{layer, "application/octet-stream"}
		refs = append(refs, fmt.Sprintf(`{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "size": %d, "digest": %q}`, len(layer), digestOf(layer)))
	}
	manifest := []byte(fmt.Sprintf(`{"schemaVersion": 2, "mediaType": %q, "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "size": %d, "digest": %q}, "layers": [%s]}`,
		ociManifestType, len(config), digestOf(config), strings.Join(refs, ", ")))
	return files, manifest
}

// ociIndex is an index by tag and digest over amd64 and arm64; stored names
// the platforms whose manifest the repository keeps.
func ociIndex(name, tag string, stored ...string) map[string]file {
	files := map[string]file{}
	var entries []string
	for _, arch := range []string{"amd64", "arm64"} {
		blobs, manifest := ociImage(arch, []byte("layer of "+arch))
		entries = append(entries, fmt.Sprintf(`{"mediaType": %q, "size": %d, "digest": %q, "platform": {"architecture": %q, "os": "linux"}}`,
			ociManifestType, len(manifest), digestOf(manifest), arch))
		if slices.Contains(stored, arch) {
			for p, f := range blobs {
				files[p] = f
			}
			files["v2/"+name+"/manifests/"+digestOf(manifest)] = file{manifest, ociManifestType}
		}
	}
	index := []byte(fmt.Sprintf(`{"schemaVersion": 2, "mediaType": %q, "manifests": [%s]}`, ociIndexType, strings.Join(entries, ", ")))
	files["v2/"+name+"/manifests/"+tag] = file{index, ociIndexType}
	files["v2/"+name+"/manifests/"+digestOf(index)] = file{index, ociIndexType}
	return files
}

// provenanceIndex is an index by tag over one amd64 image and the
// attestation manifest a build with provenance pushes beside it, both
// stored: the platform unknown/unknown and the annotations of BuildKit.
func provenanceIndex(name, tag string) map[string]file {
	files, manifest := ociImage("amd64", []byte("layer of the provenance image"))
	statement := []byte(`{"_type": "https://in-toto.io/Statement/v0.1", "predicateType": "https://slsa.dev/provenance/v0.2"}`)
	config := []byte(`{"architecture": "unknown", "os": "unknown"}`)
	files["v2/-/blobs/"+digestOf(statement)] = file{statement, "application/octet-stream"}
	files["v2/-/blobs/"+digestOf(config)] = file{config, "application/octet-stream"}
	attestation := []byte(fmt.Sprintf(`{"schemaVersion": 2, "mediaType": %q, "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "size": %d, "digest": %q}, "layers": [{"mediaType": "application/vnd.in-toto+json", "size": %d, "digest": %q, "annotations": {"in-toto.io/predicate-type": "https://slsa.dev/provenance/v0.2"}}]}`,
		ociManifestType, len(config), digestOf(config), len(statement), digestOf(statement)))
	files["v2/"+name+"/manifests/"+digestOf(manifest)] = file{manifest, ociManifestType}
	files["v2/"+name+"/manifests/"+digestOf(attestation)] = file{attestation, ociManifestType}
	index := []byte(fmt.Sprintf(`{"schemaVersion": 2, "mediaType": %q, "manifests": [`+
		`{"mediaType": %q, "size": %d, "digest": %q, "platform": {"architecture": "amd64", "os": "linux"}}, `+
		`{"mediaType": %q, "size": %d, "digest": %q, "platform": {"architecture": "unknown", "os": "unknown"}, `+
		`"annotations": {"vnd.docker.reference.digest": %q, "vnd.docker.reference.type": "attestation-manifest"}}]}`,
		ociIndexType, ociManifestType, len(manifest), digestOf(manifest), ociManifestType, len(attestation), digestOf(attestation), digestOf(manifest)))
	files["v2/"+name+"/manifests/"+tag] = file{index, ociIndexType}
	return files
}

func dockerExportBlobs() ([]testkit.BlobSpec, map[string]map[string]file) {
	images := map[string]map[string]file{
		"app":  dockerImage("demo/app", "1.0", []byte("app layer one"), []byte("app layer two")),
		"both": ociIndex("demo/both", "2.0", "amd64", "arm64"),
		"one":  ociIndex("demo/one", "3.0", "arm64"),
		"prov": provenanceIndex("demo/prov", "4.0"),
	}
	var blobs []testkit.BlobSpec
	for _, files := range images {
		blobs = append(blobs, blobsOf("demo-docker", files)...)
	}
	schema1 := []byte(`{"schemaVersion": 1, "name": "demo/old", "tag": "0.1", "fsLayers": [], "signatures": []}`)
	blobs = append(blobs, spec("demo-docker", "v2/demo/old/manifests/0.1", schema1, schema1Type))
	// a layer whose stored bytes do not hash to its digest: the same size, other bytes
	broken := []byte("good layer bytes")
	blobs = append(blobs, blobsOf("demo-docker", dockerImage("demo/broken", "1.0", broken))...)
	for i := range blobs {
		if blobs[i].Path == "v2/-/blobs/"+digestOf(broken) {
			blobs[i].Content = []byte("evil layer bytes")
		}
	}
	return blobs, images
}

type tarEntry struct {
	name string
	data []byte
}

func readTarAnswer(data []byte) ([]tarEntry, error) {
	r := tar.NewReader(bytes.NewReader(data))
	var out []tarEntry
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		content, err := io.ReadAll(r)
		if err != nil {
			return out, err
		}
		out = append(out, tarEntry{h.Name, content})
	}
}

type ociIndexDoc struct {
	Manifests []struct {
		Digest      string            `json:"digest"`
		MediaType   string            `json:"mediaType"`
		Annotations map[string]string `json:"annotations"`
	} `json:"manifests"`
}

// checkLayout reads an OCI tar: every blob under the SHA-256 of its bytes,
// index.json last; it returns the descriptor of index.json.
func checkLayout(t *testing.T, data []byte) (ociIndexDoc, []string) {
	t.Helper()
	entries, err := readTarAnswer(data)
	if err != nil {
		t.Fatalf("not a tar: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.name)
		if hex, ok := strings.CutPrefix(e.name, "blobs/sha256/"); ok && digestOf(e.data) != "sha256:"+hex {
			t.Fatalf("%s does not hold the bytes of its digest", e.name)
		}
	}
	if len(entries) < 3 || names[len(names)-1] != "index.json" || names[len(names)-2] != "oci-layout" {
		t.Fatalf("members %q", names)
	}
	var index ociIndexDoc
	if err := json.Unmarshal(entries[len(entries)-1].data, &index); err != nil || len(index.Manifests) != 1 {
		t.Fatalf("index.json %s: %v", entries[len(entries)-1].data, err)
	}
	return index, names
}

func dockerExportApp(t *testing.T, limits export.Limits) (*exportApp, map[string]map[string]file) {
	t.Helper()
	blobs, images := dockerExportBlobs()
	return startExport(t, exportOptions{blobs: blobs, limits: limits,
		defs: []*pyjson.Object{dockerDef("demo-docker", 14692), def("demo-raw", "raw", "hosted")}}), images
}

func TestExportImageByTagAndDigest(t *testing.T) {
	a, images := dockerExportApp(t, export.Limits{})
	manifest := images["app"]["v2/demo/app/manifests/1.0"].data
	digest := digestOf(manifest)
	for _, c := range []struct{ ref, archive, name string }{
		{"1.0", "demo_app-1.0.tar", "demo/app:1.0"},
		{digest, "demo_app-" + digest[7:19] + ".tar", ""},
	} {
		path := "v2/demo/app/manifests/" + c.ref
		dry := dryOf(t, a.get(exportTarget("demo-docker", "path", path, "dry", "1")))
		if dry["form"] != "oci" || dry["name"] != c.archive || dry["files"] != json.Number("4") || dry["exact"] != true {
			t.Fatalf("%s: dry %v", c.ref, dry)
		}
		got := a.get(exportTarget("demo-docker", "path", path))
		status(t, got, 200)
		if got.header.Get("Content-Type") != "application/x-tar" || got.header.Get("Content-Disposition") != `attachment; filename="`+c.archive+`"` ||
			got.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: headers %v", c.ref, got.header)
		}
		index, names := checkLayout(t, got.body)
		if index.Manifests[0].Digest != digest || index.Manifests[0].MediaType != dockerManifestType {
			t.Fatalf("%s: descriptor %+v", c.ref, index.Manifests[0])
		}
		if index.Manifests[0].Annotations["org.opencontainers.image.ref.name"] != c.name {
			t.Fatalf("%s: annotations %v", c.ref, index.Manifests[0].Annotations)
		}
		var bytesSum int64
		for p, f := range images["app"] {
			if strings.HasPrefix(p, "v2/-/blobs/") {
				bytesSum += int64(len(f.data))
				if !slices.Contains(names, "blobs/sha256/"+strings.TrimPrefix(p, "v2/-/blobs/sha256:")) {
					t.Fatalf("%s: the blob %s is missing", c.ref, p)
				}
			}
		}
		if dry["bytes"] != json.Number(fmt.Sprint(bytesSum+int64(len(manifest)))) {
			t.Fatalf("%s: dry bytes %v, blobs and manifest %d", c.ref, dry["bytes"], bytesSum+int64(len(manifest)))
		}
	}
}

func TestExportImagePlatforms(t *testing.T) {
	a, images := dockerExportApp(t, export.Limits{})
	both := exportTarget("demo-docker", "path", "v2/demo/both/manifests/2.0")
	got := a.get(both)
	if got.status != 400 || fmt.Sprint(got.object(t)["platforms"]) != "[linux/amd64 linux/arm64]" {
		t.Fatalf("an index with two stored platforms: %d %s", got.status, got.body)
	}
	dry := dryOf(t, a.get(exportTarget("demo-docker", "path", "v2/demo/both/manifests/2.0", "platform", "linux/arm64", "dry", "1")))
	if fmt.Sprint(dry["platforms"]) != "[linux/amd64 linux/arm64]" || dry["image"] != "demo/both:2.0" {
		t.Fatalf("dry %v", dry)
	}
	got = a.get(exportTarget("demo-docker", "path", "v2/demo/both/manifests/2.0", "platform", "linux/arm64"))
	status(t, got, 200)
	index, _ := checkLayout(t, got.body)
	_, arm := ociImage("arm64", []byte("layer of arm64"))
	if index.Manifests[0].Digest != digestOf(arm) {
		t.Fatalf("the chosen platform gave %s", index.Manifests[0].Digest)
	}
	status(t, a.get(exportTarget("demo-docker", "path", "v2/demo/both/manifests/2.0", "platform", "linux/s390x")), 400)
	// one stored platform of two: taken without the parameter
	got = a.get(exportTarget("demo-docker", "path", "v2/demo/one/manifests/3.0"))
	status(t, got, 200)
	index, _ = checkLayout(t, got.body)
	if index.Manifests[0].Digest != digestOf(arm) {
		t.Fatalf("the stored platform gave %s", index.Manifests[0].Digest)
	}
	_ = images
}

// An attestation manifest is no platform: an image of one platform that was
// pushed with provenance exports that platform without the parameter, and
// unknown/unknown is no choice. The twin: the stored platform by name.
func TestExportImageLeavesAttestationsOutOfThePlatforms(t *testing.T) {
	a, _ := dockerExportApp(t, export.Limits{})
	_, amd := ociImage("amd64", []byte("layer of the provenance image"))
	dry := dryOf(t, a.get(exportTarget("demo-docker", "path", "v2/demo/prov/manifests/4.0", "dry", "1")))
	if fmt.Sprint(dry["platforms"]) != "[linux/amd64]" {
		t.Fatalf("dry of an image with provenance lists %v, want [linux/amd64]", dry["platforms"])
	}
	got := a.get(exportTarget("demo-docker", "path", "v2/demo/prov/manifests/4.0"))
	status(t, got, 200)
	index, _ := checkLayout(t, got.body)
	if index.Manifests[0].Digest != digestOf(amd) {
		t.Fatalf("the image with provenance gave %s, want the amd64 manifest", index.Manifests[0].Digest)
	}
	refused := a.get(exportTarget("demo-docker", "path", "v2/demo/prov/manifests/4.0", "platform", "unknown/unknown"))
	if refused.status != 400 || fmt.Sprint(refused.object(t)["platforms"]) != "[linux/amd64]" {
		t.Fatalf("platform unknown/unknown: %d %s", refused.status, refused.body)
	}
	got = a.get(exportTarget("demo-docker", "path", "v2/demo/prov/manifests/4.0", "platform", "linux/amd64"))
	status(t, got, 200)
	if index, _ = checkLayout(t, got.body); index.Manifests[0].Digest != digestOf(amd) {
		t.Fatalf("platform linux/amd64 gave %s", index.Manifests[0].Digest)
	}
}

func TestExportImageRefusals(t *testing.T) {
	a, _ := dockerExportApp(t, export.Limits{})
	for _, c := range []struct {
		target string
		status int
	}{
		{exportTarget("demo-docker", "path", "v2/demo/old/manifests/0.1"), 409},
		{exportTarget("demo-docker", "path", "v2/demo/app/manifests/9.9"), 404},
		{exportTarget("demo-docker", "path", "v2/demo/app"), 404},
		{exportTarget("demo-docker"), 404},
		{exportTarget("demo-docker", "path", "v2/demo/app/manifests/1.0", "form", "zip"), 400},
		{exportTarget("demo-raw", "form", "oci"), 400},
	} {
		got := a.get(c.target)
		if got.status != c.status {
			t.Errorf("%s: %d %s, want %d", c.target, got.status, got.body, c.status)
		}
	}
	// the positive twin
	status(t, a.get(exportTarget("demo-docker", "path", "v2/demo/app/manifests/1.0", "form", "oci")), 200)
}

func TestExportImageEndsBeforeIndexOnABadBlob(t *testing.T) {
	a, _ := dockerExportApp(t, export.Limits{})
	got := rawGet(t, a.addr, exportTarget("demo-docker", "path", "v2/demo/broken/manifests/1.0"))
	if got.status != 200 { // the head went out before the blob was hashed
		t.Fatalf("status %d", got.status)
	}
	// the transfer fails: no last chunk, so no client keeps the tar as a whole one
	if bytes.HasSuffix(got.raw, lastChunk) || !errors.Is(got.bodyErr, io.ErrUnexpectedEOF) {
		t.Fatalf("the cut answer: last chunk %v, body error %v", bytes.HasSuffix(got.raw, lastChunk), got.bodyErr)
	}
	entries, _ := readTarAnswer(got.body)
	for _, e := range entries {
		if e.name == "index.json" || e.name == "oci-layout" {
			t.Fatalf("%s written after a blob that failed its digest", e.name)
		}
	}
	// the positive twin: an image whose blobs hash to their digests ends with index.json and the last chunk
	whole := rawGet(t, a.addr, exportTarget("demo-docker", "path", "v2/demo/app/manifests/1.0"))
	if whole.bodyErr != nil || !bytes.HasSuffix(whole.raw, lastChunk) {
		t.Fatalf("the whole image: body error %v", whole.bodyErr)
	}
	checkLayout(t, whole.body)
}

func TestExportImageByteLimitCountsTheBlobs(t *testing.T) {
	_, images := dockerExportApp(t, export.Limits{})
	var total int64
	for p, f := range images["app"] {
		if strings.HasPrefix(p, "v2/-/blobs/") || p == "v2/demo/app/manifests/1.0" {
			total += int64(len(f.data))
		}
	}
	at, _ := dockerExportApp(t, export.Limits{MaxFiles: 4, MaxBytes: total})
	status(t, at.get(exportTarget("demo-docker", "path", "v2/demo/app/manifests/1.0")), 200)
	over, _ := dockerExportApp(t, export.Limits{MaxFiles: 4, MaxBytes: total - 1})
	got := over.get(exportTarget("demo-docker", "path", "v2/demo/app/manifests/1.0"))
	if got.status != 413 || got.object(t)["bytes"] != json.Number(fmt.Sprint(total)) {
		t.Fatalf("one byte above: %d %s", got.status, got.body)
	}
}

// An index whose descriptors name one manifest many times: the handler reads
// each stored manifest once and keeps only the chosen one, so a dry call
// opens the index and one manifest, not one manifest per descriptor.
func TestExportImageReadsEachManifestOfAnIndexOnce(t *testing.T) {
	blobs, manifest := ociImage("amd64", []byte("layer of the wide image"))
	const descriptors = 50
	var entries []string
	for i := 0; i < descriptors; i++ {
		entries = append(entries, fmt.Sprintf(`{"mediaType": %q, "size": %d, "digest": %q, "platform": {"architecture": "arch%02d", "os": "linux"}}`,
			ociManifestType, len(manifest), digestOf(manifest), i))
	}
	index := []byte(fmt.Sprintf(`{"schemaVersion": 2, "mediaType": %q, "manifests": [%s]}`, ociIndexType, strings.Join(entries, ", ")))
	blobs["v2/demo/wide/manifests/"+digestOf(manifest)] = file{manifest, ociManifestType}
	blobs["v2/demo/wide/manifests/1.0"] = file{index, ociIndexType}
	var counted *countingStore
	a := startExport(t, exportOptions{blobs: blobsOf("demo-docker", blobs), defs: []*pyjson.Object{dockerDef("demo-docker", 14696)},
		wrap: func(s stores.Store) stores.Store { counted = &countingStore{Store: s}; return counted }})
	dry := dryOf(t, a.get(exportTarget("demo-docker", "path", "v2/demo/wide/manifests/1.0", "platform", "linux/arch07", "dry", "1")))
	if platforms, _ := dry["platforms"].([]any); len(platforms) != descriptors || dry["files"] != json.Number("3") {
		t.Fatalf("dry %v", dry)
	}
	if opened, _ := counted.stillOpen(); opened != 2 {
		t.Fatalf("the dry call opened %d files, want the index and the manifest once", opened)
	}
	// the positive twin: the export of the chosen platform holds the image
	got := a.get(exportTarget("demo-docker", "path", "v2/demo/wide/manifests/1.0", "platform", "linux/arch07"))
	status(t, got, 200)
	layout, _ := checkLayout(t, got.body)
	if layout.Manifests[0].Digest != digestOf(manifest) {
		t.Fatalf("the export gave %s", layout.Manifests[0].Digest)
	}
}
