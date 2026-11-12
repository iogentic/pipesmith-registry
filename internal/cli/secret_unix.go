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
	"syscall"
	"time"
	"unsafe"
)

// promptSignals end the prompt as interrupted once the terminal is
// restored (review 6, finding 10).
var promptSignals = []syscall.Signal{syscall.SIGINT, syscall.SIGTERM}

// askSecret writes the prompt and reads one line from the terminal in with
// echo off. Keys typed before the prompt are dropped. The terminal settings
// are restored before it returns, and on SIGINT or SIGTERM during the prompt
// before the process ends as interrupted.
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
		stop := watchPrompt(restore, prompt)
		defer stop()
		quiet := old
		quiet.Lflag &^= syscall.ECHO
		if err := termios(fd, ioctlFlushTermios, &quiet); err != nil {
			return "", err
		}
		defer restore()
		_, _ = fmt.Fprint(prompt, text)
		line, err := bufio.NewReader(in).ReadString('\n')
		_, _ = fmt.Fprintln(prompt)
		if err != nil && line == "" {
			return "", fmt.Errorf("no answer at the terminal: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
}

// watchPrompt catches the signals of promptSignals that were not ignored at
// start, until stop is called. A caught signal restores the terminal, ends
// the prompt line and raises the signal again with its default action, so
// the process ends as the signal would have ended it without the prompt.
func watchPrompt(restore func(), prompt io.Writer) (stop func()) {
	var catch []os.Signal
	for _, sig := range promptSignals {
		if !signal.Ignored(sig) {
			catch = append(catch, sig)
		}
	}
	if len(catch) == 0 {
		return func() {} // Notify without signals would catch every signal
	}
	caught, done := make(chan os.Signal, 1), make(chan struct{})
	signal.Notify(caught, catch...)
	go func() {
		select {
		case sig := <-caught:
			restore()
			_, _ = fmt.Fprintln(prompt)
			s, _ := sig.(syscall.Signal) // os/signal delivers syscall.Signal on these systems
			raise(s)
		case <-done:
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

func termios(fd, request uintptr, attrs *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(attrs))); errno != 0 {
		return errno
	}
	return nil
}
