package release

import (
	"os"
	"os/signal"
	"syscall"
)

// holdSignals keeps SIGINT, SIGHUP and SIGTERM from ending the process from
// the first record of a switch to the end of the command, so a Ctrl-C, a
// dropped SSH session or a stop of the service manager cannot leave a switch
// half done: the switch or its way back ends, then the command returns
// (finding R3-1, decision P8-30).
func (u *Updater) holdSignals(s *session) {
	if s.signals != nil {
		return
	}
	s.signals = make(chan os.Signal, 8)
	signal.Notify(s.signals, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
}

// releaseSignals ends the hold and names the signals that came during it.
func (u *Updater) releaseSignals(s *session) {
	if s.signals == nil {
		return
	}
	signal.Stop(s.signals)
	for {
		select {
		case sig := <-s.signals:
			u.step("received %s during the switch; the switch or its way back ran to its end first, now the command exits", sig)
		default:
			s.signals = nil
			return
		}
	}
}
