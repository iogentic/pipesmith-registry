package logship

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/logs"
)

// scriptedAdapter answers every Send with the same result and error.
type scriptedAdapter struct {
	res   Result
	err   error
	sends int
}

func (a *scriptedAdapter) Send(context.Context, []logs.Line) (Result, error) {
	a.sends++
	return a.res, a.err
}

func (a *scriptedAdapter) Close() error { return nil }

// A failed attempt whose Retry is empty but not nil leaves the whole batch
// pending, as a nil Retry does: no line of it counts as sent.
func TestFailureWithEmptyRetrySendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		final bool
		sends int
	}{{"final", true, 1}, {"retryable", false, maxAttempts}} {
		t.Run(tc.name, func(t *testing.T) {
			a := &scriptedAdapter{res: Result{Retry: []logs.Line{}}, err: &Failure{Class: "status 400", Final: tc.final}}
			clk := newFakeClock()
			s := newSender(Target{Name: "demo", Kind: kindHTTPJSON}, "", a, clk, slog.New(slog.NewJSONHandler(io.Discard, nil)))
			defer s.cancel()
			done := make(chan struct{})
			go func() { defer close(done); s.deliver(testLines("server", 0, 7)) }()
			if !tc.final {
				clk.waitPending(t, pauses[0])
				clk.Advance(pauses[0])
				clk.waitPending(t, pauses[1])
				clk.Advance(pauses[1])
			}
			<-done
			st := s.stats()
			if st.Sent != 0 || st.Failed != 7 || a.sends != tc.sends {
				t.Fatalf("sent %d, failed %d, sends %d; want sent 0, failed 7, sends %d", st.Sent, st.Failed, a.sends, tc.sends)
			}
		})
	}
}
