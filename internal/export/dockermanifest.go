package export

import (
	"archive/tar"
	"encoding/json"
)

// dockerManifestEntry is one image of manifest.json, the file docker load
// reads on an engine without OCI support.
type dockerManifestEntry struct {
	Config   string
	RepoTags []string
	Layers   []string
}

// writeDockerManifest writes manifest.json for the image: the config and
// the layers as paths of the tar, the first blob being the config, and the
// reference as its tag ("" names none). An image without blobs gets none.
func writeDockerManifest(tw *tar.Writer, image Image) error {
	if len(image.Blobs) == 0 {
		return nil
	}
	entry := dockerManifestEntry{Config: blobPath(image.Blobs[0].Digest), Layers: []string{}}
	for _, b := range image.Blobs[1:] {
		entry.Layers = append(entry.Layers, blobPath(b.Digest))
	}
	if image.Name != "" {
		entry.RepoTags = []string{image.Name}
	}
	data, err := json.Marshal([]dockerManifestEntry{entry})
	if err != nil {
		return err
	}
	return writeMember(tw, "manifest.json", data)
}
