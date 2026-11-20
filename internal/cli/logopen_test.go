package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/config"
)

// takenPort is a port that a listener of the test holds until the test ends.
func takenPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

// failedServe runs serve on a taken port and checks that it ends with the
// listen error.
func failedServe(t *testing.T, port int) {
	t.Helper()
	code, out, errOut := run(t, "serve", "--port", fmt.Sprint(port))
	if code != 1 || out != "" || !strings.Contains(errOut, fmt.Sprintf("cannot listen on 127.0.0.1 port %d: ", port)) {
		t.Fatalf("serve on a taken port: exit %d, %q, %q", code, out, errOut)
	}
}

// A serve that fails in server.Serve, here on a port that a listener of the
// test holds, leaves the disk as it was: a data directory that does not exist
// is not made, and one with its config.json keeps its listing. The same data
// directory with a free port gets the five log files.
func TestFailedServeOpensNoLogFiles(t *testing.T) {
	noLogDirVariable(t)
	port := takenPort(t)
	absent := dataDir(t)
	failedServe(t, port)
	for _, path := range []string{absent, filepath.Join(absent, "logs")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after the failed serve: %v, data %v, logs %v", path, err, namesIn(t, absent), namesIn(t, filepath.Join(absent, "logs")))
		}
	}
	existing := dataDir(t)
	if err := config.Save(config.New(existing)); err != nil {
		t.Fatal(err)
	}
	before := namesIn(t, existing)
	if !slices.Equal(before, []string{"config.json"}) {
		t.Fatalf("the data directory before serve: %v", before)
	}
	failedServe(t, port)
	if after := namesIn(t, existing); !slices.Equal(after, before) {
		t.Errorf("the data directory held %v and holds %v after the failed serve, logs %v", before, after, namesIn(t, filepath.Join(existing, "logs")))
	}
	done, _, _ := servingWith(t)
	stopServing(t, done)
	if names := namesIn(t, filepath.Join(existing, "logs")); !slices.Equal(names, logFileNames) {
		t.Fatalf("serve on a free port: logs %v", names)
	}
}
