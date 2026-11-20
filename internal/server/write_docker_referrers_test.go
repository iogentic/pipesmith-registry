package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// The subject of a manifest and the referrers route of the OCI distribution
// spec v1.1.1 (sections Pushing Manifests with Subject and Listing
// Referrers), W11 of the phase 4 plan.

const (
	ociEmptyType  = "application/vnd.oci.empty.v1+json"
	signatureType = "application/vnd.example.signature"
	sbomType      = "application/vnd.example.sbom"
)

// putBlob puts one blob into demo-docker in a single POST.
func (w *writing) putBlob(t *testing.T, blob []byte) {
	t.Helper()
	r := do(t, w.port("demo-docker"), "POST", "/v2/app/blobs/uploads/"+digestQuery(blob), blob, "basic", asAdmin)
	if r.status != http.StatusCreated {
		t.Fatalf("blob: %d %q", r.status, r.body)
	}
}

// putManifestAt puts a manifest under a reference of app and returns the answer.
func (w *writing) putManifestAt(t *testing.T, reference string, manifest []byte, contentType string) *answer {
	t.Helper()
	r := do(t, w.port("demo-docker"), "PUT", "/v2/app/manifests/"+reference, manifest, "basic", asAdmin, "Content-Type", contentType)
	if r.status != http.StatusCreated {
		t.Fatalf("PUT %s: %d %q", reference, r.status, r.body)
	}
	return r
}

func descriptorJSON(mediaType string, data []byte) string {
	return `{"mediaType": "` + mediaType + `", "size": ` + itoa(len(data)) + `, "digest": "` + sha256Digest(data) + `"}`
}

// artifact is an OCI image manifest with a subject; artifactType "" leaves
// the field out and gives the config the type configType.
func artifact(subject []byte, artifactType, configType string, config, layer []byte, note string) []byte {
	head := `{"schemaVersion": 2, "mediaType": "` + ociManifestType + `", `
	if artifactType != "" {
		head += `"artifactType": "` + artifactType + `", `
	}
	return []byte(head + `"config": ` + descriptorJSON(configType, config) + `, "layers": [` + descriptorJSON(ociEmptyType, layer) +
		`], "subject": ` + descriptorJSON(dockerManifestType, subject) + `, "annotations": {"org.example.note": "` + note + `"}}`)
}

type referrerList struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Manifests     []struct {
		MediaType    string            `json:"mediaType"`
		Digest       string            `json:"digest"`
		Size         int               `json:"size"`
		ArtifactType string            `json:"artifactType"`
		Annotations  map[string]string `json:"annotations"`
	} `json:"manifests"`
}

func referrersOf(t *testing.T, r *answer) referrerList {
	t.Helper()
	if r.status != http.StatusOK || r.header.Get("Content-Type") != ociIndexType {
		t.Fatalf("referrers: %d %v %q", r.status, r.header, r.body)
	}
	var list referrerList
	if err := json.Unmarshal(r.body, &list); err != nil {
		t.Fatalf("%v: %q", err, r.body)
	}
	if list.SchemaVersion != 2 || list.MediaType != ociIndexType || list.Manifests == nil {
		t.Fatalf("not an image index with a list of manifests: %q", r.body)
	}
	return list
}

// referrerSet pushes an image under the tag 1.0 and, before and after it,
// three manifests that name it as their subject plus one that names
// another subject. It returns the image and the referring manifests.
func referrerSet(t *testing.T, w *writing) (image []byte, byType, byConfig, index, other []byte) {
	t.Helper()
	image = w.pushImage(t, "subject")
	empty := []byte("{}")
	signature := []byte("a signature")
	w.putBlob(t, empty)
	w.putBlob(t, signature)
	byType = artifact(image, signatureType, ociEmptyType, empty, empty, "by type")
	byConfig = artifact(image, "", sbomType, signature, empty, "by config")
	w.putManifestAt(t, sha256Digest(byType), byType, ociManifestType) // before its subject, as the spec allows
	w.putManifestAt(t, "1.0", image, dockerManifestType)
	w.putManifestAt(t, sha256Digest(byConfig), byConfig, ociManifestType)
	index = []byte(`{"schemaVersion": 2, "mediaType": "` + ociIndexType + `", "manifests": [` + descriptorJSON(ociManifestType, byType) +
		`], "subject": ` + descriptorJSON(dockerManifestType, image) + `}`)
	w.putManifestAt(t, sha256Digest(index), index, ociIndexType)
	other = artifact([]byte("an image that is not pushed"), signatureType, ociEmptyType, empty, empty, "other")
	w.putManifestAt(t, sha256Digest(other), other, ociManifestType)
	return image, byType, byConfig, index, other
}

