package cli

import (
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// noGoroutineLeft waits up to 5 s until no more goroutines run than before
// and dumps the stacks of all goroutines when they do not come back.
func noGoroutineLeft(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Fatalf("%d goroutines, %d before:\n%s", runtime.NumGoroutine(), before, buf[:n])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// At the end of serve the manager of the log targets and the log files
// close and leave no goroutine (finding 7a-5): a second serve in the same
// process ends with as many goroutines as the first left, and the lock of
// the log directory is free again. The first serve takes the goroutines
// the runtime keeps for good, such as the one of os/signal.
func TestServeLeavesNoGoroutine(t *testing.T) {
	dir := importedFixture(t)
	noLogDirVariable(t)
	stub := newReceiver(t, "")
	if code, _, errOut := run(t, "log-target", "add", "demo-json", "--kind", "http-json", "--address", stub.srv.URL,
		"--flush-interval-ms", "100"); code != 0 {
		t.Fatalf("add: %s", errOut)
	}
	cycle := func(query string) {
		t.Helper()
		done, stderr, port := servingWith(t)
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health?demo=%s", port, query))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		waitUntil(t, "the request line at the target", func() bool { return strings.Contains(stub.bodies(), "demo="+query) })
		stopServing(t, done)
		stderr()
		http.DefaultClient.CloseIdleConnections()
	}
	cycle("one")
	before := runtime.NumGoroutine()
	cycle("two")
	noGoroutineLeft(t, before)
	logDir := filepath.Join(dir, "logs")
	lf := logs.Open(logs.Options{Dir: logDir})
	lf.Request(logs.Request{ID: "0000000000000001", Method: "GET", Path: "/after-serve", Status: 200})
	if err := lf.Close(); err != nil {
		t.Fatal(err)
	}
	if text := string(mustRead(t, filepath.Join(logDir, "requests.log"))); !strings.Contains(text, "/after-serve") {
		t.Fatal("the log directory is still locked after serve")
	}
}
