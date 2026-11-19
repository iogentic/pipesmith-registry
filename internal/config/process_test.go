package config_test

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// Process values this product adds to config.json: tls_cert and tls_key
// (decision E9 of the execution plan) and trusted_proxies (E8). A value at
// its default is never written, so config.json of a default start stays as
// the reference writes it.

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTrustedProxiesLoad(t *testing.T) {
	dir := writeConfig(t, `{"trusted_proxies": ["10.0.0.0/8", "192.0.2.7", "2001:db8:1::/48", "::1"]}`)
	cfg, err := config.Load(dir, config.Env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.0.0.0/8", "192.0.2.7", "2001:db8:1::/48", "::1"}; !reflect.DeepEqual(cfg.TrustedProxies, want) {
		t.Fatalf("TrustedProxies %q, want %q", cfg.TrustedProxies, want)
	}
	got, err := config.ParseTrustedProxies(cfg.TrustedProxies)
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.0.2.7/32"),
		netip.MustParsePrefix("2001:db8:1::/48"), netip.MustParsePrefix("::1/128")}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseTrustedProxies: %v %v, want %v", got, err, want)
	}
	for _, text := range []string{`{}`, `{"trusted_proxies": null}`, `{"trusted_proxies": []}`} {
		cfg, err := config.Load(writeConfig(t, text), config.Env(nil))
		if err != nil || cfg.TrustedProxies != nil {
			t.Errorf("%s: %q %v, want none", text, cfg.TrustedProxies, err)
		}
	}
}

func TestTrustedProxiesRefused(t *testing.T) {
	for _, c := range []struct{ value, message string }{
		{`"10.0.0.1"`, "trusted_proxies is not a list of addresses"},
		{`[10]`, "trusted_proxies is not a list of addresses"},
		{`["demo-proxy.example.com"]`, "trusted_proxies: 'demo-proxy.example.com' is not an IP address or a network in CIDR form"},
		{`["*"]`, "trusted_proxies: '*' is not an IP address or a network in CIDR form"},
		{`[""]`, "trusted_proxies: '' is not an IP address or a network in CIDR form"},
		{`[" 10.0.0.1"]`, "trusted_proxies: ' 10.0.0.1' is not an IP address or a network in CIDR form"},
		{`["10.0.0.0/33"]`, "trusted_proxies: '10.0.0.0/33' is not an IP address or a network in CIDR form"},
		{`["fe80::1%eth0"]`, "trusted_proxies: 'fe80::1%eth0' is not an IP address or a network in CIDR form"},
		{`["10.0.0.5/8"]`, "trusted_proxies: '10.0.0.5/8' sets bits past its prefix length; the network is 10.0.0.0/8"},
		{`["::ffff:10.0.0.1"]`, "trusted_proxies: '::ffff:10.0.0.1' is an IPv4 address in IPv6 form; write it as IPv4"},
		{`["10.0.0.0/8", "demo-proxy"]`, "trusted_proxies: 'demo-proxy' is not an IP address or a network in CIDR form"},
	} {
		dir := writeConfig(t, `{"trusted_proxies": `+c.value+`}`)
		_, err := config.Load(dir, config.Env(nil))
		var pe *pyjson.Error
		want := filepath.Join(dir, "config.json") + ": " + c.message
		if !errors.As(err, &pe) || pe.Class != "ValueError" || pe.Message != want {
			t.Errorf("%s: %v, want ValueError %q", c.value, err, want)
		}
	}
}

