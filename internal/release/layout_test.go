package release

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for text, want := range map[string]Version{
		"0.1.2":                 {0, 1, 2},
		"10.0.0":                {10, 0, 0},
		"1.20.300":              {1, 20, 300},
		"999999999.0.999999999": {999999999, 0, 999999999},
	} {
		got, err := ParseVersion(text)
		if err != nil || got != want || got.String() != text {
			t.Errorf("ParseVersion(%q) = %v %v", text, got, err)
		}
	}
	for _, text := range []string{"", "v0.1.2", "0.1", "0.1.2.3", "01.2.3", "0.01.2", "a.b.c", "0.1.2-rc1", " 0.1.2",
		"0.1.2\n", "1234567890.0.0", "0..2", "+1.2.3", "-1.2.3"} {
		if _, err := ParseVersion(text); !errors.Is(err, ErrVersion) {
			t.Errorf("ParseVersion(%q) = %v, want ErrVersion", text, err)
		}
	}
	order := []string{"0.0.1", "0.1.0", "0.1.9", "0.1.10", "0.2.0", "1.0.0", "2.0.0", "10.0.0"}
	for i := range order {
		for j := range order {
			a, _ := ParseVersion(order[i])
			b, _ := ParseVersion(order[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := a.Compare(b); got != want {
				t.Errorf("%s.Compare(%s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
}

// installRoot makes an install root with the given releases, each holding a
// pipesmith-registry script that answers version and schema, and current
// pointing to the first one.
func installRoot(t *testing.T, releases ...string) Layout {
	t.Helper()
	root := filepath.Join(t.TempDir(), "opt")
	for _, name := range releases {
		dir := filepath.Join(root, ReleasesDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, InstalledBinary), versionScript(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, ReleasesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if len(releases) > 0 {
		if err := os.Symlink(ReleasesDir+"/"+releases[0], filepath.Join(root, CurrentLink)); err != nil {
			t.Fatal(err)
		}
	}
	return Layout{Root: root}
}

func TestReadLink(t *testing.T) {
	l := installRoot(t, "0.1.0", "0.2.0")
	got, err := l.ReadLink(CurrentLink)
	if err != nil || got != "0.1.0" {
		t.Fatalf("ReadLink(current) = %q %v", got, err)
	}
	if _, err := l.ReadLink(PreviousLink); !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrLayout) {
		t.Fatalf("missing previous: %v, want fs.ErrNotExist", err)
	}
	if err := os.WriteFile(filepath.Join(l.Root, ReleasesDir, ".staging-1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(l.Root, "other", "0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"absolute":       filepath.Join(l.Root, ReleasesDir, "0.1.0"),
		"dot dot":        ReleasesDir + "/../other/0.1.0",
		"other dir":      "other/0.1.0",
		"missing":        ReleasesDir + "/0.9.0",
		"hidden":         ReleasesDir + "/.staging-1",
		"nested":         ReleasesDir + "/0.1.0/" + InstalledBinary,
		"trailing slash": ReleasesDir + "/0.1.0/",
		"releases only":  ReleasesDir,
		"partial":        ReleasesDir + "/0.2.0.partial",
	} {
		t.Run(name, func(t *testing.T) {
			link := filepath.Join(l.Root, PreviousLink)
			os.Remove(link)
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if got, err := l.ReadLink(PreviousLink); !errors.Is(err, ErrLayout) {
				t.Fatalf("ReadLink to %q = %q %v, want ErrLayout", target, got, err)
			}
		})
	}
	link := filepath.Join(l.Root, PreviousLink)
	os.Remove(link)
	if err := os.WriteFile(link, []byte("releases/0.1.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReadLink(PreviousLink); !errors.Is(err, ErrLayout) || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("a file as previous: %v", err)
	}
}

func TestSwapLink(t *testing.T) {
	l := installRoot(t, "0.1.0", "0.2.0")
	if err := l.SwapLink(CurrentLink, "0.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := l.SwapLink(PreviousLink, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	for link, want := range map[string]string{CurrentLink: "releases/0.2.0", PreviousLink: "releases/0.1.0"} {
		target, err := os.Readlink(filepath.Join(l.Root, link))
		if err != nil || target != want {
			t.Fatalf("%s -> %q %v, want %q", link, target, err, want)
		}
	}
	entries, err := os.ReadDir(l.Root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "current,previous,releases" {
		t.Fatalf("root holds %v", names)
	}
	if err := l.SwapLink(CurrentLink, "../x"); !errors.Is(err, ErrLayout) {
		t.Fatalf("a link out of releases: %v", err)
	}
	if err := l.SwapLink("other", "0.1.0"); !errors.Is(err, ErrLayout) {
		t.Fatalf("a link of another name: %v", err)
	}
	if err := os.Mkdir(filepath.Join(l.Root, ".current.new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.Root, ".current.new", "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.SwapLink(CurrentLink, "0.1.0"); err == nil {
		t.Fatal("a swap over a busy temporary name passed")
	}
	if got, _ := l.ReadLink(CurrentLink); got != "0.2.0" {
		t.Fatalf("a failed swap changed current to %q", got)
	}
}

func writeConfig(t *testing.T, root, text string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ConfigFile), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadUpdateConfig(t *testing.T) {
	root := t.TempDir()
	_, err := LoadUpdateConfig(root)
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), filepath.Join(root, ConfigFile)) {
		t.Fatalf("missing file: %v", err)
	}

	writeConfig(t, root, `{"trusted_key": "/etc/keys/trusted.pub", "data_dir": "/var/lib/data", "channel": 3}`)
	cfg, err := LoadUpdateConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedKey != "/etc/keys/trusted.pub" || cfg.Unit != DefaultUnit || cfg.DataDir != "/var/lib/data" {
		t.Fatalf("config %+v", cfg)
	}

	writeConfig(t, root, `{"trusted_key": "keys/trusted.pub", "unit": "registry@a_1.service", "data_dir": "data"}`)
	cfg, err = LoadUpdateConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedKey != filepath.Join(root, "keys/trusted.pub") || cfg.Unit != "registry@a_1.service" || cfg.DataDir != filepath.Join(root, "data") {
		t.Fatalf("relative paths: %+v", cfg)
	}

	writeConfig(t, root, `{}`)
	cfg, err = LoadUpdateConfig(root)
	if err != nil || cfg.TrustedKey != "" || cfg.Unit != DefaultUnit || cfg.DataDir != "" {
		t.Fatalf("empty object: %+v %v", cfg, err)
	}

	for name, text := range map[string]string{
		"not JSON":           `{`,
		"array":              `[]`,
		"null":               `null`,
		"key not a string":   `{"trusted_key": 1}`,
		"unit not a string":  `{"unit": null}`,
		"data not a string":  `{"data_dir": []}`,
		"unit dot dot":       `{"unit": "../x.service"}`,
		"unit option":        `{"unit": "-x.service"}`,
		"unit no ending":     `{"unit": "pipesmith-registry"}`,
		"unit socket":        `{"unit": "pipesmith-registry.socket"}`,
		"unit space":         `{"unit": "a b.service"}`,
		"unit slash":         `{"unit": "a/b.service"}`,
		"unit empty":         `{"unit": ""}`,
		"unit only ending":   `{"unit": ".service"}`,
		"trailing value":     `{} {}`,
		"too large":          `{"x": "` + strings.Repeat("a", maxConfig) + `"}`,
		"duplicate key":      `{"unit": "a.service", "unit": "b.service"}`,
		"unit newline":       `{"unit": "a.service\n"}`,
		"unit too long name": `{"unit": "` + strings.Repeat("a", 250) + `.service"}`,
	} {
		t.Run(name, func(t *testing.T) {
			writeConfig(t, root, text)
			if cfg, err := LoadUpdateConfig(root); !errors.Is(err, ErrConfig) {
				t.Fatalf("%s: %+v %v, want ErrConfig", text, cfg, err)
			}
		})
	}
}
