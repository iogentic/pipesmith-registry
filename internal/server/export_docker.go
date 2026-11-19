package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/docker"
	"github.com/iogentic/pipesmith-registry/internal/export"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/stores"
)

// The form oci: one image of a Docker repository as an OCI image layout
// (docs/plans/browse-tools.md, call B22). path names a manifest file,
// v2/<image>/manifests/<tag or digest>. An index resolves to one stored
// platform, chosen by the parameter platform. dockerread.go does not
// change; the manifest and the blobs are read through rp.open as the
// registry read path reads them, and every blob is hashed while it is
// written.

// descriptor is one entry of a manifest or an index.
type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform"`
	Annotations map[string]string `json:"annotations"`
}

// manifestDoc holds the fields of a manifest or an index the export reads.
type manifestDoc struct {
	SchemaVersion int          `json:"schemaVersion"`
	Config        *descriptor  `json:"config"`
	Layers        []descriptor `json:"layers"`
	Manifests     []descriptor `json:"manifests"`
}

// platform is os/arch of a descriptor of an index, with its variant, or ""
// for none. An attestation manifest that BuildKit pushes beside an image
// (annotation vnd.docker.reference.type, platform unknown/unknown) names no
// platform: it holds no image to load.
func (d descriptor) platform() string {
	if d.Platform == nil || d.Annotations["vnd.docker.reference.type"] == "attestation-manifest" ||
		d.Platform.OS == "unknown" && d.Platform.Architecture == "unknown" {
		return ""
	}
	p := d.Platform.OS + "/" + d.Platform.Architecture
	if d.Platform.Variant != "" {
		p += "/" + d.Platform.Variant
	}
	return p
}

// storedManifest is a manifest read from a member, with its media type.
type storedManifest struct {
	data      []byte
	mediaType string
}

// readManifest is the manifest of image under ref, as the registry read
// path answers it: the first member whose bytes answer the reference; by
// digest only when they hash to it. Nil when no member has it.
func (s *Server) readManifest(st *state, members []repos.Repo, image, ref string) (*storedManifest, error) {
	path := "v2/" + image + "/manifests/" + ref
	for _, member := range members {
		asset, f, size, err := s.rp.open(st, member.Name, path)
		if err != nil {
			return nil, err
		}
		if asset == nil {
			continue
		}
		if size > docker.MaxManifestBytes {
			f.Close()
			continue
		}
		data, err := readUpTo(f, docker.MaxManifestBytes+1)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 || docker.IsDigest(ref) && docker.DigestOf(data) != ref {
			continue
		}
		mediaType := stores.ManifestMediaType(data)
		if headerSafe(asset.ContentType) {
			mediaType = *asset.ContentType
		}
		return &storedManifest{data: data, mediaType: mediaType}, nil
	}
	return nil, nil
}

// manifestPath splits v2/<image>/manifests/<reference>.
func manifestPath(path string) (image, ref string, ok bool) {
	rest, found := strings.CutPrefix(path, "v2/")
	if !found {
		return "", "", false
	}
	cut := strings.LastIndex(rest, "/manifests/")
	if cut <= 0 {
		return "", "", false
	}
	image, ref = rest[:cut], rest[cut+len("/manifests/"):]
	return image, ref, ref != "" && !strings.Contains(ref, "/") && image != "-"
}

