//go:build !darwin && !linux

package release

import "errors"

var errPlatform = errors.New("update and rollback run on darwin and linux only")

func fileOwner(string) (int, error) { return 0, errPlatform }

func lstatFile(string) (fileStat, error) { return fileStat{}, errPlatform }

func lockRoot(string) (func(), error) { return nil, errPlatform }

func lockDir(string, error) (func(), error) { return nil, errPlatform }

func textBusy(error) bool { return false }

const openNonblock = 0
