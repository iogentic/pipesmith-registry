package repos_test

import (
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/repos"
)

func TestPublicURL(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{"https://deploy-user:demo-secret@repo.example.com/maven2/", "https://repo.example.com/maven2/"},
		{"https://deploy-user@repo.example.com", "https://repo.example.com"},
		{"https://deploy-user:demo-secret@repo.example.com:8443?x=@y#z", "https://repo.example.com:8443?x=@y#z"},
		{"HTTPS://User:Pw@Repo.Example.com/a", "HTTPS://Repo.Example.com/a"},
		{"https://:demo-secret@repo.example.com/", "https://repo.example.com/"},
		// these do not parse: the authority is cut up to its last "@"
		{"https://deploy-user:demo@secret@repo.example.com/x", "https://repo.example.com/x"},
		{"https://deploy user:demo-secret@repo.example.com/x", "https://repo.example.com/x"},
		{"https://deploy-user:%zz@repo.example.com/", "https://repo.example.com/"},
		{"https://deploy-user:demo-secret@repo.example.com/a b%zz", "https://repo.example.com/a b%zz"},
		// no user information: byte for byte
		{"https://repo.example.com/maven2/", "https://repo.example.com/maven2/"},
		{"https://repo.example.com/a%2Fb/?q=1&r=%41#x", "https://repo.example.com/a%2Fb/?q=1&r=%41#x"},
		{"https://repo.example.com/path@v1", "https://repo.example.com/path@v1"},
		{"https://repo.example.com/?q=a@b", "https://repo.example.com/?q=a@b"},
		{"HTTPS://Repo.Example.COM:0443//x/../y", "HTTPS://Repo.Example.COM:0443//x/../y"},
		{"https://repo.example.com/a b%zz", "https://repo.example.com/a b%zz"},
		{"mailto:someone@example.com", "mailto:someone@example.com"},
		{"repo.example.com/maven2", "repo.example.com/maven2"},
		{"not an address", "not an address"},
		{"", ""},
		// round two: a password with an unescaped slash does not parse
		{"https://deploy-user:CANARY-with/slash-3e7c@registry.example.com/", "https://registry.example.com/"},
		{"https://deploy-user:a/b@registry.example.com/npm/", "https://registry.example.com/npm/"},
		{"https://repo.example.com:8443/npm/@scope/", "https://repo.example.com:8443/npm/@scope/"},
		{"https://deploy-user:demo-secret@repo.example.com:8443/npm/@scope/", "https://repo.example.com:8443/npm/@scope/"},
		{"https://repo.example.com:99999/x", "https://repo.example.com:99999/x"},
	} {
		if got := repos.PublicURL(c.raw); got != c.want {
			t.Errorf("PublicURL(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestRowShowsTheRemoteAddressWithoutCredentials(t *testing.T) {
	remote := "https://deploy-user:demo-secret@repo.example.com/maven2/"
	row := repos.Row{Name: "demo-proxy", Format: "raw", Type: "proxy", RemoteURL: &remote}
	got, _ := row.Object().Get("remote_url")
	if got != "https://repo.example.com/maven2/" {
		t.Errorf("remote_url %v", got)
	}
	if !strings.Contains(remote, "demo-secret") {
		t.Errorf("the row changed its address: %s", remote)
	}
}

func TestPublicValue(t *testing.T) {
	remote := "https://deploy-user:demo-secret@repo.example.com/maven2/"
	stored := map[string]any{
		"proxy":   map[string]any{"remoteUrl": remote, "contentMaxAge": 1440},
		"mirrors": []any{remote, "plain text", map[string]any{"url": "https://u:p/q@mirror.example.com/"}},
		"note":    "deploy-user@repo.example.com",
		"online":  true,
	}
	got := repos.PublicValue(stored).(map[string]any)
	if v := got["proxy"].(map[string]any)["remoteUrl"]; v != "https://repo.example.com/maven2/" {
		t.Errorf("remoteUrl %v", v)
	}
	mirrors := got["mirrors"].([]any)
	if mirrors[0] != "https://repo.example.com/maven2/" || mirrors[1] != "plain text" ||
		mirrors[2].(map[string]any)["url"] != "https://mirror.example.com/" {
		t.Errorf("mirrors %v", mirrors)
	}
	if got["note"] != "deploy-user@repo.example.com" || got["online"] != true || got["proxy"].(map[string]any)["contentMaxAge"] != 1440 {
		t.Errorf("other values %v", got)
	}
	if stored["proxy"].(map[string]any)["remoteUrl"] != remote || stored["mirrors"].([]any)[0] != remote {
		t.Errorf("the stored value changed: %v", stored)
	}
}
