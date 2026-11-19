//go:build !(darwin || linux)

package migrate

import "errors"

// FreeSpace is not known on this platform.
func FreeSpace(string) (uint64, error) {
	return 0, errors.New("the free space is not known on this platform")
}
