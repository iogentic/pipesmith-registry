package ldapauth_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/ldapauth"
	"github.com/iogentic/pipesmith-registry/internal/model"
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

// The lookup of the user of a token runs under a context that no request
// ends (review R4 of the second security review), so the timeout of the
// connection alone bounds it: a silent server answers within about that
// timeout.
func TestLookupTimeout(t *testing.T) {
	port, count := listen(t, true)
	s := validServer()
	s.Port = port
	s.ConnectionTimeoutSeconds = 1
	d, err := ldapauth.New(s, canaryBindPassword)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, _, err = d.Lookup(context.WithoutCancel(context.Background()), "demo-ldap-reader")
	elapsed := time.Since(start)
	if !errors.Is(err, ldapauth.ErrUnavailable) {
		t.Fatalf("silent server: %v", err)
	}
	if elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("silent server answered after %v, timeout 1 s", elapsed)
	}
	if count.Load() != 1 || strings.Contains(err.Error(), canaryBindPassword) {
		t.Fatalf("%d connections, error %v", count.Load(), err)
	}
}

// waitFor polls cond until it holds, for at most 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("after 10 s still not: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// signIns starts a sign in for each name with ctx and gives the channel of
// their errors.
func signIns(ctx context.Context, d *ldapauth.Directory, names ...string) <-chan error {
	out := make(chan error, len(names))
	for _, name := range names {
		go func() {
			_, _, err := d.Login(ctx, name, "demo-guess")
			out <- err
		}()
	}
	return out
}

// unknownNames are n invented user names, numbered from first.
func unknownNames(first, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("demo-unknown-%d", first+i)
	}
	return out
}

// Finding A2 of the security assessment: every sign in with an unknown name
// opened a connection without an upper bound. At most MaxChecksInFlight sign
// ins run against a connection at a time. A sign in beyond them waits for a
// place without a connection of its own, and takes the first place that
// frees up. Before, it got ErrBusy at once, so a burst of distinct accounts
// after a start saw 429 for most of them.
func TestLoginWaitsForAPlace(t *testing.T) {
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
	first, stopFirst := context.WithCancel(ctx)
	firstDone := signIns(first, d, "demo-unknown-first")
	waitFor(t, "the first sign in connected", func() bool { return count.Load() == 1 })
	held := signIns(ctx, d, unknownNames(1, ldapauth.MaxChecksInFlight-1)...)
	waitFor(t, "every place taken", func() bool { return count.Load() == ldapauth.MaxChecksInFlight })
	surplus := signIns(ctx, d, "demo-unknown-surplus")
	waitFor(t, "the sign in beyond the places waits", func() bool { return ldapauth.Waiting(d) == 1 })
	time.Sleep(50 * time.Millisecond) // a dial of the waiting sign in would be accepted by now
	if n := count.Load(); n != ldapauth.MaxChecksInFlight {
		t.Errorf("%d connections while %d sign ins run and one waits, want %d", n, ldapauth.MaxChecksInFlight, ldapauth.MaxChecksInFlight)
	}
	stopFirst()
	if err := <-firstDone; !errors.Is(err, ldapauth.ErrUnavailable) || !errors.Is(err, context.Canceled) {
		t.Errorf("the sign in whose context ended: %v", err)
	}
	waitFor(t, "the waiting sign in takes the place given back",
		func() bool { return count.Load() == ldapauth.MaxChecksInFlight+1 && ldapauth.Waiting(d) == 0 })
	cancel()
	for range ldapauth.MaxChecksInFlight - 1 {
		if err := <-held; !errors.Is(err, ldapauth.ErrUnavailable) {
			t.Errorf("a sign in that held a place: %v", err)
		}
	}
	if err := <-surplus; errors.Is(err, ldapauth.ErrBusy) || !errors.Is(err, ldapauth.ErrUnavailable) {
		t.Errorf("the sign in that took the place given back: %v, want the end of its context", err)
	}
}

