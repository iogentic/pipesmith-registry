package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
	"github.com/iogentic/pipesmith-registry/internal/server"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// The help texts argparse of Python 3.12 prints at 80 columns for the
// maven command.
var mavenHelp = map[string]string{
	"maven": "usage: pipesmith-registry maven [-h] {rebuild-metadata} ...\n\npositional arguments:\n  {rebuild-metadata}\n\n" +
		"options:\n  -h, --help          show this help message and exit\n",
	"maven rebuild-metadata": "usage: pipesmith-registry maven rebuild-metadata [-h] [--group GROUP]\n" +
		"                                                 [--artifact ARTIFACT]\n" +
		"                                                 [--version VERSION]\n" +
		"                                                 repository\n\npositional arguments:\n  repository\n\n" +
		"options:\n  -h, --help           show this help message and exit\n  --group GROUP\n  --artifact ARTIFACT\n  --version VERSION\n",
}

func TestMavenCommandHelp(t *testing.T) {
	dir := dataDir(t)
	for command, want := range mavenHelp {
		code, out, errOut := run(t, append(strings.Fields(command), "--help")...)
		if code != 0 || out != want || errOut != "" {
			t.Errorf("%s --help: %d\n%s\nwant\n%s", command, code, out, want)
		}
	}
	_, out, _ := run(t, "help")
	if !strings.Contains(out, "  maven rebuild-metadata <repository> [--group G] [--artifact A] [--version V]\n") {
		t.Errorf("help lacks the maven command:\n%s", out)
	}
	for args, want := range map[string]string{
		"maven":                  "error: pipesmith-registry maven: the following arguments are required: maven_command\n",
		"maven rebuild-metadata": "error: pipesmith-registry maven rebuild-metadata: the following arguments are required: repository\n",
	} {
		if code, _, errOut := run(t, strings.Fields(args)...); code != 2 || !strings.HasPrefix(errOut, want) {
			t.Errorf("%s: %d %q", args, code, errOut)
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("help created the data directory")
	}
}

func TestMavenRebuildNeedsState(t *testing.T) {
	dataDir(t)
	code, out, errOut := run(t, "maven", "rebuild-metadata", "demo-releases")
	if code != 1 || out != "" || !strings.Contains(errOut, "needs state.sqlite") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
}

// The command writes the metadata of the files of a repository into the
// write store, a second run writes nothing, stale metadata of a read only
// store stays, and a running writer of the store refuses the command.
func TestMavenRebuildMetadata(t *testing.T) {
	dir := importedFixture(t)
	base := filepath.Dir(dir)
	settings := filepath.Join(base, "settings.json")
	if err := os.WriteFile(settings, []byte(`[{"name": "demo-releases", "format": "maven2", "type": "hosted",
		"storage": {"writePolicy": "ALLOW_ONCE"}, "maven": {"versionPolicy": "RELEASE", "layoutPolicy": "STRICT"}}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, "import-state", "--settings", settings); code != 0 {
		t.Fatalf("import-state: %s", errOut)
	}
	var blobs []testkit.BlobSpec
	for _, p := range []string{"com/example/demo-lib/1.0.0/demo-lib-1.0.0.pom", "com/example/demo-lib/1.1.0/demo-lib-1.1.0.pom",
		"com/example/demo-gone/maven-metadata.xml"} {
		blobs = append(blobs, testkit.NewBlobSpec("demo-releases", p))
	}
	old, err := testkit.MakeBlobStore(filepath.Join(base, "old"), blobs)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"store", "add", "old", "--kind", "nexus-file", "--root", old}, {"scan"}} {
		if code, _, errOut := run(t, args...); code != 0 {
			t.Fatalf("%v: %s", args, errOut)
		}
	}
	before, err := testkit.TreeDigest(old)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run(t, "maven", "rebuild-metadata", "demo-releases")
	want := "rebuilt the metadata of repository demo-releases: 3 written, 0 unchanged, 0 removed\n" +
		"written com/example/demo-lib/maven-metadata.xml\nwritten com/example/demo-lib/maven-metadata.xml.md5\n" +
		"written com/example/demo-lib/maven-metadata.xml.sha1\n" +
		"kept com/example/demo-gone/maven-metadata.xml: stale, but a read only store holds it\n"
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("%d\n%s\nwant\n%s\n%q", code, out, want, errOut)
	}
	written, err := filepath.Glob(filepath.Join(dir, "stores", "local-write", "content", "*", "*", "*", "*", "*", "*.bytes"))
	if err != nil || len(written) != 3 {
		t.Fatalf("%v %v", written, err)
	}
	code, out, _ = run(t, "maven", "rebuild-metadata", "demo-releases", "--group", "com.example", "--artifact", "demo-lib")
	if code != 0 || out != "rebuilt the metadata of repository demo-releases: 0 written, 3 unchanged, 0 removed\n" {
		t.Fatalf("second run: %d %q", code, out)
	}
	if again, _ := filepath.Glob(filepath.Join(dir, "stores", "local-write", "content", "*", "*", "*", "*", "*", "*.bytes")); len(again) != 3 {
		t.Fatalf("the second run wrote: %v", again)
	}
	if after, _ := testkit.TreeDigest(old); after != before {
		t.Fatal("the read only store changed")
	}
	for _, args := range [][]string{{"maven", "rebuild-metadata", "missing"}, {"maven", "rebuild-metadata", "demo-releases", "--version", "1.0.0"}} {
		if code, _, errOut := run(t, args...); code != 1 || errOut == "" {
			t.Errorf("%v: %d %q", args, code, errOut)
		}
	}
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := buildSecurity(cfg.DataDir, nil, nil, logTo(&bytes.Buffer{}))
	if err != nil {
		t.Fatal(err)
	}
	defer closeSecurity(sec)
	srv, err := server.New(cfg, nil, server.Options{Security: sec, Writes: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if code, _, errOut := run(t, "maven", "rebuild-metadata", "demo-releases"); code != 1 || !strings.Contains(errOut, "has another writer") {
		t.Fatalf("with a writer on the store: %d %q", code, errOut)
	}
}
