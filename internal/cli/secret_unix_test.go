//go:build darwin || linux

package cli

import (
	"sync"
	"sync/atomic"
	"testing"
)

// Review N9: once a caught signal restored the terminal, the change of the
// prompt does not run, so a signal between the start of the watch and the
// change cannot leave echo off. Without a signal the change runs, and a
// restore after it turns echo on again.
func TestTerminalGuardOrdersChangeAndRestore(t *testing.T) {
	var echo atomic.Bool
	echoOff := func() error { echo.Store(false); return nil }
	echoOn := func() { echo.Store(true) }

	echo.Store(true)
	var first terminalGuard
	first.restore(echoOn)
	if changed, err := first.change(echoOff); changed || err != nil || !echo.Load() {
		t.Fatalf("change after a restore: changed %v, %v, echo %v", changed, err, echo.Load())
	}
	var plain terminalGuard
	if changed, err := plain.change(echoOff); !changed || err != nil || echo.Load() {
		t.Fatalf("change without a signal: changed %v, %v, echo %v", changed, err, echo.Load())
	}
	plain.restore(echoOn)
	if !echo.Load() {
		t.Fatal("echo is off after the restore")
	}
	// in either order, echo ends on
	for i := range 2000 {
		echo.Store(true)
		var g terminalGuard
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = g.change(echoOff) }()
		go func() { defer wg.Done(); g.restore(echoOn) }()
		wg.Wait()
		if !echo.Load() {
			t.Fatalf("round %d: echo is off after a change and a restore", i+1)
		}
	}
}
