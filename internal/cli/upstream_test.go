package cli

import (
	"bufio"
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

// The switch upstream of serve (decision 5.1 of the phase 5 plan), tested as
// writes_test.go tests writes.

// upstreamNeeds is the start of the refusal of upstream without state.sqlite.
const upstreamNeeds = "upstream needs state.sqlite"

// upstream needs state.sqlite (decision P5-2 of the phase 5 session, as E4
// for writes): serve refuses to start with upstream on and no state, whether
// the option, the variable or config.json switches it on, and makes neither
// the write store nor catalog.sqlite.
func TestServeRefusesUpstreamWithoutState(t *testing.T) {
	dir := dataDir(t)
	port := freePort(t)
	t.Setenv(config.EnvWrites, "")
	for _, c := range []struct {
		args []string
		env  string
		file bool
	}{{[]string{"--upstream"}, "", false}, {nil, "on", false}, {nil, "", true}} {
		t.Setenv(config.EnvUpstream, c.env)
		if c.file {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"upstream": true}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		code, errOut := refusedServe(t, append([]string{"--port", fmt.Sprint(port)}, c.args...)...)
		if code != 1 || !strings.Contains(errOut, upstreamNeeds) || !strings.Contains(errOut, "upstream") {
			t.Fatalf("%v %q: %d %q", c.args, c.env, code, errOut)
		}
	}
	for _, name := range []string{"stores", "catalog.sqlite"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("a refused start made %s", name)
		}
	}
}

func TestServeRefusesAnUpstreamVariableItCannotRead(t *testing.T) {
	dataDir(t)
	t.Setenv(config.EnvWrites, "")
	t.Setenv(config.EnvUpstream, "maybe")
	code, errOut := refusedServe(t, "--port", fmt.Sprint(freePort(t)))
	if code != 1 || !strings.Contains(errOut, config.EnvUpstream) {
		t.Fatalf("%d %q", code, errOut)
	}
}

// serveLog runs serve with args in this process until it answers, stops it
// and returns its log.
func serveLog(t *testing.T, args ...string) []string {
	t.Helper()
	port := freePort(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		done <- Main(append([]string{"serve", "--port", fmt.Sprint(port)}, args...), os.Stdout, w)
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
	return <-lines
}

// lineAt is the position of the first line that matches, -1 for none.
func lineAt(lines []string, re *regexp.Regexp) int {
	for i, l := range lines {
		if re.MatchString(l) {
			return i
		}
	}
	return -1
}

// With state.sqlite and upstream on, serve makes the write store, opens
// catalog.sqlite and names upstream in one line of its start log, after the
// line of writes when both are on (decision P5-6). With writes off the line
// of writes stays out. config.json gets no key from the option.
func TestServeWithUpstreamOn(t *testing.T) {
	dir := importedFixture(t)
	t.Setenv(config.EnvWrites, "")
	t.Setenv(config.EnvUpstream, "")
	root := filepath.Join(dir, "stores", "local-write")
	prefix := `^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d,\d{3} INFO pipesmith_registry\.server: `
	upstreamLine := regexp.MustCompile(prefix + `upstream on: proxies ask their remotes for what they do not hold and store it in store local-write at ` +
		regexp.QuoteMeta(root) + `$`)
	writesLine := regexp.MustCompile(prefix + `writes on: `)
	log := serveLog(t, "--upstream")
	if lineAt(log, upstreamLine) < 0 || lineAt(log, writesLine) >= 0 {
		t.Fatalf("start log with upstream on and writes off: %q", log)
	}
	for _, name := range []string{"stores/local-write/metadata.properties", "catalog.sqlite"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil && strings.Contains(string(data), "upstream") {
		t.Fatalf("config.json %s", data)
	}
	log = serveLog(t, "--writes", "--upstream")
	if w, u := lineAt(log, writesLine), lineAt(log, upstreamLine); w < 0 || u != w+1 {
		t.Fatalf("start log with writes and upstream on: %q", log)
	}
	t.Setenv(config.EnvUpstream, "on")
	if log = serveLog(t); lineAt(log, upstreamLine) < 0 {
		t.Fatalf("start log with the variable on: %q", log)
	}
	t.Setenv(config.EnvUpstream, "off")
	if log = serveLog(t, "--upstream"); lineAt(log, upstreamLine) < 0 {
		t.Fatalf("the option loses to the variable: %q", log)
	}
	if log = serveLog(t); lineAt(log, upstreamLine) >= 0 {
		t.Fatalf("start log with the variable off: %q", log)
	}
}
