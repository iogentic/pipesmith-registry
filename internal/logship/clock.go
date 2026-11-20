package logship

import "time"

// clock is the time of a manager: the pauses between attempts, the flush
// interval, the reload and the rate of warnings. The attempt timeout and the
// 2 s of a stop are real time.
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// realClock is the clock of the machine.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
