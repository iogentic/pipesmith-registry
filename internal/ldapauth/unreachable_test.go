package ldapauth_test

// Ruling I25: a check tells a directory it could not reach from one that
// answered, but not how it failed to reach it.

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
)

// silentPort is a port of 127.0.0.1 that takes connections and, with
// hold, keeps them open without a word, else closes them at once.
func silentPort(t *testing.T, hold bool) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if !hold {
				_ = conn.Close()
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func refusedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func TestUnreachable(t *testing.T) {
	for name, port := range map[string]int{"refused": refusedPort(t), "closed at once": silentPort(t, false),
		"silent": silentPort(t, true)} {
		for _, protocol := range []string{"ldap", "ldaps"} {
			s := validServer()
			s.Port, s.Protocol, s.ConnectionTimeoutSeconds = port, protocol, 1
			d, err := ldapauth.New(s, fakeBind, ldapauth.WithCheckBounds())
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.Check(context.Background())
			_, _, uerr := d.Users(context.Background(), 20)
			_, _, lerr := d.Login(context.Background(), "demo-person-01", "x")
			for _, e := range []error{err, uerr, lerr} {
				if !errors.Is(e, ldapauth.ErrUnreachable) || !errors.Is(e, ldapauth.ErrUnavailable) {
					t.Errorf("%s %s: %v", name, protocol, e)
				}
			}
		}
	}
	// a directory that answers is reached, also when it refuses the bind
	fake := fakeDirectory(t)
	s := validServer()
	s.Port = fake.Port()
	d, err := ldapauth.New(s, "wrong-bind", ldapauth.WithCheckBounds())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Check(context.Background()); errors.Is(err, ldapauth.ErrUnreachable) || !errors.Is(err, ldapauth.ErrUnavailable) {
		t.Fatalf("a refused bind: %v", err)
	}
}
