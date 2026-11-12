package ldapauth_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/ldapauth"
	"github.com/mirosso25/pipesmith-registry/internal/model"
)

// The identity code calls exactly this method.
type loginer interface {
	Login(ctx context.Context, username, password string) (model.User, []string, error)
}

var (
	_ loginer = (*ldapauth.Directory)(nil)
	_ loginer = (*ldapauth.Chain)(nil)
)

const canaryBindPassword = "canary-bind-7f3a9"

func validServer() model.LDAPServer {
	return model.LDAPServer{
		ID: "demo-directory-1", Name: "demo-directory", Order: 1, Protocol: "ldap", Host: "127.0.0.1", Port: 389,
		SearchBase: "dc=example,dc=com", AuthScheme: "SIMPLE", AuthUsername: "uid=demo-bind,ou=services,dc=example,dc=com",
		ConnectionTimeoutSeconds: 2, ConnectionRetryDelaySeconds: 300, MaxIncidentsCount: 3,
		UserBaseDN: "ou=people", UserSubtree: true, UserObjectClass: "inetOrgPerson", UserIDAttribute: "uid",
		UserRealNameAttribute: "cn", UserEmailAddressAttribute: "mail", LDAPGroupsAsRoles: true, GroupType: "static",
		GroupBaseDN: "ou=groups", GroupSubtree: true, GroupObjectClass: "groupOfNames", GroupIDAttribute: "cn",
		GroupMemberAttribute: "member", GroupMemberFormat: "${dn}",
	}
}

func TestNewAcceptsStoredSpellings(t *testing.T) {
	cases := []func(*model.LDAPServer){
		func(s *model.LDAPServer) {},
		func(s *model.LDAPServer) { s.AuthScheme = "simple" },
		func(s *model.LDAPServer) { s.AuthScheme = "none"; s.AuthUsername = "" },
		func(s *model.LDAPServer) { s.AuthScheme = "NONE"; s.AuthUsername = "" },
		func(s *model.LDAPServer) { s.Protocol = "LDAPS"; s.Port = 636 },
		func(s *model.LDAPServer) { s.GroupType = "Static" },
		func(s *model.LDAPServer) { s.GroupType = "dynamic"; s.UserMemberOfAttribute = "memberOf" },
		func(s *model.LDAPServer) { s.LDAPGroupsAsRoles = false; s.GroupType = ""; s.GroupMemberFormat = "" },
		func(s *model.LDAPServer) { s.UserLDAPFilter = "mail=*@example.com" },
		func(s *model.LDAPServer) { s.UserObjectClass = "" },
		func(s *model.LDAPServer) { s.GroupMemberFormat = "uid=${username},ou=people,dc=example,dc=com" },
		func(s *model.LDAPServer) { s.ConnectionTimeoutSeconds = 0; s.ConnectionRetryDelaySeconds = 0 },
	}
	for i, change := range cases {
		s := validServer()
		change(&s)
		if _, err := ldapauth.New(s, canaryBindPassword); err != nil {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestNewRefusesSettings(t *testing.T) {
	cases := map[string]func(*model.LDAPServer, *string){
		"DIGEST-MD5 waits for phase 3": func(s *model.LDAPServer, _ *string) { s.AuthScheme = "DIGEST_MD5" },
		"CRAM-MD5 waits for phase 3":   func(s *model.LDAPServer, _ *string) { s.AuthScheme = "CRAM-MD5" },
		"empty scheme":                 func(s *model.LDAPServer, _ *string) { s.AuthScheme = "" },
		"protocol":                     func(s *model.LDAPServer, _ *string) { s.Protocol = "ftp" },
		"host":                         func(s *model.LDAPServer, _ *string) { s.Host = "" },
		"port 0":                       func(s *model.LDAPServer, _ *string) { s.Port = 0 },
		"port 70000":                   func(s *model.LDAPServer, _ *string) { s.Port = 70000 },
		"simple without password":      func(_ *model.LDAPServer, p *string) { *p = "" },
		"simple without bind name":     func(s *model.LDAPServer, _ *string) { s.AuthUsername = "" },
		"search base":                  func(s *model.LDAPServer, _ *string) { s.SearchBase = "not a dn" },
		"user base":                    func(s *model.LDAPServer, _ *string) { s.UserBaseDN = "people" },
		"group base":                   func(s *model.LDAPServer, _ *string) { s.GroupBaseDN = "groups" },
		"no id attribute":              func(s *model.LDAPServer, _ *string) { s.UserIDAttribute = "" },
		"id attribute":                 func(s *model.LDAPServer, _ *string) { s.UserIDAttribute = "uid)(x" },
		"object class":                 func(s *model.LDAPServer, _ *string) { s.UserObjectClass = "a*" },
		"user filter":                  func(s *model.LDAPServer, _ *string) { s.UserLDAPFilter = "(uid=" },
		"group type":                   func(s *model.LDAPServer, _ *string) { s.GroupType = "nested" },
		"no member attribute":          func(s *model.LDAPServer, _ *string) { s.GroupMemberAttribute = "" },
		"no group id attribute":        func(s *model.LDAPServer, _ *string) { s.GroupIDAttribute = "" },
		"format without placeholder":   func(s *model.LDAPServer, _ *string) { s.GroupMemberFormat = "cn=everyone" },
		"dynamic without attribute": func(s *model.LDAPServer, _ *string) {
			s.GroupType = "dynamic"
			s.UserMemberOfAttribute = ""
		},
	}
	for name, change := range cases {
		s := validServer()
		password := canaryBindPassword
		change(&s, &password)
		d, err := ldapauth.New(s, password)
		if !errors.Is(err, ldapauth.ErrConfiguration) {
			t.Errorf("%s: New gave %v, %v", name, d, err)
			continue
		}
		if strings.Contains(err.Error(), canaryBindPassword) {
			t.Errorf("%s: the error holds the bind password", name)
		}
	}
	if _, err := ldapauth.New(validServer(), canaryBindPassword, ldapauth.WithCACertificates([]byte("no pem"))); !errors.Is(err, ldapauth.ErrConfiguration) {
		t.Errorf("a CA option without a certificate: %v", err)
	}
}

func TestStringHidesBindPassword(t *testing.T) {
	d, err := ldapauth.New(validServer(), canaryBindPassword)
	if err != nil {
		t.Fatal(err)
	}
	chain := ldapauth.NewChain(d)
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x", "%q", "%10.3v"} {
		for _, value := range []any{d, *d, chain, *chain, []*ldapauth.Directory{d}, map[string]any{"d": d}} {
			if out := fmt.Sprintf(format, value); strings.Contains(out, canaryBindPassword) {
				t.Fatalf("%s of %T prints the bind password", format, value)
			}
		}
	}
}

// listen returns a loopback listener whose connections are counted. hold
// keeps accepted connections open and silent; otherwise each one gets one
// byte and is closed, so a probe that reads the byte knows the count is
// current.
func listen(t *testing.T, hold bool) (int, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var count atomic.Int64
	var held []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			count.Add(1)
			if hold {
				held = append(held, c)
			} else {
				_, _ = c.Write([]byte{0})
				_ = c.Close()
			}
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		for _, c := range held {
			_ = c.Close()
		}
	})
	return ln.Addr().(*net.TCPAddr).Port, &count
}