// A manifest with a subject answers its digest in OCI-Subject, also when
// the subject is not in the repository; one without a subject answers none.
func TestDockerManifestWithSubjectAnswersOCISubject(t *testing.T) {
	w := startOCI(t, nil)
	image := w.pushImage(t, "subject")
	plain := w.putManifestAt(t, "1.0", image, dockerManifestType)
	if plain.header.Get("Docker-Content-Digest") != sha256Digest(image) || plain.header.Values("OCI-Subject") != nil {
		t.Fatalf("a manifest without subject: %v", plain.header)
	}
	empty := []byte("{}")
	w.putBlob(t, empty)
	ref := artifact(image, signatureType, ociEmptyType, empty, empty, "one")
	r := w.putManifestAt(t, sha256Digest(ref), ref, ociManifestType)
	if r.header.Get("OCI-Subject") != sha256Digest(image) || r.header.Get("Docker-Content-Digest") != sha256Digest(ref) {
		t.Fatalf("a manifest with subject: %v", r.header)
	}
	missing := artifact([]byte("not pushed"), signatureType, ociEmptyType, empty, empty, "two")
	r = w.putManifestAt(t, "sig", missing, ociManifestType)
	if r.header.Get("OCI-Subject") != sha256Digest([]byte("not pushed")) {
		t.Fatalf("a subject that is not there: %v", r.header)
	}
}

// GET <name>/referrers/<digest> lists every manifest of the name whose
// subject is the digest, with its type, digest, size, artifact type and
// annotations, on the Docker port and on the main port.
func TestDockerReferrersListTheReferringManifests(t *testing.T) {
	w := startOCI(t, nil)
	image, byType, byConfig, index, _ := referrerSet(t, w)
	for _, target := range []string{"/v2/app/referrers/", "/repository/demo-docker/v2/app/referrers/"} {
		addr := w.port("demo-docker")
		if target[1] == 'r' {
			addr = w.addr()
		}
		r := do(t, addr, "GET", target+sha256Digest(image), nil, "basic", asAdmin)
		list := referrersOf(t, r)
		if r.header.Values("OCI-Filters-Applied") != nil {
			t.Fatalf("no filter asked: %v", r.header)
		}
		want := map[string][3]string{
			sha256Digest(byType):   {ociManifestType, signatureType, "by type"},
			sha256Digest(byConfig): {ociManifestType, sbomType, "by config"},
			sha256Digest(index):    {ociIndexType, "", ""},
		}
		sizes := map[string]int{sha256Digest(byType): len(byType), sha256Digest(byConfig): len(byConfig), sha256Digest(index): len(index)}
		if len(list.Manifests) != len(want) {
			t.Fatalf("%s: %q", target, r.body)
		}
		var digests []string
		for _, m := range list.Manifests {
			w, ok := want[m.Digest]
			if !ok || m.MediaType != w[0] || m.ArtifactType != w[1] || m.Size != sizes[m.Digest] || m.Annotations["org.example.note"] != w[2] {
				t.Fatalf("%s: %+v in %q", target, m, r.body)
			}
			digests = append(digests, m.Digest)
		}
		if !slices.IsSorted(digests) {
			t.Fatalf("not in digest order: %v", digests)
		}
	}
}

