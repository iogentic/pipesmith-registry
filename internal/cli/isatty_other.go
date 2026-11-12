//go:build !darwin && !linux

package cli

import "os"

func isTerminal(*os.File) bool { return false }
