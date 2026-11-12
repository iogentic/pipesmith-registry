//go:build !darwin && !linux

package cli

func catchContinue() (stop func()) { return func() {} }