// artifactType keeps the referrers of that type and says so in
// OCI-Filters-Applied.
func TestDockerReferrersFilterByArtifactType(t *testing.T) {
	w := startOCI(t, nil)
	image, _, byConfig, _, _ := referrerSet(t, w)
	r := do(t, w.port("demo-docker"), "GET", "/v2/app/referrers/"+sha256Digest(image)+"?artifactType="+sbomType, nil, "basic", asAdmin)
	list := referrersOf(t, r)
	if r.header.Get("OCI-Filters-Applied") != "artifactType" || len(list.Manifests) != 1 || list.Manifests[0].Digest != sha256Digest(byConfig) {
		t.Fatalf("%v %q", r.header, r.body)
	}
	r = do(t, w.port("demo-docker"), "GET", "/v2/app/referrers/"+sha256Digest(image)+"?artifactType=application/vnd.example.none", nil, "basic", asAdmin)
	if list := referrersOf(t, r); len(list.Manifests) != 0 || r.header.Get("OCI-Filters-Applied") != "artifactType" {
		t.Fatalf("%v %q", r.header, r.body)
	}
}

// A digest nothing refers to, a subject that was never pushed and a name
// that does not exist answer an empty index, never 404; a digest outside
// the grammar is 400 DIGEST_INVALID.
func TestDockerReferrersOfAnUnknownDigestIsAnEmptyIndex(t *testing.T) {
	w := startOCI(t, nil)
	_, _, _, _, other := referrerSet(t, w)
	for _, target := range []string{"/v2/app/referrers/" + sha256Digest([]byte("nothing")), "/v2/nowhere/referrers/" + sha256Digest([]byte("nothing"))} {
		r := do(t, w.port("demo-docker"), "GET", target, nil, "basic", asAdmin)
		if list := referrersOf(t, r); len(list.Manifests) != 0 {
			t.Fatalf("%s: %q", target, r.body)
		}
	}
	r := do(t, w.port("demo-docker"), "GET", "/v2/app/referrers/"+sha256Digest([]byte("an image that is not pushed")), nil, "basic", asAdmin)
	if list := referrersOf(t, r); len(list.Manifests) != 1 || list.Manifests[0].Digest != sha256Digest(other) {
		t.Fatalf("the referrer of a missing subject: %q", r.body)
	}
	bad := do(t, w.port("demo-docker"), "GET", "/v2/app/referrers/sha256:abc", nil, "basic", asAdmin)
	if bad.status != http.StatusBadRequest || errorCodeOf(t, bad.body) != "DIGEST_INVALID" {
		t.Fatalf("a bad digest: %d %q", bad.status, bad.body)
	}
}

// A deleted referrer leaves the list.
func TestDockerReferrersLeaveTheListWhenDeleted(t *testing.T) {
	w := startOCI(t, nil)
	image, byType, byConfig, index, _ := referrerSet(t, w)
	for _, gone := range [][]byte{index, byType} {
		if r := do(t, w.port("demo-docker"), "DELETE", "/v2/app/manifests/"+sha256Digest(gone), nil, "basic", asAdmin); r.status != http.StatusAccepted {
			t.Fatalf("DELETE: %d %q", r.status, r.body)
		}
	}
	r := do(t, w.port("demo-docker"), "GET", "/v2/app/referrers/"+sha256Digest(image), nil, "basic", asAdmin)
	if list := referrersOf(t, r); len(list.Manifests) != 1 || list.Manifests[0].Digest != sha256Digest(byConfig) {
		t.Fatalf("%q", r.body)
	}
}

// The spec defines only GET for the referrers route: any other method is
// 405 UNSUPPORTED with Allow GET, HEAD, like the tags route, and GET still
// answers the list.
func TestDockerReferrersRefuseOtherMethods(t *testing.T) {
	w := startOCI(t, nil)
	image, _, _, _, _ := referrerSet(t, w)
	target := "/v2/app/referrers/" + sha256Digest(image)
	for _, method := range []string{"PUT", "POST", "PATCH", "DELETE"} {
		r := do(t, w.port("demo-docker"), method, target, []byte("{}"), "basic", asAdmin)
		if r.status != http.StatusMethodNotAllowed || errorCodeOf(t, r.body) != "UNSUPPORTED" || r.header.Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d %v %q", method, r.status, r.header, r.body)
		}
	}
	if list := referrersOf(t, do(t, w.port("demo-docker"), "GET", target, nil, "basic", asAdmin)); len(list.Manifests) != 3 {
		t.Fatalf("GET after the refusals: %+v", list)
	}
}
