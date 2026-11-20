package proxy

import (
	"errors"
	"io"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// Auto block (decision 5.3 of the phase 5 plan): three failures in a row (a
// connection error, a timeout, a body the remote broke off, a 5xx) block the
// remote of a proxy for 60 s. The first request after the wait is the probe,
// and no other request goes out while it runs. A failed probe doubles the
// wait up to 3,600 s; a probe the remote answers sets the proxy ready. An
// answer below 500 is never a failure and ends a row of failures. The state
// lives in memory, so a restart starts ready.

// The numbers of decision 5.3.
const (
	AutoBlockFailures  = 3
	AutoBlockFirstWait = time.Minute
	AutoBlockMaxWait   = time.Hour
)

// autoBlock is the auto block state of one remote.
type autoBlock struct {
	failures int           // failures in a row
	until    time.Time     // the end of the wait; zero while ready
	wait     time.Duration // the wait that ends at until
	probing  bool          // the probe is in flight
}

// verdict is what one request says about its remote.
type verdict int

const (
	unjudged verdict = iota // no request reached the remote
	answered                // the remote answered below 500
	failure                 // the remote failed
)

// judge is the verdict of an error of Do: a request the client refused
// before it went out says nothing, a redirect or a length the client refused
// is an answer, everything else is a failure.
func judge(err error) verdict {
	switch {
	case errors.Is(err, ErrPath):
		return unjudged
	case errors.Is(err, ErrRedirects), errors.Is(err, ErrRedirectScheme), errors.Is(err, ErrRedirectUser), errors.Is(err, ErrBodyTooLarge),
		errors.Is(err, ErrInnerAddress):
		return answered
	}
	return failure
}

// watched is the body of an answer of the remote; it keeps the error that
// broke a read off, which tells a failed remote from a refusal of the store.
type watched struct {
	r   io.Reader
	err error
}

func (w *watched) Read(b []byte) (int, error) {
	n, err := w.r.Read(b)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, ErrBodyTooLarge) {
		w.err = err
	}
	return n, err
}

func (p *Proxies) now() time.Time {
	if p.o.Now != nil {
		return p.o.Now()
	}
	return time.Now()
}

// waiting reports whether the remote of a proxy is auto blocked now: its
// wait runs, or its probe is in flight.
func (p *Proxies) waiting(r *remote, settings repos.ProxySettings) bool {
	if !settings.AutoBlock {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return !r.block.until.IsZero() && (r.block.probing || p.now().Before(r.block.until))
}

// admit lets one request go to the remote: every request while the remote
// is ready, and after a wait the first one, as the probe.
func (p *Proxies) admit(r *remote, settings repos.ProxySettings) (probe, ok bool) {
	if !settings.AutoBlock {
		return false, true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch b := &r.block; {
	case b.until.IsZero():
		return false, true
	case b.probing || p.now().Before(b.until):
		return false, false
	default:
		b.probing = true
		return true, true
	}
}

// report takes the verdict of a request into the auto block state of its
// remote.
func (p *Proxies) report(name string, r *remote, settings repos.ProxySettings, v verdict, probe bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := &r.block
	if probe {
		b.probing = false
	}
	if !settings.AutoBlock {
		*b = autoBlock{}
		return
	}
	switch v {
	case answered:
		if !b.until.IsZero() {
			p.info("proxy %s is ready again: the remote answered the probe", name)
		}
		*b = autoBlock{}
	case failure:
		b.failures++
		now := p.now()
		switch {
		case probe:
			b.wait = min(2*b.wait, AutoBlockMaxWait)
			b.until = now.Add(b.wait)
			p.warn("proxy %s stays auto blocked: the probe failed; the next probe after %s", name, b.wait)
		case b.until.IsZero() && b.failures >= AutoBlockFailures:
			b.wait = AutoBlockFirstWait
			b.until = now.Add(b.wait)
			p.warn("proxy %s is auto blocked after %d failures in a row; the next request after %s probes the remote", name,
				b.failures, b.wait)
		}
	}
}
