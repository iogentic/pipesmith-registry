package cli

import (
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

func TestStateShowPrintsRemoteAddressesWithoutCredentials(t *testing.T) {
	dir := dataDir(t)
	remote := "https://deploy-user:demo-secret@repo.example.com/maven2/"
	slash := "https://deploy-user:demo-with/slash@registry.example.com/"
	st, err := state.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap := &model.Snapshot{
		BlobStores: []model.BlobStore{{Name: "store-s3", Type: "S3", Path: "s3://deploy-user:demo-secret@bucket.example.com/blobs"}},
		Repositories: []model.Repository{
			{Name: "demo-maven-proxy", Format: "maven2", Type: "proxy", Online: true,
				Attributes: map[string]any{"proxy": map[string]any{"remoteUrl": remote}}},
			{Name: "demo-npm-proxy", Format: "npm", Type: "proxy", Online: true,
				Attributes: map[string]any{"proxy": map[string]any{"remoteUrl": slash}}},
		},
	}
	if err := st.Write([]state.Kind{state.KindBlobStores, state.KindRepositories}, snap); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	_, out := show(t)
	for _, secret := range []string{"demo-secret", "demo-with", "deploy-user"} {
		if strings.Contains(out, secret) {
			t.Errorf("state show prints %s:\n%s", secret, out)
		}
	}
	for _, want := range []string{"https://repo.example.com/maven2/", "https://registry.example.com/", "s3://bucket.example.com/blobs"} {
		if !strings.Contains(out, want) {
			t.Errorf("state show lacks %s:\n%s", want, out)
		}
	}
	// the stored rows keep the addresses as given
	st, err = state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Repositories[0].Attributes["proxy"].(map[string]any)["remoteUrl"]; got != remote {
		t.Errorf("stored remoteUrl %v", got)
	}
	if loaded.BlobStores[0].Path != snap.BlobStores[0].Path {
		t.Errorf("stored path %v", loaded.BlobStores[0].Path)
	}
}