// exportImage answers the form oci.
func (s *Server) exportImage(w *recorder, req *request, er *exportRequest) error {
	st, repo := er.st, er.repo
	members := dockerMembers(st.repositories, repo.Name)
	image, ref, ok := manifestPath(er.folder)
	if !ok {
		return exportRefusal(w, req, http.StatusNotFound,
			fmt.Sprintf("the form oci exports one image: path names a manifest, v2/<image>/manifests/<tag or digest>, not %q", er.folder))
	}
	top, err := s.readManifest(st, members, image, ref)
	if err != nil {
		return err
	}
	if top == nil {
		return exportRefusal(w, req, http.StatusNotFound, fmt.Sprintf("manifest %s of %s is not stored in %s", ref, image, repo.Name))
	}
	var doc manifestDoc
	if err := json.Unmarshal(top.data, &doc); err != nil {
		return exportRefusal(w, req, http.StatusConflict, "the manifest is no JSON document: "+err.Error(), [2]any{"path", er.folder})
	}
	if doc.SchemaVersion == 1 {
		return exportRefusal(w, req, http.StatusConflict, "a schema 1 manifest has no OCI image layout", [2]any{"path", er.folder})
	}
	chosen := top
	var platforms []any
	if doc.Manifests != nil {
		// each stored manifest is read once, however many descriptors name
		// it, and its bytes are kept only where it may be the chosen one: a
		// manifest of the platform asked for (the last descriptor of it
		// wins), or without the parameter the first stored one, taken when
		// it is the only stored platform
		want, _ := req.params().get("platform")
		wanted := map[string]bool{}
		for _, d := range doc.Manifests {
			if want != "" && d.platform() == want {
				wanted[d.Digest] = true
			}
		}
		read := map[string]*storedManifest{} // nil: not stored
		found := 0
		chosen = nil
		for _, d := range doc.Manifests {
			p := d.platform()
			if p == "" {
				continue
			}
			m, known := read[d.Digest]
			if !known {
				var err error
				if m, err = s.readManifest(st, members, image, d.Digest); err != nil {
					return err
				}
				if m != nil && !wanted[d.Digest] && !(want == "" && found == 0) {
					m = &storedManifest{} // stored; its bytes are not kept
				}
				read[d.Digest] = m
			}
			if m == nil {
				continue
			}
			found++
			platforms = append(platforms, p)
			if want != "" && p == want || want == "" && found == 1 {
				chosen = m
			}
		}
		if want == "" && found != 1 {
			chosen = nil
		}
		if found == 0 {
			return exportRefusal(w, req, http.StatusNotFound, fmt.Sprintf("%s names no platform whose manifest %s stores", er.folder, repo.Name))
		}
		if chosen == nil {
			message := "the index holds several stored platforms; choose one with the parameter platform"
			if want != "" {
				message = fmt.Sprintf("the platform %s is not stored for this index", want)
			}
			return exportRefusal(w, req, http.StatusBadRequest, message, [2]any{"platforms", platforms})
		}
		doc = manifestDoc{}
		if err := json.Unmarshal(chosen.data, &doc); err != nil || doc.Config == nil {
			return exportRefusal(w, req, http.StatusConflict, "the manifest of the platform is no image manifest", [2]any{"path", er.folder})
		}
	}
	if doc.Config == nil {
		return exportRefusal(w, req, http.StatusConflict, "the manifest names no config", [2]any{"path", er.folder})
	}
	limits := s.limits()
	tally := export.NewTally(limits)
	tally.Add(int64(len(chosen.data)))
	var blobs []export.Blob
	for _, d := range append([]descriptor{*doc.Config}, doc.Layers...) {
		if !docker.IsDigest(d.Digest) || !strings.HasPrefix(d.Digest, "sha256:") {
			return exportRefusal(w, req, http.StatusConflict, fmt.Sprintf("the blob %q has no sha256 digest", d.Digest), [2]any{"path", er.folder})
		}
		blobPath := dockerBlobPrefix + d.Digest
		holder := ""
		for _, m := range members {
			a, err := s.rp.index.Lookup(m.Name, blobPath)
			if err != nil {
				return err
			}
			if a != nil {
				holder = m.Name
				break
			}
		}
		if holder == "" {
			return exportRefusal(w, req, http.StatusConflict, fmt.Sprintf("the blob %s of the image is not stored in %s", d.Digest, repo.Name),
				[2]any{"path", blobPath})
		}
		if !tally.Add(d.Size) {
			break
		}
		member := holder
		blobs = append(blobs, export.Blob{Digest: d.Digest, Size: d.Size, Open: func() (io.ReadCloser, int64, error) {
			if err := req.r.Context().Err(); err != nil {
				return nil, 0, fmt.Errorf("the client left: %w", err)
			}
			asset, f, size, err := s.rp.open(st, member, blobPath)
			if err != nil {
				return nil, 0, err
			}
			if asset == nil {
				return nil, 0, fmt.Errorf("%s is gone from %s", blobPath, member)
			}
			return f, size, nil
		}})
	}
	if err := tally.Err(); err != nil {
		over := err.(*export.LimitError)
		return exportRefusal(w, req, http.StatusRequestEntityTooLarge, over.Error(),
			[2]any{"files", over.Files}, [2]any{"bytes", over.Bytes}, [2]any{"max_files", over.MaxFiles},
			[2]any{"max_bytes", over.MaxBytes}, [2]any{"complete", over.Complete})
	}
	shown, name := image+":"+ref, image+":"+ref
	if docker.IsDigest(ref) {
		shown, name = image+"@"+ref, ""
	}
	archive := export.ImageArchiveName(image, ref)
	if er.dry {
		body := pyjson.NewObject().Set("repository", repo.Name).Set("path", er.folder).Set("form", er.form).Set("name", archive).
			Set("files", tally.Files()).Set("bytes", tally.Bytes()).Set("exact", true).
			Set("max_files", limits.MaxFiles).Set("max_bytes", limits.MaxBytes).Set("image", shown)
		if platforms != nil {
			body.Set("platforms", platforms)
		}
		return writeExportJSON(w, req, s.withPlaces(req, body))
	}
	release, ok := s.takeSlot(w, req)
	if !ok {
		return nil
	}
	defer release()
	started := time.Now()
	streamHeaders(w, "application/x-tar", archive)
	out, done := s.stallBound(w)
	err = export.WriteOCI(out, export.Image{Manifest: chosen.data, MediaType: chosen.mediaType, Name: name, Blobs: blobs})
	done()
	s.exportLog(er, len(blobs)+1, tally.Bytes(), started, err)
	endStream(err)
	return nil
}
