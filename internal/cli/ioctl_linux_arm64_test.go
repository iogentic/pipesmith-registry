package cli

import (
	"syscall"
	"testing"
)

// The number of TCFLSH that ioctl_linux.go writes for every architecture
// equals the one package syscall names where it names one.
func TestFlushQueueIsTCFLSH(t *testing.T) {
	if ioctlFlushQueue != syscall.TCFLSH {
		t.Fatalf("ioctlFlushQueue is %#x, syscall.TCFLSH is %#x", ioctlFlushQueue, syscall.TCFLSH)
	}
}
