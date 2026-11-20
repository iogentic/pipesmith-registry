//go:build darwin || linux

package logs

import "syscall"

// noFollow makes the open of a log file fail on a link.
const noFollow = syscall.O_NOFOLLOW