// The wait for a place ends at the connection timeout, the bound of each
// step of a check. Against a directory that hangs, 16 sign ins hold the
// places until the timeout ends their service bind, and 16 more wait for
// them and take them in order. The sign in after those waits one timeout,
// finds no place free and gets ErrBusy without a connection of its own.
// Once all sign ins have ended, no goroutine of theirs is left and the next
// sign in finds a place at once.
func TestLoginWaitsAtMostTheTimeoutForAPlace(t *testing.T) {
	port, count := listen(t, true)
	s := validServer()
	s.Port = port
	s.ConnectionTimeoutSeconds = 1
	s.MaxIncidentsCount = 0 // the timeouts of this test rest no connection
	d, err := ldapauth.New(s, canaryBindPassword)
	if err != nil {
		t.Fatal(err)
	}
	const n = ldapauth.MaxChecksInFlight
	baseline := runtime.NumGoroutine()
	ctx := context.Background()
	holders := signIns(ctx, d, unknownNames(0, n)...)
	waitFor(t, "every place taken", func() bool { return count.Load() == n })
	time.Sleep(300 * time.Millisecond) // the next sign ins wait well after the timed steps began
	queued := signIns(ctx, d, unknownNames(n, n)...)
	waitFor(t, "16 sign ins wait for a place", func() bool { return ldapauth.Waiting(d) == n })
	time.Sleep(50 * time.Millisecond) // they wait in this order
	start := time.Now()
	_, _, err = d.Login(ctx, "demo-unknown-surplus", "demo-guess")
	took := time.Since(start)
	if !errors.Is(err, ldapauth.ErrBusy) || errors.Is(err, ldapauth.ErrUnavailable) || took < 900*time.Millisecond || took > 1800*time.Millisecond {
		t.Errorf("sign in %d: %v after %v, want ErrBusy after the timeout of 1 s", 2*n+1, err, took)
	}
	for _, c := range []<-chan error{holders, queued} {
		for range n {
			if err := <-c; !errors.Is(err, ldapauth.ErrUnavailable) || errors.Is(err, ldapauth.ErrBusy) {
				t.Errorf("a sign in against the silent directory: %v, want its timeout", err)
			}
		}
	}
	if got := count.Load(); got != 2*n {
		t.Errorf("%d connections, want %d: the sign in that got ErrBusy opened none", got, 2*n)
	}
	waitFor(t, "no goroutine of the sign ins left", func() bool { return runtime.NumGoroutine() <= baseline })
	after, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	defer stop()
	if _, _, err := d.Login(after, "demo-unknown-after", "demo-guess"); errors.Is(err, ldapauth.ErrBusy) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a sign in once all have ended: %v, want the deadline of its context in the silent directory", err)
	}
}

// Finding T2 of review 3: the sign ins that wait for a place of one
// connection are bounded as well, at MaxWaiting. With every place taken and
// MaxWaiting sign ins waiting, the next sign in gets ErrBusy at once: it
// opens no connection and does not wait, and the waiting ones keep waiting.
// 500 sign ins with distinct unknown names at once against a directory that
// hangs: 16 hold a place, 64 wait for one, the waiting ones never number
// more, and the other 420 get ErrBusy at once without a connection.
func TestLoginBeyondTheWaitersIsBusyAtOnce(t *testing.T) {
	port, count := listen(t, true)
	s := validServer()
	s.Port = port
	s.ConnectionTimeoutSeconds = 30
	d, err := ldapauth.New(s, canaryBindPassword)
	if err != nil {
		t.Fatal(err)
	}
	const n, inFlight = 500, ldapauth.MaxChecksInFlight + ldapauth.MaxWaiting
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := signIns(ctx, d, unknownNames(0, n)...)
	var mostWaiting int64
	deadline := time.After(5 * time.Second)
	for answered := 0; answered < n-inFlight; {
		mostWaiting = max(mostWaiting, ldapauth.Waiting(d))
		select {
		case err := <-done:
			answered++
			if !errors.Is(err, ldapauth.ErrBusy) || errors.Is(err, ldapauth.ErrUnavailable) || ctx.Err() != nil {
				t.Errorf("sign in %d beyond the places and the waiting ones: %v, want ErrBusy at once", answered, err)
			}
		case <-time.After(time.Millisecond):
		case <-deadline:
			t.Fatalf("after 5 s %d of %d sign ins have their answer, %d wait", answered, n-inFlight, ldapauth.Waiting(d))
		}
	}
	waitFor(t, "every place taken", func() bool { return count.Load() >= ldapauth.MaxChecksInFlight })
	time.Sleep(50 * time.Millisecond) // a dial of a sign in that got ErrBusy would be accepted by now
	if c, w := count.Load(), ldapauth.Waiting(d); c != ldapauth.MaxChecksInFlight || w != ldapauth.MaxWaiting ||
		mostWaiting > ldapauth.MaxWaiting {
		t.Errorf("%d connections and %d waiting sign ins, at most %d waiting; want %d, %d and %d",
			c, w, mostWaiting, ldapauth.MaxChecksInFlight, ldapauth.MaxWaiting, ldapauth.MaxWaiting)
	}
	cancel()
	for range inFlight {
		if err := <-done; errors.Is(err, ldapauth.ErrBusy) && !errors.Is(err, context.Canceled) {
			t.Errorf("a sign in that held or waited for a place: %v, want the end of its context", err)
		}
	}
}

