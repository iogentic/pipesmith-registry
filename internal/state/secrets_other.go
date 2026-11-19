//go:build !darwin && !linux

package state

import (
	"errors"
	"os"
)

// Secret files are read on darwin and linux only, where the owner of a file
// can be checked.

func ownerOf(os.FileInfo) (int, bool) { return -1, false }

func openNoFollow(string) (*os.File, error) {
	return nil, errors.New("secret files are read on darwin and linux only")
}

func isLoop(error) bool { return false }
