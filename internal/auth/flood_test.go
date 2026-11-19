package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/auth"
	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/model"
)

// liveConnection gives the errors of package ldapauth the meaning auth reads
// from them, as the adapter of package cli does.
type liveConnection struct{ d *ldapauth.Directory }

func (c liveConnection) Login(ctx context.Context, username, password string) (model.User, []string, error) {
	user, groups, err := c.d.Login(ctx, username, password)
	switch {
	case err == nil:
		return user, groups, nil
	case errors.Is(err, ldapauth.ErrInvalidCredentials):
		return model.User{}, nil, auth.ErrWrongPair
	case errors.Is(err, ldapauth.ErrUnavailable):
		return model.User{}, nil, fmt.Errorf("%w: %w", auth.ErrUnreachable, err)
	case errors.Is(err, ldapauth.ErrBusy):
		return model.User{}, nil, fmt.Errorf("%w: %w", auth.ErrBusy, err)
	}
	return model.User{}, nil, err
}

func (c liveConnection) Lookup(ctx context.Context, username string) (model.User, []string, error) {
	return model.User{}, nil, fmt.Errorf("demo connection: %w", auth.ErrNoUser)
}

// hangingDirectory listens on loopback and keeps every connection open and
// silent, as a directory that hangs. It counts the connections and the most
// that were open at the same time. A client gives a place back after it
// closed its connection, but the end of that connection can reach the
// listener after the connection of the next place; so at each accept it
// reads what every open connection holds and drops the ones whose client
// closed them, before it counts the new one.
type hangingDirectory struct {
	port        int
	mu          sync.Mutex
	live        []net.Conn
	total, peak int
}

func hang(t *testing.T) *hangingDirectory {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &hangingDirectory{port: ln.Addr().(*net.TCPAddr).Port}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			h.mu.Lock()
			open := h.live[:0]
			for _, l := range h.live {
				if closedByClient(l) {
					_ = l.Close()
				} else {
					open = append(open, l)
				}
			}
			h.live = append(open, c)
			h.total++
			h.peak = max(h.peak, len(h.live))
			h.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, c := range h.live {
			_ = c.Close()
		}
	})
	return h
}

// counts gives the connections so far and the most open at the same time.
func (h *hangingDirectory) counts() (total, peak int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total, h.peak
}

// closedByClient reads what a connection holds without waiting and reports
// whether its client has closed it; a read error counts as closed.
func closedByClient(c net.Conn) bool {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return false
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return false
	}
	closed := false
	buf := make([]byte, 4096)
	_ = raw.Read(func(fd uintptr) bool {
		for {
			n, _, err := syscall.Recvfrom(int(fd), buf, syscall.MSG_DONTWAIT)
			switch {
			case errors.Is(err, syscall.EINTR):
			case errors.Is(err, syscall.EAGAIN):
				return true
			case err != nil || n == 0:
				closed = true
				return true
			}
		}
	})
	return closed
}

// Finding T2 of review 3: Basic requests with distinct unknown names that
// wait for a place of an LDAP connection each held a pair of the limiter
// table for up to the connection timeout, without one failure, so a flood
// filled the table. At most MaxWaiting requests wait for a place of one
// connection; a request beyond them is answered at once, and as it compared
// nothing it keeps nothing on record. 500 such requests at once against a
// directory that hangs: never more than 16 connections open, the other 420
// requests answered with 429 while 80 are still in flight, 16 of them with a
// connection and 64 waiting for a place, and the table holds only their 80
// pairs. Once the connection timeout has ended them all, no goroutine of
// theirs and no pair in flight is left. TestLoginBeyondTheWaitersIsBusyAtOnce
// of package ldapauth counts the waiting sign ins themselves.
func TestFloodOfUnknownNamesHoldsAtMostEightyPairs(t *testing.T) {
	const n, timeout = 500, 2
	h := hang(t)
	d, err := ldapauth.New(model.LDAPServer{
		ID: "demo-directory-1", Name: "demo-directory", Order: 1, Protocol: "ldap", Host: "127.0.0.1", Port: h.port,
		SearchBase: "dc=example,dc=com", AuthScheme: "SIMPLE", AuthUsername: "uid=demo-bind,ou=services,dc=example,dc=com",
		ConnectionTimeoutSeconds: timeout, ConnectionRetryDelaySeconds: 300, MaxIncidentsCount: 0,
		UserBaseDN: "ou=people", UserSubtree: true, UserObjectClass: "inetOrgPerson", UserIDAttribute: "uid",
	}, "demo-bind-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	snap := fixture(t)
	snap.Realms = []string{auth.RealmLDAP}
	e := newEnv(t, snap, liveConnection{d})
	const address, inFlight = "192.0.2.81", ldapauth.MaxChecksInFlight + ldapauth.MaxWaiting
	baseline := runtime.NumGoroutine()
	out := make(chan int, n)
	start := make(chan struct{})
	for i := range n {
		go func() {
			<-start
			out <- codeOf(basicFrom(context.Background(), e, address, fmt.Sprintf("demo-unknown-%d", i), "demo-guess"))
		}()
	}
	close(start)
	began := time.Now()
	codes := map[int]int{}
	for answered := 0; answered < n-inFlight; answered++ {
		select {
		case code := <-out:
			codes[code]++
		case <-time.After(10 * time.Second):
			total, _ := h.counts()
			t.Fatalf("after 10 s: %d answers %v, %d connections", answered, codes, total)
		}
	}
	took := time.Since(began)
	if took >= timeout*time.Second || codes[http.StatusTooManyRequests] != n-inFlight {
		t.Errorf("%d requests answered after %v with %v, want %d with 429 before the timeout of %d s",
			n-inFlight, took, codes, n-inFlight, timeout)
	}
	waitUntil(t, "a connection for each place", func() bool {
		total, _ := h.counts()
		return total >= ldapauth.MaxChecksInFlight
	})
	connections, _ := h.counts()
	pairs, flying := auth.PairCount(e.svc), auth.PairsInFlight(e.svc)
	if connections != ldapauth.MaxChecksInFlight || flying-connections != ldapauth.MaxWaiting {
		t.Errorf("%d requests in flight, %d of them with a connection, while the others were answered; want %d and %d",
			flying, connections, inFlight, ldapauth.MaxChecksInFlight)
	}
	if pairs != flying {
		t.Errorf("the limiter table holds %d pairs while %d requests are in flight", pairs, flying)
	}
	deadline := time.After(4*timeout*time.Second + 5*time.Second)
	for range inFlight {
		select {
		case code := <-out:
			codes[code]++
		case <-deadline:
			t.Fatalf("not every request has its answer: %v", codes)
		}
	}
	if codes[-1] != 0 || codes[http.StatusOK] != 0 {
		t.Errorf("answers %v, want 401 and 429 only", codes)
	}
	if _, peak := h.counts(); peak > ldapauth.MaxChecksInFlight {
		t.Errorf("%d connections open at the same time, want at most %d", peak, ldapauth.MaxChecksInFlight)
	}
	if flying := auth.PairsInFlight(e.svc); flying != 0 {
		t.Errorf("%d pairs in flight after every request has its answer", flying)
	}
	waitUntil(t, "no goroutine of the requests left", func() bool { return runtime.NumGoroutine() <= baseline })
}
