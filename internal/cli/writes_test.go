package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/config"
)

// refusedServe runs a serve that must refuse to start. A serve that starts
// all the same is stopped after 10 s and fails the test, so that the test
// cannot hang.
func refusedServe(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- Main(append([]string{"serve"}, args...), &out, &errOut) }()
	select {
	case code := <-done:
		if out.Len() != 0 {
			t.Fatalf("serve printed %q", out.String())
		}
		return code, errOut.String()
	case <-time.After(10 * time.Second):
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		<-done
		t.Fatalf("serve %v started instead of refusing", args)
	}
	return 0, ""
}

// Writes need state.sqlite (decision E4): serve refuses to start with writes
// on and no state, whether the option, the variable or config.json switches
// them on, and makes no write store.
func TestServeRefusesWritesWithoutState(t *testing.T) {
	dir := dataDir(t)
	port := freePort(t)
	for _, c := range []struct {
		args []string
		env  string
		file bool
	}{{[]string{"--writes"}, "", false}, {nil, "on", false}, {nil, "", true}} {
		t.Setenv(config.EnvWrites, c.env)
		if c.file {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"writes": true}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		code, errOut := refusedServe(t, append([]string{"--port", fmt.Sprint(port)}, c.args...)...)
		if code != 1 || !strings.Contains(errOut, "writes need state.sqlite") {
			t.Fatalf("%v %q: %d %q", c.args, c.env, code, errOut)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "stores")); err == nil {
		t.Fatal("a refused start made the write store")
	}
}

func TestServeRefusesAWritesVariableItCannotRead(t *testing.T) {
	dataDir(t)
	t.Setenv(config.EnvWrites, "maybe")
	code, errOut := refusedServe(t, "--port", fmt.Sprint(freePort(t)))
	if code != 1 || !strings.Contains(errOut, config.EnvWrites) {
		t.Fatalf("%d %q", code, errOut)
	}
}

// With state.sqlite and writes on, serve makes the write store and names it
// in its start log; config.json gets no key from the option.
func TestServeWithWritesOn(t *testing.T) {
	dir := importedFixture(t)
	t.Setenv(config.EnvWrites, "")
	port := freePort(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		done <- Main([]string{"serve", "--writes", "--port", fmt.Sprint(port)}, os.Stdout, w)
		w.Close()
	}()
	lines := make(chan []string, 1)
	go func() {
		var all []string
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			all = append(all, scanner.Text())
		}
		lines <- all
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health", port))
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
	log := <-lines
	root := filepath.Join(dir, "stores", "local-write")
	line := regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d,\d{3} INFO pipesmith_registry\.server: writes on: new blobs go into store local-write at ` +
		regexp.QuoteMeta(root) + `$`)
	found := false
	for _, l := range log {
		found = found || line.MatchString(l)
	}
	if !found {
		t.Fatalf("no start line of the writes in %q", log)
	}
	if _, err := os.Stat(filepath.Join(root, "metadata.properties")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil && strings.Contains(string(data), "writes") {
		t.Fatalf("config.json %s", data)
	}
}
