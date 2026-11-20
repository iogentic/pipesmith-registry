//go:build !darwin && !linux

package logs

// noFollow is not built for this system: a link is followed.
const noFollow = 0
