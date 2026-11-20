package logs

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// noLeak waits up to 5 s until no more goroutines run than before and
// dumps the stacks of all goroutines when they do not come back.
func noLeak(t *testing.T, before int) {
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

// The ticker of Start and the goroutine of maint, which compresses what
// rotates, end with Close (finding 7a-5).
func TestFilesLeaveNoGoroutine(t *testing.T) {
	dir := t.TempDir()
	before := runtime.NumGoroutine()
	f := Open(Options{Dir: dir, Settings: Settings{MaxBytes: 1 << 10, Classic: true}})
	f.Start()
	if runtime.NumGoroutine() <= before {
		t.Fatal("Start runs no goroutine")
	}
	for i := 0; i < 200; i++ {
		f.Request(Request{ID: "0000000000000001", Method: "GET", Path: "/demo", Status: 200})
		if i%20 == 0 {
			f.Tick()
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	noLeak(t, before)
	gz := 0
	for _, name := range namesOf(t, dir) {
		if strings.HasSuffix(name, extGz) {
			gz++
		}
	}
	if gz == 0 {
		t.Fatalf("nothing rotated and compressed: %v", namesOf(t, dir))
	}
}