func TestTrustedProxiesSave(t *testing.T) {
	cfg := config.New(filepath.Join(t.TempDir(), "data"))
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.ConfigPath())
	if err != nil || !strings.Contains(string(data), `"docker_ports": {}`) || strings.Contains(string(data), "trusted_proxies") {
		t.Fatalf("default config.json %q %v", data, err)
	}
	cfg.TrustedProxies = []string{"10.0.0.0/8", "192.0.2.7"}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"bind\": \"127.0.0.1\",\n  \"port\": 8793,\n  \"stores\": [],\n  \"docker_ports\": {},\n" +
		"  \"trusted_proxies\": [\n    \"10.0.0.0/8\",\n    \"192.0.2.7\"\n  ]\n}\n"
	if data, err := os.ReadFile(cfg.ConfigPath()); err != nil || string(data) != want {
		t.Fatalf("config.json %q %v, want %q", data, err, want)
	}
	loaded, err := config.Load(cfg.DataDir, config.Env(nil))
	if err != nil || !reflect.DeepEqual(loaded.TrustedProxies, cfg.TrustedProxies) {
		t.Fatalf("loaded %q %v", loaded.TrustedProxies, err)
	}
	cfg.TrustedProxies = []string{"demo-proxy"}
	var pe *pyjson.Error
	if err := config.Save(cfg); !errors.As(err, &pe) || pe.Class != "ValueError" ||
		pe.Message != "trusted_proxies: 'demo-proxy' is not an IP address or a network in CIDR form" {
		t.Fatalf("save of a bad entry: %v", err)
	}
}

func TestTLSFiles(t *testing.T) {
	dir := writeConfig(t, `{"tls_cert": "/srv/demo/cert.pem", "tls_key": "/srv/demo/key.pem"}`)
	cfg, err := config.Load(dir, config.Env(nil))
	if err != nil || cfg.TLSCert != "/srv/demo/cert.pem" || cfg.TLSKey != "/srv/demo/key.pem" {
		t.Fatalf("loaded %q %q %v", cfg.TLSCert, cfg.TLSKey, err)
	}
	for _, text := range []string{`{}`, `{"tls_cert": null, "tls_key": null}`} {
		cfg, err := config.Load(writeConfig(t, text), config.Env(nil))
		if err != nil || cfg.TLSCert != "" || cfg.TLSKey != "" {
			t.Errorf("%s: %q %q %v", text, cfg.TLSCert, cfg.TLSKey, err)
		}
	}
	for _, key := range []string{"tls_cert", "tls_key"} {
		for _, value := range []string{`""`, `5`, `["/srv/demo/cert.pem"]`, `true`} {
			dir := writeConfig(t, `{"`+key+`": `+value+`}`)
			_, err := config.Load(dir, config.Env(nil))
			var pe *pyjson.Error
			want := filepath.Join(dir, "config.json") + ": " + key + " is not a path"
			if !errors.As(err, &pe) || pe.Class != "ValueError" || pe.Message != want {
				t.Errorf("%s %s: %v, want %q", key, value, err, want)
			}
		}
	}
	cfg = config.New(filepath.Join(t.TempDir(), "data"))
	cfg.TLSCert, cfg.TLSKey, cfg.TrustedProxies = "/srv/demo/cert.pem", "/srv/demo/key.pem", []string{"10.0.0.0/8"}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"bind\": \"127.0.0.1\",\n  \"port\": 8793,\n  \"stores\": [],\n  \"docker_ports\": {},\n" +
		"  \"tls_cert\": \"/srv/demo/cert.pem\",\n  \"tls_key\": \"/srv/demo/key.pem\",\n" +
		"  \"trusted_proxies\": [\n    \"10.0.0.0/8\"\n  ]\n}\n"
	if data, err := os.ReadFile(cfg.ConfigPath()); err != nil || string(data) != want {
		t.Fatalf("config.json %q %v, want %q", data, err, want)
	}
	loaded, err := config.Load(cfg.DataDir, config.Env(nil))
	if err != nil || loaded.TLSCert != cfg.TLSCert || loaded.TLSKey != cfg.TLSKey {
		t.Fatalf("loaded %q %q %v", loaded.TLSCert, loaded.TLSKey, err)
	}
	items := loaded.ProcessItems()
	if len(items) != 3 || items[0][0] != "tls_cert" || items[1][0] != "tls_key" || items[2][0] != "trusted_proxies" {
		t.Fatalf("ProcessItems %v", items)
	}
	if items := config.New(cfg.DataDir).ProcessItems(); len(items) != 0 {
		t.Fatalf("ProcessItems at the defaults %v", items)
	}
}
