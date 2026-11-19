package cli

import (
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// publicAddresses replaces the attributes of the repositories and the
// paths of the blob stores of a loaded snapshot by copies without user
// information, for state show. The stored rows stay as they are.
func publicAddresses(snap *model.Snapshot) {
	for i := range snap.Repositories {
		if attributes, ok := repos.PublicValue(snap.Repositories[i].Attributes).(map[string]any); ok {
			snap.Repositories[i].Attributes = attributes
		}
	}
	for i := range snap.BlobStores {
		snap.BlobStores[i].Path = repos.PublicValue(snap.BlobStores[i].Path).(string)
	}
}
