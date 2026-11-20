package server

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// A log directory that cannot be used never fails a request (L6): with the
// directory below a regular file the server answers 200 with the file, the
// lines count as errors, the check of the log directory is red, the audit
// writer returns the failure and the server keeps serving. Once the
// directory can be made and 5 s of the clock passed, the next line lands in
// requests.log and the check is green.
func TestRequestAnswersWhileTheLogFails(t *testing.T) {
	const content = "jar bytes of the log failure test"
	a, _ := jarApp(t, content)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("a regular file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(file, "logs")
	var mu sync.Mutex
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	files := logs.Open(logs.Options{Dir: dir, Settings: logs.DefaultSettings(), Now: clock})
	t.Cleanup(func() { _ = files.Close() })
	recorded := make(chan struct{}, 8)
	a.srv.requests = func(r logs.Request) {
		files.Request(r)
		recorded <- struct{}{}
	}
	get := func() {
		t.Helper()
		got := a.get(jarURL)
		expect(t, got, 200)
		if string(got.body) != content {
			t.Fatalf("body %q", got.body)
		}
		<-recorded
	}
	get()
	files.Tick()
	if files.Errors() == 0 {
		t.Fatal("no error counted for the lines of the request")
	}
	if check := files.Health(); check.Healthy || check.Name != "log directory" || !strings.Contains(check.Detail, "requests.log: ") {
		t.Fatalf("check %+v", check)
	}
	if err := files.Audit(logs.AuditEvent{Action: "repository.create", Result: "ok"}); err == nil {
		t.Fatal("the audit writer did not report the failure")
	}
	get()
	files.Tick()
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0o750); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(5 * time.Second)
	mu.Unlock()
	counted := files.Errors()
	get()
	files.Tick()
	data, err := os.ReadFile(filepath.Join(dir, "requests.log"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"); len(lines) != 1 || !strings.Contains(lines[0], `"path":"`+jarURL+`"`) ||
		!strings.Contains(lines[0], `"status":200`) {
		t.Fatalf("requests.log %q", data)
	}
	if check := files.Health(); !check.Healthy || files.Errors() != counted {
		t.Fatalf("after the next success: %+v, %d errors, %d before", check, files.Errors(), counted)
	}
}
