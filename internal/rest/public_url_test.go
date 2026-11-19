package rest

import (
	"context"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/repos"
)

func TestRemoteAddressIsAnsweredWithoutCredentials(t *testing.T) {
	stored := "https://deploy-user:demo-secret@repo.example.com/maven2"
	defined := "https://deploy-user:demo-secret@repo.example.com/raw/"
	snap := &model.Snapshot{Repositories: []model.Repository{{Name: "demo-proxy", Format: "maven2", Type: "proxy", Online: true,
		Attributes: map[string]any{"proxy": map[string]any{"remoteUrl": stored, "contentMaxAge": 1440}}}}}
	env := &Env{Context: context.Background(), Snapshot: snap, Repositories: repos.New([]repos.Repo{
		{Name: "demo-proxy", Format: "maven2", Type: "proxy", Defined: true},
		{Name: "demo-raw-proxy", Format: "raw", Type: "proxy", Defined: true, RemoteURL: &defined},
	}, nil, nil), Permits: func(string) bool { return true }, Base: "http://localhost:14650"}

	answers := map[string]func() (Answer, error){
		"repositories":        func() (Answer, error) { return Repositories(env) },
		"repository":          func() (Answer, error) { return Repository(env, "demo-proxy") },
		"defined repository":  func() (Answer, error) { return Repository(env, "demo-raw-proxy") },
		"repository settings": func() (Answer, error) { return RepositorySettings(env) },
		"detail":              func() (Answer, error) { return RepositoryDetail(env, "maven", "proxy", "demo-proxy") },
	}
	for name, answer := range answers {
		a, err := answer()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		data, _ := a.JSON()
		text := string(data)
		if strings.Contains(text, "demo-secret") || strings.Contains(text, "deploy-user") || !strings.Contains(text, "https://repo.example.com/") {
			t.Errorf("%s: %s", name, text)
		}
	}
	if got := snap.Repositories[0].Attributes["proxy"].(map[string]any)["remoteUrl"]; got != stored {
		t.Errorf("the snapshot holds %v after the answers", got)
	}
}

func TestEveryStoredAddressIsAnsweredWithoutCredentials(t *testing.T) {
	slash := "https://deploy-user:demo-with/slash@registry.example.com/"
	snap := &model.Snapshot{Repositories: []model.Repository{{Name: "demo-npm-proxy", Format: "npm", Type: "proxy", Online: true,
		Attributes: map[string]any{"proxy": map[string]any{"remoteUrl": slash},
			"httpclient": map[string]any{"connection": map[string]any{"proxyUrl": "http://deploy-user:demo-secret@proxy.example.com:3128"}}}}}}
	env := &Env{Context: context.Background(), Snapshot: snap, Repositories: repos.New([]repos.Repo{
		{Name: "demo-npm-proxy", Format: "npm", Type: "proxy", Defined: true}}, nil, nil),
		Permits: func(string) bool { return true }, Base: "http://localhost:14650"}
	for name, answer := range map[string]func() (Answer, error){
		"repository":          func() (Answer, error) { return Repository(env, "demo-npm-proxy") },
		"repository settings": func() (Answer, error) { return RepositorySettings(env) },
		"detail":              func() (Answer, error) { return RepositoryDetail(env, "npm", "proxy", "demo-npm-proxy") },
	} {
		a, err := answer()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		data, _ := a.JSON()
		text := string(data)
		if strings.Contains(text, "demo-with") || strings.Contains(text, "demo-secret") || strings.Contains(text, "deploy-user") ||
			!strings.Contains(text, "https://registry.example.com/") {
			t.Errorf("%s: %s", name, text)
		}
	}
	if got := snap.Repositories[0].Attributes["proxy"].(map[string]any)["remoteUrl"]; got != slash {
		t.Errorf("the snapshot holds %v after the answers", got)
	}
}
