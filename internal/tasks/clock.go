package tasks

import "time"

// Clock is the time of the scheduler: the current time, and timers that
// call a function once.
type Clock interface {
	Now() time.Time
	// AfterFunc calls f once d has passed; a d of zero or less is due now.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a timer of a Clock.
type Timer interface {
	// Stop keeps the function from being called. It is false when the timer
	// had fired or was stopped already.
	Stop() bool
}

// RealClock is the clock of the machine.
func RealClock() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
