//go:build !darwin && !linux

package cli

import (
	"errors"
	"io"
	"os"
)

func askSecret(*os.File, io.Writer) askFunc {
	return func(string) (string, error) {
		return "", errors.New("--ask-ldap-password is not built for this system")
	}
}
