//go:build darwin || linux

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// promptSignals end the prompt as interrupted once the terminal is
// restored (review 6, finding 10; SIGHUP and SIGQUIT review N9).
var promptSignals = []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

// askSecret writes the prompt and reads one line from the terminal in with
// echo off. Keys typed before the prompt are dropped. The terminal settings
// are restored before it returns, and on a signal of promptSignals during
// the prompt before the process ends by that signal. SIGTSTP during the
// prompt turns echo on before the process stops, and the prompt goes on
// with echo off after SIGCONT (review 3-18).
func askSecret(in *os.File, prompt io.Writer) askFunc {
	return func(text string) (string, error) {
		if !isTerminal(in) {
			return "", errors.New("--ask-ldap-password needs a terminal on standard input")
		}
		fd := in.Fd()
		var old syscall.Termios
		if err := termios(fd, ioctlReadTermios, &old); err != nil {
			return "", err
		}
		restore := func() { _ = termios(fd, ioctlWriteTermios, &old) } // nothing is left to do if the terminal refuses
		quiet := old
		quiet.Lflag &^= syscall.ECHO
		var guard terminalGuard
		pause := func(halt func()) {
			if guard.pause(restore, func() error { return termios(fd, ioctlWriteTermios, &quiet) }, func() {
				_, _ = fmt.Fprintln(prompt)
				halt()
			}) {
				_, _ = fmt.Fprint(prompt, text)
			}
		}
		stop := watchPrompt(func() { guard.restore(restore) }, pause, prompt)
		defer stop()
		changed, err := guard.change(func() error { return termios(fd, ioctlFlushTermios, &quiet) })
		if err != nil {
			return "", err
		}
		if !changed {
			select {} // a caught signal restored the terminal and ends the process
		}
		defer guard.restore(restore)
		if err := dropQueuedInput(fd); err != nil {
			return "", err
		}
		_, _ = fmt.Fprint(prompt, text)
		line, err := bufio.NewReader(in).ReadString('\n')
		_, _ = fmt.Fprintln(prompt)
		if err != nil && line == "" {
			return "", fmt.Errorf("no answer at the terminal: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
}

// terminalGuard orders the change of the terminal for the prompt and its
// restore by a caught signal (review N9): once a signal restored the
// terminal, the change does not run, so no signal between the start of the
// watch and the change leaves echo off.
type terminalGuard struct {
	mu      sync.Mutex
	ended   bool
	changed bool
}

// change runs f unless a signal restored the terminal first; then it reports
// false.
func (g *terminalGuard) change(f func() error) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ended {
		return false, nil
	}
	err := f()
	g.changed = err == nil
	return true, err
}

// pause runs off while the change holds, then halt, then on again unless a
// restore ran meanwhile; it reports whether the prompt goes on. The lock is
// not held during halt, so a signal that ends the process while it is
// stopped still restores the terminal.
func (g *terminalGuard) pause(off func(), on func() error, halt func()) bool {
	g.mu.Lock()
	held := g.changed && !g.ended
	if held {
		off()
	}
	g.mu.Unlock()
	halt()
	g.mu.Lock()
	defer g.mu.Unlock()
	if !held || g.ended {
		return false
	}
	_ = on() // a refusal leaves echo on
	return true
}

// restore runs f and keeps every later change from running.
func (g *terminalGuard) restore(f func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ended = true
	f()
}

// watchPrompt catches the signals of promptSignals that were not ignored at
// start, until stop is called. A caught signal restores the terminal, ends
// the prompt line and raises the signal again with its default action, so
// the process ends as the signal would have ended it without the prompt.
// A caught SIGTSTP runs pause with the function that stops the process; pause
// restores the terminal around it.
func watchPrompt(restore func(), pause func(halt func()), prompt io.Writer) (stop func()) {
	var catch []os.Signal
	for _, sig := range promptSignals {
		if !signal.Ignored(sig) {
			catch = append(catch, sig)
		}
	}
	if !signal.Ignored(syscall.SIGTSTP) {
		catch = append(catch, syscall.SIGTSTP)
	}
	if len(catch) == 0 {
		return func() {} // Notify without signals would catch every signal
	}
	caught, done := make(chan os.Signal, 1), make(chan struct{})
	signal.Notify(caught, catch...)
	go func() {
		for {
			select {
			case sig := <-caught:
				if sig == syscall.SIGTSTP {
					pause(stopProcess)
					continue
				}
				restore()
				_, _ = fmt.Fprintln(prompt)
				s, _ := sig.(syscall.Signal) // os/signal delivers syscall.Signal on these systems
				raise(s)
				return
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(caught)
		close(done)
	}
}

// raise ends the process by the signal, so a shell sees it as interrupted.
// The signal reaches the process on another thread; should it not end the
// process within raiseWait, 128 plus its number is the exit status.
func raise(sig syscall.Signal) {
	signal.Reset(sig)
	_ = syscall.Kill(syscall.Getpid(), sig) // the exit below covers a refusal
	time.Sleep(raiseWait)
	os.Exit(128 + int(sig))
}

const raiseWait = 5 * time.Second

// stopProcess stops the process by SIGSTOP and returns once SIGCONT
// arrives, or after stopWait. Once os/signal caught SIGTSTP, the runtime
// keeps its own handler for it, so a SIGTSTP raised again would not stop the
// process.
func stopProcess() {
	cont := make(chan os.Signal, 1)
	signal.Notify(cont, syscall.SIGCONT)
	defer signal.Stop(cont)
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGSTOP) // a refusal leaves the process running
	select {
	case <-cont:
	case <-time.After(stopWait):
	}
}

const stopWait = time.Second

func termios(fd, request uintptr, attrs *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(attrs))); errno != 0 {
		return errno
	}
	return nil
}
