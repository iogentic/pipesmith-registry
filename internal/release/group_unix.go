//go:build linux || darwin

package release

import (
	"os/exec"
	"syscall"
)

// ownGroup runs cmd in a process group of its own, so a signal of the
// terminal to the group of update does not reach systemctl or the release
// binary while update holds it.
func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
