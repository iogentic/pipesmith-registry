package ldapauth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
)

// Review 3-54: lookups of the users of tokens hold places of their own, at
// most MaxChecksInFlight connections at a time; one beyond them waits
// without a connection, and sign ins keep their own places meanwhile.
func TestLookupsStayWithinABound(t *testing.T) {
	port, count := listen(t, true)
	s := validServer()
	s.Port = port
	s.ConnectionTimeoutSeconds = 30
	d, err := ldapauth.New(s, canaryBindPassword)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, ldapauth.MaxChecksInFlight+2)
	lookup := func(name string) {
		go func() {
			_, _, err := d.Lookup(ctx, name)
			done <- err
		}()
	}
	for _, name := range unknownNames(1, ldapauth.MaxChecksInFlight+1) {
		lookup(name)
	}
	waitFor(t, "the lookups beyond the bound wait", func() bool {
		return count.Load() == ldapauth.MaxChecksInFlight && ldapauth.LookupWaiting(d) == 1
	})
	signIn := signIns(ctx, d, "demo-unknown-sign-in")
	waitFor(t, "the sign in takes a place of its own", func() bool { return count.Load() == ldapauth.MaxChecksInFlight+1 })
	if n := ldapauth.LookupWaiting(d); n != 1 {
		t.Errorf("%d lookups wait, want 1", n)
	}
	cancel()
	for range ldapauth.MaxChecksInFlight + 1 {
		if err := <-done; !errors.Is(err, ldapauth.ErrUnavailable) {
			t.Errorf("a lookup whose context ended: %v", err)
		}
	}
	<-signIn
}
