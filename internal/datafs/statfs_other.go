//go:build !linux

package datafs

func statfsType(string) (int64, bool) { return 0, false }