// A sign in whose context ends while it waits for a place gets ErrBusy
// with the end of its context: it opened no connection, counts no incident
// and waits no longer.
func TestLoginWaitStopsWithContext(t *testing.T) {
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
	held := signIns(ctx, d, unknownNames(0, ldapauth.MaxChecksInFlight)...)
	waitFor(t, "every place taken", func() bool { return count.Load() == ldapauth.MaxChecksInFlight })
	short, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer stop()
	start := time.Now()
	_, _, err = d.Login(short, "demo-unknown-surplus", "demo-guess")
	took := time.Since(start)
	if !errors.Is(err, ldapauth.ErrBusy) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ldapauth.ErrUnavailable) ||
		took < 250*time.Millisecond || took > 2*time.Second {
		t.Errorf("a sign in whose context ends while it waits: %v after %v, want ErrBusy after 300 ms", err, took)
	}
	if w := ldapauth.Waiting(d); w != 0 {
		t.Errorf("%d sign ins still wait", w)
	}
	time.Sleep(50 * time.Millisecond) // a dial of the sign in would be accepted by now
	if n := count.Load(); n != ldapauth.MaxChecksInFlight {
		t.Errorf("%d connections, want %d", n, ldapauth.MaxChecksInFlight)
	}
	cancel()
	for range ldapauth.MaxChecksInFlight {
		<-held
	}
}

// listenDroppable is listen with hold, and drop closes every connection held
// so far from the side of the directory.
func listenDroppable(t *testing.T) (int, *atomic.Int64, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		count atomic.Int64
		mu    sync.Mutex
		held  []net.Conn
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
			count.Add(1)
		}
	}()
	drop := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
		held = nil
	}
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		drop()
	})
	return ln.Addr().(*net.TCPAddr).Port, &count, drop
}

// Sign ins whose request ends while their service bind waits, and whose
// connections the directory closes at about the same moment, end without a
// panic. The stop of the request closed the connection that open held in
// its result value, from a goroutine of the context, and the return of open
// after the failed bind wrote nil into that value: when the goroutine read
// it after that, Close of a nil connection ended the process.
func TestSignInsWhoseRequestAndConnectionEndTogether(t *testing.T) {
	for round := range 10 {
		port, count, drop := listenDroppable(t)
		s := validServer()
		s.Port = port
		s.ConnectionTimeoutSeconds = 30
		s.MaxIncidentsCount = 0
		d, err := ldapauth.New(s, canaryBindPassword)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := signIns(ctx, d, unknownNames(round*ldapauth.MaxChecksInFlight, ldapauth.MaxChecksInFlight)...)
		waitFor(t, "every sign in connected", func() bool { return count.Load() == ldapauth.MaxChecksInFlight })
		time.Sleep(20 * time.Millisecond) // the service binds are sent by now
		cancel()
		drop()
		for range ldapauth.MaxChecksInFlight {
			if err := <-done; !errors.Is(err, ldapauth.ErrUnavailable) {
				t.Errorf("round %d: a sign in whose request and connection ended: %v", round+1, err)
			}
		}
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
