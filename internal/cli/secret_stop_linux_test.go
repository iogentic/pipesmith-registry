package cli

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// processState is the state letter of /proc/<pid>/stat: T when stopped.
func processState(t *testing.T, pid int) byte {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	end := bytes.LastIndexByte(data, ')')
	if end < 0 || end+2 >= len(data) {
		t.Fatalf("stat %q", data)
	}
	return data[end+2]
}

func waitState(t *testing.T, pid int, stopped bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for (processState(t, pid) == 'T') != stopped {
		if time.Now().After(deadline) {
			t.Fatalf("the child is not stopped=%v within 10 s", stopped)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Review 3-18: SIGTSTP during the prompt turns echo on before the process
// stops, and SIGCONT turns it off again for the rest of the prompt; the
// answer still arrives and echo is on at the end.
func TestPromptRestoresTheTerminalWhileStopped(t *testing.T) {
	master, slave := openPTY(t)
	_, _ = drain(master)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperPrompt$")
	cmd.Env = append(os.Environ(), promptChild+"=1")
	cmd.Stdin = slave
	// a process group of its own in the session of the test, so the group
	// is not orphaned and SIGTSTP stops it
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	prompts := make(chan struct{}, 4)
	go func() {
		r := bufio.NewReader(stderr)
		var seen []byte
		for {
			b, err := r.ReadByte()
			if err != nil {
				return
			}
			seen = append(seen, b)
			if bytes.HasSuffix(seen, []byte("bind password: ")) {
				prompts <- struct{}{}
				seen = seen[:0]
			}
		}
	}()
	waitPrompt := func(what string) {
		t.Helper()
		select {
		case <-prompts:
		case <-time.After(10 * time.Second):
			t.Fatalf("no %s within 10 s", what)
		}
	}
	waitPrompt("prompt")
	if echoOn(t, slave) {
		t.Fatal("echo is on during the prompt")
	}
	if err := cmd.Process.Signal(syscall.SIGTSTP); err != nil {
		t.Fatal(err)
	}
	waitState(t, cmd.Process.Pid, true)
	if !echoOn(t, slave) {
		t.Fatal("echo is off while the prompt is stopped by SIGTSTP")
	}
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	waitState(t, cmd.Process.Pid, false)
	waitPrompt("prompt after SIGCONT")
	if echoOn(t, slave) {
		t.Fatal("echo is on after SIGCONT, during the prompt")
	}
	if _, err := master.Write([]byte("s3cret-pw\n")); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err = <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("the prompt did not end within 10 s")
	}
	if err != nil || !strings.Contains(stdout.String(), `answer "s3cret-pw", error <nil>`) {
		t.Fatalf("the child ended with %v, stdout %q", err, stdout.String())
	}
	if !echoOn(t, slave) {
		t.Fatal("echo is still off after the answer")
	}
}
