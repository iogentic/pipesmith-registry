package cli

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/config"
	"github.com/mirosso25/pipesmith-registry/internal/pystr"
	"github.com/mirosso25/pipesmith-registry/internal/server"
	"github.com/mirosso25/pipesmith-registry/internal/testkit"
	"github.com/mirosso25/pipesmith-registry/internal/version"
)

// The vectors come from tools/vectors/cli.py, which runs the argument
// parser and the output helpers of cli.py of the reference under the
// program name of this product.
type cliVectors struct {
	Version    string   `json:"version"`
	Program    string   `json:"program"`
	Help       string   `json:"help"`
	ConfigKeys []string `json:"config_keys"`
	Kinds      []string `json:"kinds"`
	Parse      []struct {
		Argv   []string                   `json:"argv"`
		Usage  *string                    `json:"usage"`
		Exit   *int                       `json:"exit"`
		Stdout string                     `json:"stdout"`
		Run    string                     `json:"run"`
		Values map[string]json.RawMessage `json:"values"`
	} `json:"parse"`
	CommandHelp []struct {
		Argv   []string `json:"argv"`
		Exit   int      `json:"exit"`
		Stdout string   `json:"stdout"`
	} `json:"command_help"`
	Size []struct {
		Count json.RawMessage `json:"count"`
		Text  string          `json:"text"`
	} `json:"size"`
	Time []struct {
		MS   *int64 `json:"ms"`
		Text string `json:"text"`
	} `json:"time"`
	Table []struct {
		Rows  [][]string `json:"rows"`
		Right []int      `json:"right"`
		Out   string     `json:"out"`
	} `json:"table"`
	// TableEncoded is _table into a strict UTF-8 stream; cells are code
	// points, as a lone surrogate has no other JSON form.
	TableEncoded []struct {
		Rows    [][][]rune `json:"rows"`
		Right   []int      `json:"right"`
		Out     string     `json:"out"`
		Error   *string    `json:"error"`
		Message string     `json:"message"`
	} `json:"table_encoded"`
	PortOrNone []struct {
		Value   string      `json:"value"`
		Port    json.Number `json:"port"`
		Error   *string     `json:"error"`
		Message string      `json:"message"`
	} `json:"port_or_none"`
	StoreName []struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
	} `json:"store_name"`
}

func loadVectors(t *testing.T) *cliVectors {
	t.Helper()
	data, err := os.ReadFile("../../testdata/vectors/cli.json")
	if err != nil {
		t.Fatal(err)
	}
	var v cliVectors
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	if len(v.Parse) < 100 || len(v.CommandHelp) != 12 || v.Program != "pipesmith-registry" {
		t.Fatalf("vector file: %d parse cases, %d help texts", len(v.Parse), len(v.CommandHelp))
	}
	return &v
}

// run is Main with captured output.
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Main(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// dataDir points the commands at a fresh data directory.
func dataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	for _, name := range []string{config.EnvData, config.EnvPort, config.LegacyEnvPort} {
		t.Setenv(name, "")
	}
	t.Setenv(config.LegacyEnvData, dir)
	return dir
}

func wantHelp(v *cliVectors) string {
	text := v.Help
	text = strings.Replace(text, "Artifact Repository "+v.Version+"\n", "Pipesmith Registry "+version.Version+"\n", 1)
	text = strings.ReplaceAll(text, "python -m artifact_repository", "pipesmith-registry")
	text = strings.Replace(text, "The data directory is ARTIFACT_REPOSITORY_DATA, else <project root>/data.\n",
		"The data directory is PIPESMITH_REGISTRY_DATA, else ARTIFACT_REPOSITORY_DATA, else <project root>/data.\n", 1)
	text = strings.Replace(text, "ARTIFACT_REPOSITORY_PORT overrides the port of the configuration.\n",
		"PIPESMITH_REGISTRY_PORT, else ARTIFACT_REPOSITORY_PORT, overrides the port of the configuration.\n", 1)
	// The commands of the import are new in this product; the rest of the
	// help is that of the reference.
	return strings.Replace(text, "\n\nSettings\n", "\n\n"+stateHelpBody+"Settings\n", 1)
}

