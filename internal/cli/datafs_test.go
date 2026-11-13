package cli

import (
	"strings"
	"testing"
)

func onSharedFS(t *testing.T, kind string) {
	t.Helper()
	old := sharedFS
	sharedFS = func(string) string { return kind }
	t.Cleanup(func() { sharedFS = old })
}

func sharedFSWarning(dir string) string {
	return "warning: the data directory " + dir + " lies on a fakeowner file system; two processes that use the index" +
		" there at the same time can lose its writes. Run one process on it and start scans through the server" +
		" (POST /api/scan), or put the data directory on a local Linux file system such as a Docker volume.\n"
}

func TestScanWarnsOnASharedFileSystem(t *testing.T) {
	dir := dataDir(t)
	onSharedFS(t, "fakeowner")
	code, out, errOut := run(t, "scan")
	want := sharedFSWarning(dir) + "error: no stores attached; add one with: store add <name> --kind <kind> --root <path>\n"
	if code != 1 || out != "" || errOut != want {
		t.Fatalf("%d %q\n%q\nwant\n%q", code, out, errOut, want)
	}
}

func TestServeWarnsOnASharedFileSystem(t *testing.T) {
	dir := dataDir(t)
	onSharedFS(t, "fakeowner")
	code, _, errOut := run(t, "serve", "--port", "0")
	if want := sharedFSWarning(dir) + "error: port 0 is outside 1 to 65535\n"; code != 1 || errOut != want {
		t.Fatalf("%d\n%q\nwant\n%q", code, errOut, want)
	}
}

func TestNoWarningOnALocalFileSystem(t *testing.T) {
	dataDir(t)
	onSharedFS(t, "")
	for _, args := range [][]string{{"scan"}, {"serve", "--port", "0"}} {
		if _, _, errOut := run(t, args...); strings.Contains(errOut, "warning: the data directory") {
			t.Errorf("%v: %q", args, errOut)
		}
	}
}