func TestLoginRefusesWithoutConnecting(t *testing.T) {
	port, count := listen(t, false)
	s := validServer()
	s.Port = port
	d, err := ldapauth.New(s, canaryBindPassword)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ username, password string }{
		{"demo-ldap-reader", ""},
		{"", "reader-pass"},
		{strings.Repeat("u", 257), "reader-pass"},
		{"demo-ldap-reader", strings.Repeat("p", 1025)},
		{"bad\xffutf8", "reader-pass"},
	}
	for _, c := range cases {
		user, groups, err := d.Login(context.Background(), c.username, c.password)
		if err != ldapauth.ErrInvalidCredentials || user.ID != "" || groups != nil {
			t.Errorf("Login(%q, %d bytes) = %v, %v, %v", c.username, len(c.password), user, groups, err)
		}
	}
	// Login dials before it returns, so a connection of it is accepted before
	// the probe, which reads its byte only after its own accept.
	probe, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	_ = probe.Close()
	if got := count.Load(); got != 1 {
		t.Fatalf("refused sign ins opened %d connections", got-1)
	}
}

func TestLoginUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	s := validServer()
	s.Port = port
	d, err := ldapauth.New(s, canaryBindPassword)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, _, err = d.Login(context.Background(), "demo-ldap-reader", "user-pass-canary")
	if !errors.Is(err, ldapauth.ErrUnavailable) || errors.Is(err, ldapauth.ErrInvalidCredentials) {
		t.Fatalf("closed port: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("closed port took %v", elapsed)
	}
	for _, secret := range []string{canaryBindPassword, "user-pass-canary"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("the error holds a password")
		}
	}
}

func TestLoginTimeout(t *testing.T) {
	port, count := listen(t, true)
	s := validServer()
	s.Port = port
	s.ConnectionTimeoutSeconds = 1
	d, err := ldapauth.New(s, canaryBindPassword)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, _, err = d.Login(context.Background(), "demo-ldap-reader", "user-pass-canary")
	elapsed := time.Since(start)
	if !errors.Is(err, ldapauth.ErrUnavailable) {
		t.Fatalf("silent server: %v", err)
	}
	if elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("silent server answered after %v, timeout 1 s", elapsed)
	}
	if count.Load() != 1 {
		t.Fatalf("%d connections", count.Load())
	}
	if strings.Contains(err.Error(), canaryBindPassword) || strings.Contains(err.Error(), "user-pass-canary") {
		t.Fatal("the error holds a password")
	}
}

func TestLoginStopsWithContext(t *testing.T) {
	port, _ := listen(t, true)
	s := validServer()
	s.Port = port
	s.ConnectionTimeoutSeconds = 30
	d, err := ldapauth.New(s, canaryBindPassword)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = d.Login(ctx, "demo-ldap-reader", "user-pass-canary")
	if !errors.Is(err, ldapauth.ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the context deadline was not kept: %v", elapsed)
	}
}

func TestEmptyChainRefuses(t *testing.T) {
	_, _, err := ldapauth.NewChain().Login(context.Background(), "demo-ldap-reader", "x")
	if err != ldapauth.ErrInvalidCredentials {
		t.Fatalf("empty chain: %v", err)
	}
}

func TestChainUnavailableWhenNoneAnswers(t *testing.T) {
	var dirs []*ldapauth.Directory
	for i := 0; i < 2; i++ {
		port, _ := listen(t, false)
		s := validServer()
		s.ID = fmt.Sprintf("demo-%d", i)
		s.Port = port
		d, err := ldapauth.New(s, canaryBindPassword)
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, d)
	}
	_, _, err := ldapauth.NewChain(dirs...).Login(context.Background(), "demo-ldap-reader", "x")
	if !errors.Is(err, ldapauth.ErrUnavailable) || errors.Is(err, ldapauth.ErrInvalidCredentials) {
		t.Fatalf("no directory answered: %v", err)
	}
}