// withStateCommands is a usage message of the reference with the commands
// of the import in the list of choices.
func withStateCommands(text string) string {
	return strings.Replace(text, "import-settings, config", "import-settings, import, import-state, state, config", 1)
}

// withServeFlag is a text of the reference with the flags of serve that name
// the host names of a network bind and that ask
// for LDAP bind passwords, as argparse 3.12.13 prints it.
func withServeFlag(text string) string {
	text = strings.Replace(text, "usage: pipesmith-registry serve [-h] [--port PORT] [--bind BIND]\n",
		"usage: pipesmith-registry serve [-h] [--port PORT] [--bind BIND]\n                                [--server-names SERVER_NAMES]\n"+
			"                                [--ask-ldap-password]\n", 1)
	return strings.Replace(text, "  -h, --help   show this help message and exit\n  --port PORT\n  --bind BIND\n",
		"  -h, --help            show this help message and exit\n  --port PORT\n  --bind BIND\n  --server-names SERVER_NAMES\n"+
			"  --ask-ldap-password\n", 1)
}

func TestHelpTexts(t *testing.T) {
	v := loadVectors(t)
	dir := dataDir(t)
	for _, args := range [][]string{nil, {"help"}, {"-h"}, {"--help"}, {"help", "extra"}} {
		code, out, errOut := run(t, args...)
		if code != 0 || out != wantHelp(v) || errOut != "" {
			t.Fatalf("%v: %d %q %q", args, code, out, errOut)
		}
	}
	for _, c := range v.CommandHelp {
		code, out, errOut := run(t, c.Argv...)
		if code != c.Exit || out != withServeFlag(c.Stdout) || errOut != "" {
			t.Errorf("%v: %d\n%s\nwant\n%s", c.Argv, code, out, c.Stdout)
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("help created the data directory")
	}
}

func TestParseVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Parse {
		ns, err := parseArgs(c.Argv)
		switch {
		case c.Usage != nil:
			if err == nil || err.Error() != withServeFlag(withStateCommands(*c.Usage)) {
				t.Errorf("%q: %v %v; want usage %q", c.Argv, ns, err, *c.Usage)
			}
		case c.Exit != nil:
			help, ok := err.(*helpExit)
			if !ok || help.text != withServeFlag(c.Stdout) {
				t.Errorf("%q: %v; want help %q", c.Argv, err, c.Stdout)
			}
		default:
			if err != nil {
				t.Errorf("%q: %v", c.Argv, err)
				continue
			}
			if ns.run != c.Run {
				t.Errorf("%q: run %q, want %q", c.Argv, ns.run, c.Run)
			}
			want := c.Values
			if ns.run == "cmd_serve" {
				// The flag of serve for LDAP bind passwords is new in this product.
				want = maps.Clone(c.Values)
				want["ask_ldap_password"] = json.RawMessage("false")
				want["server_names"] = json.RawMessage("null")
			}
			if len(ns.values) != len(want) {
				t.Errorf("%q: values %v, want %v", c.Argv, ns.values, want)
			}
			for key, raw := range want {
				got, ok := ns.values[key]
				if !ok || !sameValue(got, raw) {
					t.Errorf("%q: %s = %v, want %s", c.Argv, key, got, raw)
				}
			}
		}
	}
}

func sameValue(got any, raw json.RawMessage) bool {
	switch g := got.(type) {
	case nil:
		return string(raw) == "null"
	case bool:
		return string(raw) == fmt.Sprint(g)
	case string:
		var s string
		return json.Unmarshal(raw, &s) == nil && s == g
	case *big.Int:
		return g.String() == string(raw)
	}
	return false
}

func TestOutputHelperVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Size {
		var count *big.Int
		if string(c.Count) != "null" {
			count, _ = new(big.Int).SetString(string(c.Count), 10)
		}
		if got := size(count); got != c.Text {
			t.Errorf("_size(%s) = %q, want %q", c.Count, got, c.Text)
		}
	}
	for _, c := range v.Time {
		if got := timeText(c.MS); got != c.Text {
			t.Errorf("_time(%v) = %q, want %q", c.MS, got, c.Text)
		}
	}
	for _, c := range v.Table {
		var out bytes.Buffer
		if err := table(&out, c.Rows, c.Right...); err != nil || out.String() != c.Out {
			t.Errorf("_table(%q) = %q, %v, want %q", c.Rows, out.String(), err, c.Out)
		}
	}
	if len(v.TableEncoded) < 10 {
		t.Fatalf("%d table_encoded cases", len(v.TableEncoded))
	}
	failures := 0
	for _, c := range v.TableEncoded {
		rows := make([][]string, len(c.Rows))
		for i, row := range c.Rows {
			for _, cell := range row {
				rows[i] = append(rows[i], pystr.FromRunes(cell))
			}
		}
		var out bytes.Buffer
		err := table(&out, rows, c.Right...)
		if out.String() != c.Out {
			t.Errorf("_table(%q) = %q, want %q", rows, out.String(), c.Out)
		}
		if c.Error == nil && err != nil || c.Error != nil && (err == nil || server.PyClass(err) != *c.Error || err.Error() != c.Message) {
			t.Errorf("_table(%q): err %v, want %v %s", rows, err, c.Error, c.Message)
		}
		if c.Error != nil {
			failures++
		}
	}
	if failures < 9 {
		t.Fatalf("%d encode failures, the generator writes 9", failures)
	}
	for _, c := range v.PortOrNone {
		got, err := portOrNone(c.Value)
		switch {
		case c.Error != nil:
			if err == nil || err.Error() != c.Message {
				t.Errorf("port_or_none(%q) = %v, %v; want %q", c.Value, got, err, c.Message)
			}
		case c.Port == "":
			if err != nil || got != nil {
				t.Errorf("port_or_none(%q) = %v, %v; want None", c.Value, got, err)
			}
		default:
			if err != nil || got.(*big.Int).String() != string(c.Port) {
				t.Errorf("port_or_none(%q) = %v, %v; want %s", c.Value, got, err, c.Port)
			}
		}
	}
	for _, c := range v.StoreName {
		if got := storeName.MatchString(c.Name); got != c.OK {
			t.Errorf("store name %q: %v", c.Name, got)
		}
	}
	if strings.Join(configKeys, ",") != strings.Join(v.ConfigKeys, ",") || strings.Join(kinds, ",") != strings.Join(v.Kinds, ",") {
		t.Fatalf("keys %v kinds %v", configKeys, kinds)
	}
}

func TestUsageErrorsAndVersion(t *testing.T) {
	dataDir(t)
	code, out, errOut := run(t, "frobnicate")
	if code != 2 || out != "" || errOut != "error: pipesmith-registry: argument command: invalid choice: 'frobnicate' (choose from serve, store, scan, repos, docker-check, import-settings, import, import-state, state, config, version)\nsee: pipesmith-registry help\n" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	code, out, _ = run(t, "version")
	if code != 0 || out != "pipesmith-registry "+version.Version+"\n" {
		t.Fatalf("%d %q", code, out)
	}
	code, _, errOut = run(t, "config", "--get", "nope")
	if code != 2 || errOut != "error: unknown key 'nope'; keys: data_dir, bind, port, config_path, index_path, repositories_path\n" {
		t.Fatalf("%d %q", code, errOut)
	}
}

