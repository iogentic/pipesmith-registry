package release

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestHealthAddressOfConfig(t *testing.T) {
	r := newRig(t, "0.1.0")
	_, port, err := net.SplitHostPort(r.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r.config(fmt.Sprintf(`, "health_address": "127.0.0.1:%s"`, port))
	r.u.Address = func(*UpdateConfig) (string, error) { return "", errors.New("the address of config.json was asked") }
	if err := r.u.Update(r.release(versionScript("0.2.0")), false); err != nil {
		t.Fatalf("update: %v\n%s", err, r.out.String())
	}
	if !strings.Contains(r.out.String(), "healthy: http://127.0.0.1:"+port+" answers") {
		t.Fatalf("output:\n%s", r.out.String())
	}
}

func TestHealthAddressKey(t *testing.T) {
	for text, want := range map[string]string{
		`"127.0.0.1:14771"`:             "127.0.0.1:14771",
		`"192.0.2.7:443"`:               "192.0.2.7:443",
		`"[::1]:14771"`:                 "[::1]:14771",
		`"[2001:db8::5]:8080"`:          "[2001:db8::5]:8080",
		`"registry.example.org:14771"`:  "registry.example.org:14771",
		`"Registry-1.Example.ORG:1"`:    "Registry-1.Example.ORG:1",
		`"registry.example.org:65535"`:  "registry.example.org:65535",
		`"localhost:14771"`:             "localhost:14771",
		`"registry.example.org.:14771"`: "",
	} {
		root := t.TempDir()
		writeConfig(t, root, `{"health_address": `+text+`}`)
		cfg, err := LoadUpdateConfig(root)
		if want == "" {
			if !errors.Is(err, ErrConfig) {
				t.Errorf("%s: %v", text, err)
			}
			continue
		}
		if err != nil || cfg.HealthAddress != want {
			t.Errorf("%s: %+v %v, want %s", text, cfg, err, want)
		}
	}
	for _, text := range []string{`"http://127.0.0.1:14771"`, `"127.0.0.1"`, `"127.0.0.1:0"`, `"127.0.0.1:65536"`,
		`"127.0.0.1:port"`, `"127.0.0.1:014771"`, `"host/path:80"`, `"127.0.0.1:80/api"`, `"a b:80"`, `""`, `":80"`,
		`"::1:80"`, `"[fe80::1%eth0]:80"`, `"user@host:80"`, `"-host:80"`, `"0.0.0.0:80"`, `"[::]:80"`, `5`, `null`, `["127.0.0.1:80"]`} {
		root := t.TempDir()
		writeConfig(t, root, `{"health_address": `+text+`}`)
		if _, err := LoadUpdateConfig(root); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "health_address") {
			t.Errorf("%s: %v", text, err)
		}
	}
	root := t.TempDir()
	writeConfig(t, root, `{}`)
	if cfg, err := LoadUpdateConfig(root); err != nil || cfg.HealthAddress != "" {
		t.Fatalf("without the key: %+v %v", cfg, err)
	}
}
