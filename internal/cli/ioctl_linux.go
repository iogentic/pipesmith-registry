package cli

import "syscall"

const (
	ioctlReadTermios  = syscall.TCGETS
	ioctlWriteTermios = syscall.TCSETS
	// ioctlFlushTermios is TCSETSF, which package syscall does not name: it
	// sets the attributes after it drops the input not read yet, as
	// tcsetattr with TCSAFLUSH. It is TCSETS plus 2 in the asm/ioctls.h of
	// every architecture of linux-libc-dev in the build image (0x5402 and
	// 0x5404 in asm-generic, read 2026-11-12).
	ioctlFlushTermios = syscall.TCSETS + 2
	// ioctlFlushQueue is TCFLSH. Package syscall of Go 1.27.4 names it for
	// linux/arm64 but not for linux/amd64; both take 0x540b from the
	// asm-generic ioctls.h, as zerrors_linux_amd64.go and
	// zerrors_linux_arm64.go of golang.org/x/sys v0.50.0 have it (read
	// 2026-11-18).
	ioctlFlushQueue = 0x540b
)

// dropQueuedInput drops the keys the tty layer still holds for the line
// discipline. TCSETSF drops only what the line discipline holds (set_termios
// in drivers/tty/tty_ioctl.c); TCFLSH with TCIFLUSH flushes the buffer of the
// tty layer as well (tty_ioctl in drivers/tty/tty_io.c, read 2026-11-18).
func dropQueuedInput(fd uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlFlushQueue, syscall.TCIFLUSH); errno != 0 {
		return errno
	}
	return nil
}
