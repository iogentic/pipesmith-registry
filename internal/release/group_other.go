//go:build !linux && !darwin

package release

import "os/exec"

// ownGroup does nothing where process groups are not set this way.
func ownGroup(*exec.Cmd) {}