func storesOf(t *testing.T, dir string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Stores []map[string]any `json:"stores"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Stores
}

func demoTree(t *testing.T, base string) string {
	t.Helper()
	root, err := testkit.MakeTree(filepath.Join(base, "tree"), map[string][]byte{
		"demo-raw/docs/readme.txt": []byte("hello"), "demo-raw/top.txt": []byte("top level"),
		"demo-releases/com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom": []byte("<project/>"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestStoreCommands(t *testing.T) {
	dir := dataDir(t)
	base := filepath.Dir(dir)
	tree := demoTree(t, base)
	code, out, errOut := run(t, "store", "add", "tree", "--kind", "directory", "--root", tree, "--repos", "demo-raw, demo-releases")
	if code != 0 || out != "added store tree (directory, "+tree+")\nnext: scan tree\n" || errOut != "" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if got := storesOf(t, dir); len(got) != 1 || got[0]["root"] != tree || fmt.Sprint(got[0]["repos"]) != "[demo-raw demo-releases]" {
		t.Fatalf("%v", got)
	}
	code, out, _ = run(t, "store", "list")
	want := "NAME  KIND       FILES  SIZE  ROOT\ntree  directory      -     -  " + tree + "  (repos: demo-raw, demo-releases)\n"
	if code != 0 || out != want {
		t.Fatalf("%d\n%s\nwant\n%s", code, out, want)
	}
	code, out, errOut = run(t, "scan", "tree")
	if code != 0 || !regexp.MustCompile(`^scan tree: live=3 deleted=0 copies=0 no_bytes=0 unparsable=0 seconds=\d+\.\d\d\n$`).MatchString(out) {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	code, out, _ = run(t, "store", "list")
	if code != 0 || !strings.Contains(out, "tree  directory      3  24 B  "+tree) {
		t.Fatalf("%q", out)
	}
	for _, c := range []struct {
		args []string
		text string
	}{
		{[]string{"store", "add", "tree", "--kind", "directory", "--root", tree}, "error: a store named tree is attached already\n"},
		{[]string{"store", "add", "gone", "--kind", "directory", "--root", filepath.Join(base, "nowhere")},
			"error: store root " + filepath.Join(base, "nowhere") + " does not exist or is not a directory\n"},
		{[]string{"store", "add", "../tree", "--kind", "directory", "--root", tree},
			"error: store name '../tree' is not allowed; use letters, digits, '.', '_' and '-'\n"},
		{[]string{"store", "add", "blobs", "--kind", "nexus-file", "--root", tree},
			"error: " + tree + " has no content/ directory, so it is not a file blob store\n"},
		{[]string{"store", "add", "two", "--kind", "nexus-file", "--root", tree, "--repos", "a"},
			"error: " + tree + " has no content/ directory, so it is not a file blob store\n"},
		{[]string{"store", "add", "two", "--kind", "directory", "--root", tree, "--manifest", filepath.Join(base, "none.tsv")},
			"error: manifest " + filepath.Join(base, "none.tsv") + " does not exist or is neither a file nor a directory\n"},
		{[]string{"store", "remove", "nope"}, "error: no store named nope is attached\n"},
		{[]string{"scan", "nope"}, "error: no store named nope is attached\n"},
	} {
		code, out, errOut := run(t, c.args...)
		if code != 1 || out != "" || errOut != c.text {
			t.Errorf("%v: %d %q %q, want %q", c.args, code, out, errOut, c.text)
		}
	}
	code, _, errOut = run(t, "store", "add", "s3x", "--kind", "s3", "--root", tree)
	if code != 2 || !strings.Contains(errOut, "'s3'") {
		t.Fatalf("%d %q", code, errOut)
	}
	code, out, _ = run(t, "store", "remove", "tree")
	if code != 0 || out != "removed store tree from the configuration and the index\n" || len(storesOf(t, dir)) != 0 {
		t.Fatalf("%d %q", code, out)
	}
	code, out, _ = run(t, "store", "list")
	if code != 0 || out != "no stores attached; add one with: store add <name> --kind <kind> --root <path>\n" {
		t.Fatalf("%q", out)
	}
	code, _, errOut = run(t, "scan")
	if code != 1 || errOut != "error: no stores attached; add one with: store add <name> --kind <kind> --root <path>\n" {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestStoreAddRelativeRootAndManifest(t *testing.T) {
	dir := dataDir(t)
	base := filepath.Dir(dir)
	demoTree(t, base)
	wd, _ := os.Getwd()
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
	real, _ := filepath.EvalSymlinks(base)
	code, out, errOut := run(t, "store", "add", "tree", "--kind", "directory", "--root", "tree")
	if code != 0 || storesOf(t, dir)[0]["root"] != filepath.Join(real, "tree") {
		t.Fatalf("%d %q %q %v", code, out, errOut, storesOf(t, dir))
	}
	os.WriteFile(filepath.Join(base, "m.tsv"), nil, 0o644)
	code, out, _ = run(t, "store", "add", "withm", "--kind", "directory", "--root", "tree", "--manifest", "m.tsv")
	want := "added store withm (directory, " + filepath.Join(real, "tree") + ")\nmanifest: " + filepath.Join(real, "m.tsv") +
		"\nthe first scan builds its lookup table below " + filepath.Join(dir, "manifests") + "\nnext: scan withm\n"
	if code != 0 || out != want {
		t.Fatalf("%d\n%s\nwant\n%s", code, out, want)
	}
	os.Mkdir(filepath.Join(base, "parts"), 0o755)
	code, out, _ = run(t, "store", "add", "withdir", "--kind", "directory", "--root", "tree", "--manifest", "parts")
	if code != 0 || !strings.Contains(out, "note: the manifest holds no part-*.tsv, part-*.tsv.gz or part-*.tsv.zst files\n") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestScanCountsAndMissingRoot(t *testing.T) {
	dir := dataDir(t)
	base := filepath.Dir(dir)
	root, err := testkit.MakeBlobStore(filepath.Join(base, "blobs"), testkit.SkipCaseBlobs())
	if err != nil {
		t.Fatal(err)
	}
	testkit.AddUnparsable(root, testkit.DefaultUnparsable)
	run(t, "store", "add", "s1", "--kind", "nexus-file", "--root", root)
	code, out, _ := run(t, "scan")
	if code != 0 || !strings.HasPrefix(out, "scan s1: live=1 deleted=1 copies=1 no_bytes=1 unparsable=1 seconds=") {
		t.Fatalf("%d %q", code, out)
	}
	tree := demoTree(t, base)
	run(t, "store", "add", "tree", "--kind", "directory", "--root", tree)
	run(t, "scan", "tree")
	os.Rename(tree, filepath.Join(base, "moved"))
	code, out, errOut := run(t, "scan")
	if code != 1 || !strings.Contains(out, "scan s1: live=1") ||
		!strings.HasPrefix(errOut, "error: scan tree failed, the index keeps its previous rows: store root "+tree+" is not a directory") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
}

// A ValueError of one store, from int() of a number beyond the digit limit,
// fails that store only: the scan reports it and goes on with the next.
func TestScanGoesOnAfterValueError(t *testing.T) {
	dir := dataDir(t)
	base := filepath.Dir(dir)
	s := testkit.NewBlobSpec("demo-raw", "big.bin")
	root, err := testkit.MakeBlobStore(filepath.Join(base, "digits"), []testkit.BlobSpec{s})
	if err != nil {
		t.Fatal(err)
	}
	props := testkit.SpecProperties(s)
	props["size"] = strings.Repeat("9", 4301)
	if err := os.WriteFile(filepath.Join(root, testkit.PropertiesLocation(s)), testkit.EncodeProperties(props, s.CreatedMS), 0o644); err != nil {
		t.Fatal(err)
	}
	other, err := testkit.MakeBlobStore(filepath.Join(base, "a"), []testkit.BlobSpec{testkit.NewBlobSpec("demo-raw", "ok.bin")})
	if err != nil {
		t.Fatal(err)
	}
	run(t, "store", "add", "digits", "--kind", "nexus-file", "--root", root)
	run(t, "store", "add", "a", "--kind", "nexus-file", "--root", other)
	code, out, errOut := run(t, "scan")
	want := "error: scan digits failed, the index keeps its previous rows: Exceeds the limit (4300 digits) for integer string" +
		" conversion: value has 4301 digits; use sys.set_int_max_str_digits() to increase the limit\n"
	if code != 1 || errOut != want || !regexp.MustCompile(`^scan a: live=1 deleted=0 copies=0 no_bytes=0 unparsable=0 seconds=\d+\.\d\d\n$`).MatchString(out) {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	code, out, _ = run(t, "store", "list")
	if code != 0 || !regexp.MustCompile(`(?m)^digits  nexus-file      -     -  .*\n^a       nexus-file      1   1 B  `).MatchString(out) {
		t.Fatalf("%d %q", code, out)
	}
}

// print() of a lone surrogate fails with UnicodeEncodeError on the strict
// UTF-8 stdout: the lines before it are out, main reports the error.
func TestReposTableFailsOnALoneSurrogate(t *testing.T) {
	dir := dataDir(t)
	base := filepath.Dir(dir)
	tree := demoTree(t, base)
	run(t, "store", "add", "tree", "--kind", "directory", "--root", tree)
	run(t, "scan")
	settings := filepath.Join(base, "surrogate.json")
	if err := os.WriteFile(settings, []byte(`[{"name": "\ud800"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, "import-settings", settings); code != 0 {
		t.Fatalf("import: %d %q", code, errOut)
	}
	code, out, errOut := run(t, "repos")
	if code != 1 || errOut != "error: 'utf-8' codec can't encode character '\\ud800' in position 0: surrogates not allowed\n" ||
		!strings.HasPrefix(out, "NAME ") || strings.Count(out, "\n") != 1+strings.Count(out, "no definition") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if code, out, _ := run(t, "repos", "--json"); code != 0 || !strings.Contains(out, `"name": "\ud800"`) {
		t.Fatalf("json: %d %q", code, out)
	}
}

func writeSettings(t *testing.T, path string, items ...map[string]any) string {
	t.Helper()
	data, _ := json.Marshal(items)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestReposAndImport(t *testing.T) {
	dir := dataDir(t)
	base := filepath.Dir(dir)
	jar := testkit.NewBlobSpec("demo-releases", "com/example/a/1.0/a-1.0.jar")
	jar.Content = []byte("12345")
	pom := testkit.NewBlobSpec("demo-releases", "com/example/a/1.0/a-1.0.pom")
	pom.Content, pom.CreatedMS = []byte("123"), 1_700_000_100_000
	old := testkit.NewBlobSpec("old-repo", "x.txt")
	old.Content = []byte("xy")
	layer := testkit.NewBlobSpec("demo-docker", "v2/blobs/x")
	layer.Content = []byte("layer")
	root, _ := testkit.MakeBlobStore(filepath.Join(base, "blobs"), []testkit.BlobSpec{jar, pom, old, layer})
	run(t, "store", "add", "s1", "--kind", "nexus-file", "--root", root)
	run(t, "scan")
	settings := writeSettings(t, filepath.Join(base, "settings.json"),
		map[string]any{"name": "demo-releases", "format": "maven2", "type": "hosted"},
		map[string]any{"name": "demo-docker", "format": "docker", "type": "hosted", "docker": map[string]any{"httpPort": 50100}},
		map[string]any{"name": "demo-group", "format": "maven2", "type": "group", "group": map[string]any{"memberNames": []string{"demo-releases"}}},
	)
	code, out, _ := run(t, "import-settings", settings)
	if code != 0 || out != "imported 3 repositories into "+filepath.Join(dir, "repositories.json")+"\n" {
		t.Fatalf("%d %q", code, out)
	}
	code, out, _ = run(t, "repos", "--json")
	var doc struct {
		Repositories []map[string]any `json:"repositories"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || len(doc.Repositories) != 4 || !strings.HasPrefix(out, "{\n  \"repositories\": [\n    {\n      \"name\": \"demo-docker\",") {
		t.Fatalf("%d %q", code, out)
	}
	code, out, _ = run(t, "repos")
	lines := strings.Split(out, "\n")
	if code != 0 || !strings.HasPrefix(lines[0], "NAME           FORMAT   TYPE    ACCESS   PORT  FILES") ||
		!regexp.MustCompile(`^demo-docker\s+docker\s+hosted\s+docker\s+50100\s+1\s+5 B\s+2023-11-14 22:13$`).MatchString(lines[1]) ||
		!regexp.MustCompile(`^old-repo\s+unknown\s+hosted\s+files\s+-\s+1\s+2 B\s+2023-11-14 22:13\s+no definition$`).MatchString(lines[4]) ||
		!strings.Contains(lines[2], "members: demo-releases") {
		t.Fatalf("%q", out)
	}
	code, out, _ = run(t, "repos", "port", "demo-docker", "50200")
	if code != 0 || out != "Docker port of demo-docker set to 50200 in "+filepath.Join(dir, "config.json")+"\nthe change takes effect at the next start of the server\n" {
		t.Fatalf("%d %q", code, out)
	}
	code, out, _ = run(t, "config", "--docker-ports")
	if code != 0 || out != "50200 demo-docker\n" {
		t.Fatalf("%d %q", code, out)
	}
	code, out, _ = run(t, "repos", "port", "demo-docker", "none")
	if code != 0 || out != "removed the Docker port of demo-docker from "+filepath.Join(dir, "config.json")+"; the port of the definition applies: 50100\nthe change takes effect at the next start of the server\n" {
		t.Fatalf("%d %q", code, out)
	}
	for _, c := range []struct {
		args []string
		text string
	}{
		{[]string{"repos", "port", "demo-docker", "80"}, "error: Docker port 80 of demo-docker is outside 1024 to 65535; nothing changed\n"},
		{[]string{"repos", "port", "demo-releases", "50300"}, "error: repository demo-releases has format maven2; only Docker repositories take a port\n"},
		{[]string{"repos", "port", "nope", "50300"}, "error: no repository named nope; see: repos\n"},
		{[]string{"import-settings", filepath.Join(base, "nope.json")}, "error: settings file " + filepath.Join(base, "nope.json") + " does not exist\n"},
	} {
		code, out, errOut := run(t, c.args...)
		if code != 1 || out != "" || errOut != c.text {
			t.Errorf("%v: %d %q %q, want %q", c.args, code, out, errOut, c.text)
		}
	}
	bad := filepath.Join(base, "bad.json")
	os.WriteFile(bad, []byte("{not json"), 0o644)
	code, _, errOut := run(t, "import-settings", bad)
	if code != 1 || errOut != "error: "+bad+" is not valid JSON: Expecting property name enclosed in double quotes: line 1 column 2 (char 1)\n" {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestConfigCommand(t *testing.T) {
	dir := dataDir(t)
	base := filepath.Dir(dir)
	code, out, _ := run(t, "config", "--get", "port")
	if code != 0 || out != "8793\n" {
		t.Fatalf("%d %q", code, out)
	}
	t.Setenv(config.LegacyEnvPort, "14555")
	if _, out, _ := run(t, "config", "--get", "port"); out != "14555\n" {
		t.Fatalf("%q", out)
	}
	if _, out, _ := run(t, "config", "--get", "data_dir"); out != dir+"\n" {
		t.Fatalf("%q", out)
	}
	tree := demoTree(t, base)
	run(t, "store", "add", "tree", "--kind", "directory", "--root", tree)
	if code, out, _ := run(t, "config", "--check"); code != 0 || out != "" {
		t.Fatalf("%d %q", code, out)
	}
	code, out, _ = run(t, "config")
	var want strings.Builder
	for _, kv := range [][2]string{{"data_dir", dir}, {"bind", "127.0.0.1"}, {"port", "14555"},
		{"config_path", filepath.Join(dir, "config.json")}, {"index_path", filepath.Join(dir, "index.sqlite")},
		{"repositories_path", filepath.Join(dir, "repositories.json")}, {"stores", "1"}} {
		fmt.Fprintf(&want, "%-17s  %s\n", kv[0], kv[1])
	}
	want.WriteString("  tree  directory  " + tree + "\ndocker ports  0\n")
	if code != 0 || out != want.String() {
		t.Fatalf("%d\n%s\nwant\n%s", code, out, want.String())
	}
	code, out, _ = run(t, "config", "--json")
	if code != 0 || !strings.Contains(out, "\"port\": 14555,") || !strings.Contains(out, "\"docker_ports\": {}") {
		t.Fatalf("%q", out)
	}
	os.Rename(tree, filepath.Join(base, "moved"))
	code, out, _ = run(t, "config", "--check")
	if code != 1 || out != "store tree: root "+tree+" is missing\n" {
		t.Fatalf("%d %q", code, out)
	}
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o644)
	code, _, errOut := run(t, "store", "list")
	if code != 1 || !strings.Contains(errOut, "config.json") || strings.Contains(errOut, "Traceback") {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestDockerCheck(t *testing.T) {
	dir := dataDir(t)
	base := filepath.Dir(dir)
	code, _, errOut := run(t, "docker-check", "demo-docker")
	if code != 1 || errOut != "error: no index yet; attach a store and run: scan\n" {
		t.Fatalf("%d %q", code, errOut)
	}
	layer := []byte("layer one")
	cfgBlob := []byte(`{"architecture": "amd64"}`)
	missing := digestOf([]byte("not stored"))
	manifest := []byte(fmt.Sprintf(`{"schemaVersion": 2, "config": {"digest": %q}, "layers": [{"digest": %q}, {"digest": %q}]}`,
		digestOf(cfgBlob), digestOf(layer), missing))
	var blobs []testkit.BlobSpec
	for path, content := range map[string][]byte{
		"v2/-/blobs/" + digestOf(layer): layer, "v2/-/blobs/" + digestOf(cfgBlob): cfgBlob,
		"v2/demo/app/manifests/1.0": manifest, "v2/demo/app/manifests/" + digestOf(manifest): manifest,
	} {
		s := testkit.NewBlobSpec("demo-docker", path)
		s.Content = content
		blobs = append(blobs, s)
	}
	root, _ := testkit.MakeBlobStore(filepath.Join(base, "blobs"), blobs)
	run(t, "store", "add", "s1", "--kind", "nexus-file", "--root", root)
	run(t, "scan")
	code, out, _ := run(t, "docker-check", "demo-docker")
	want := "demo/app:1.0 missing " + missing + "\ndemo/app@" + digestOf(manifest) + " missing " + missing +
		"\ndemo-docker: 2 manifests read, 2 incomplete, 1 missing, 0 unreadable\n"
	if code != 1 || out != want {
		t.Fatalf("%d\n%s\nwant\n%s", code, out, want)
	}
	code, _, errOut = run(t, "docker-check", "nope")
	if code != 1 || errOut != "error: no repository named nope; see: repos\n" {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestServeRefusals(t *testing.T) {
	dataDir(t)
	for _, c := range []struct {
		args []string
		text string
	}{
		{[]string{"serve", "--port", "14556", "--bind", "0.0.0.0"},
			"error: bind address '0.0.0.0' is not local; only 127.0.0.1, localhost, ::1 are allowed: no authentication in phase 1\n"},
		{[]string{"serve", "--port", "0"}, "error: port 0 is outside 1 to 65535\n"},
		{[]string{"serve", "--port", "99999999999999999999"}, "error: port 99999999999999999999 is outside 1 to 65535\n"},
		{[]string{"serve", "--port", "8791"}, "error: port 8791 is reserved on this machine; use --port N\n"},
	} {
		code, out, errOut := run(t, c.args...)
		if code != 1 || out != "" || errOut != c.text {
			t.Errorf("%v: %d %q %q", c.args, code, out, errOut)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestServeAnswersAndStopsOnSignal(t *testing.T) {
	dataDir(t)
	port := freePort(t)
	r, w, _ := os.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- Main([]string{"serve", "--port", fmt.Sprint(port)}, os.Stdout, w)
		w.Close()
	}()
	lines := make(chan string, 100)
	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	url := fmt.Sprintf("http://127.0.0.1:%d/api/health", port)
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no answer")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("serve did not stop")
	}
	var log []string
	for line := range lines {
		log = append(log, line)
	}
	start := regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d,\d{3} INFO pipesmith_registry\.listeners: listening on 127\.0\.0\.1 port ` +
		fmt.Sprint(port) + `$`)
	access := regexp.MustCompile(`^INFO:     127\.0\.0\.1:\d+ - "GET /api/health HTTP/1\.1" 200 OK$`)
	if len(log) < 2 || !start.MatchString(log[0]) || !access.MatchString(log[len(log)-1]) {
		t.Fatalf("log %q", log)
	}
}
