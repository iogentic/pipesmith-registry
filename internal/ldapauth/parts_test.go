package ldapauth

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

func TestPasswordVariable(t *testing.T) {
	cases := map[string]string{
		"6f1c2a52-0000-4000-8000-000000000001": "PIPESMITH_REGISTRY_LDAP_PASSWORD_6F1C2A52_0000_4000_8000_000000000001",
		"demo.dir/ü":                           "PIPESMITH_REGISTRY_LDAP_PASSWORD_DEMO_DIR__",
		"Ab9":                                  "PIPESMITH_REGISTRY_LDAP_PASSWORD_AB9",
		"":                                     "PIPESMITH_REGISTRY_LDAP_PASSWORD_",
	}
	for id, want := range cases {
		if got := PasswordVariable(id); got != want {
			t.Errorf("PasswordVariable(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestPasswordFromEnv(t *testing.T) {
	t.Setenv(PasswordVariable("demo-1"), "x-y")
	if got, ok := PasswordFromEnv("demo-1"); !ok || got != "x-y" {
		t.Fatalf("set variable: got ok=%v", ok)
	}
	t.Setenv(PasswordVariable("demo-2"), "")
	if _, ok := PasswordFromEnv("demo-2"); ok {
		t.Fatal("an empty variable counts as set")
	}
	if _, ok := PasswordFromEnv("demo-unset-3"); ok {
		t.Fatal("an unset variable counts as set")
	}
}

func TestUserFilterEscapes(t *testing.T) {
	cases := []struct{ username, want string }{
		{"demo-ldap-reader", "(&(objectClass=inetOrgPerson)(uid=demo-ldap-reader))"},
		{"*)(uid=*", `(&(objectClass=inetOrgPerson)(uid=\2a\29\28uid=\2a))`},
		{"*", `(&(objectClass=inetOrgPerson)(uid=\2a))`},
		{`a\b`, `(&(objectClass=inetOrgPerson)(uid=a\5cb))`},
		{"ä", `(&(objectClass=inetOrgPerson)(uid=\c3\a4))`},
		{"nul\x00", `(&(objectClass=inetOrgPerson)(uid=nul\00))`},
	}
	for _, c := range cases {
		got := userFilter("inetOrgPerson", "uid", "", c.username)
		if got != c.want {
			t.Errorf("userFilter(%q) = %q, want %q", c.username, got, c.want)
			continue
		}
		packet, err := ldap.CompileFilter(got)
		if err != nil {
			t.Errorf("filter %q does not compile: %v", got, err)
			continue
		}
		if back, _ := ldap.DecompileFilter(packet); back != got {
			t.Errorf("filter %q decompiles to %q", got, back)
		}
	}
}

func TestUserFilterExtraAndClass(t *testing.T) {
	want := "(&(objectClass=inetOrgPerson)(uid=x)(mail=*@example.com))"
	for _, extra := range []string{"(mail=*@example.com)", "mail=*@example.com", "  (mail=*@example.com) "} {
		if got := userFilter("inetOrgPerson", "uid", extra, "x"); got != want {
			t.Errorf("extra %q: %q, want %q", extra, got, want)
		}
	}
	if got := userFilter("", "uid", "", "x"); got != "(&(objectClass=*)(uid=x))" {
		t.Errorf("empty object class: %q", got)
	}
}

func TestMemberValue(t *testing.T) {
	dn := `uid=a\,b,ou=people,dc=example,dc=com`
	cases := []struct{ format, username, want string }{
		{"${dn}", "a,b", dn},
		{"${username}", "a,b", "a,b"},
		{"uid=${username},ou=people,dc=example,dc=com", "a,b", `uid=a\,b,ou=people,dc=example,dc=com`},
		{"uid=${username},ou=people", " lead#", `uid=\ lead#,ou=people`},
		{"uid=${username},ou=people", "#x+y", `uid=\#x\+y,ou=people`},
	}
	for _, c := range cases {
		if got := memberValue(c.format, dn, c.username); got != c.want {
			t.Errorf("memberValue(%q, %q) = %q, want %q", c.format, c.username, got, c.want)
		}
	}
}

func TestGroupFilterEscapes(t *testing.T) {
	got := groupFilter("groupOfNames", "member", `uid=a\,b(x)*,ou=people,dc=example,dc=com`)
	want := `(&(objectClass=groupOfNames)(member=uid=a\5c,b\28x\29\2a,ou=people,dc=example,dc=com))`
	if got != want {
		t.Fatalf("groupFilter = %q, want %q", got, want)
	}
	if _, err := ldap.CompileFilter(got); err != nil {
		t.Fatalf("filter does not compile: %v", err)
	}
	if got := groupFilter("", "memberUid", "x"); got != "(&(objectClass=*)(memberUid=x))" {
		t.Fatalf("empty object class: %q", got)
	}
}

// N19: the first part of a group DN is a role name, the full DN is one too.
func TestDynamicGroupNames(t *testing.T) {
	got := dynamicGroupNames([]string{
		"cn=demo-admins,ou=groups,dc=example,dc=com",
		`cn=Dev\2C Ops,ou=groups,dc=example,dc=com`,
		"not a dn",
		"cn=demo-admins,ou=groups,dc=example,dc=com",
	})
	want := []string{
		"Dev, Ops",
		`cn=Dev\2C Ops,ou=groups,dc=example,dc=com`,
		"cn=demo-admins,ou=groups,dc=example,dc=com",
		"demo-admins",
		"not a dn",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("dynamicGroupNames = %q, want %q", got, want)
	}
	if got := dynamicGroupNames(nil); len(got) != 0 {
		t.Fatalf("no values give %q", got)
	}
}

func TestJoinDN(t *testing.T) {
	cases := [][3]string{
		{"ou=people", "dc=example,dc=com", "ou=people,dc=example,dc=com"},
		{"", "dc=example,dc=com", "dc=example,dc=com"},
		{"ou=people", "", "ou=people"},
		{" ou=people ", " dc=example,dc=com ", "ou=people,dc=example,dc=com"},
	}
	for _, c := range cases {
		if got := joinDN(c[0], c[1]); got != c[2] {
			t.Errorf("joinDN(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestBreakerRestsAfterIncidents(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	b := newBreaker(3, 300*time.Second, clock.now)
	for i := 0; i < 2; i++ {
		if !b.allow() {
			t.Fatalf("refused after %d failures", i)
		}
		b.failure()
	}
	if !b.allow() {
		t.Fatal("refused after 2 failures")
	}
	b.failure()
	if b.allow() {
		t.Fatal("allowed after the third failure")
	}
	clock.advance(299 * time.Second)
	if b.allow() {
		t.Fatal("allowed 299 s after the third failure")
	}
	clock.advance(time.Second)
	if !b.allow() {
		t.Fatal("refused 300 s after the third failure")
	}
	b.failure()
	if b.allow() {
		t.Fatal("a failure after the rest does not rest again")
	}
	clock.advance(300 * time.Second)
	if !b.allow() {
		t.Fatal("refused after the second rest")
	}
	b.success()
	b.failure()
	if !b.allow() {
		t.Fatal("a success did not reset the count")
	}
}

func TestBreakerWithoutLimitNeverRests(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	b := newBreaker(0, 300*time.Second, clock.now)
	for i := 0; i < 50; i++ {
		b.failure()
	}
	if !b.allow() {
		t.Fatal("a limit of 0 rests the connection")
	}
}

// acceptCounter listens on loopback and counts connections. Each accepted
// connection gets one byte and is closed, so a probe can tell that the count
// is current.
type acceptCounter struct {
	ln    net.Listener
	count atomic.Int64
}

func newAcceptCounter(t *testing.T) *acceptCounter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &acceptCounter{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			a.count.Add(1)
			_, _ = c.Write([]byte{0})
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return a
}

func (a *acceptCounter) port() int { return a.ln.Addr().(*net.TCPAddr).Port }

// seen returns the number of connections before a probe of its own.
func (a *acceptCounter) seen(t *testing.T) int64 {
	t.Helper()
	c, err := net.Dial("tcp", a.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	return a.count.Load() - 1
}

func localServer(port int) model.LDAPServer {
	return model.LDAPServer{
		ID: "demo-local-1", Name: "demo-local", Order: 1, Protocol: "ldap", Host: "127.0.0.1", Port: port,
		SearchBase: "dc=example,dc=com", AuthScheme: "NONE", ConnectionTimeoutSeconds: 2,
		ConnectionRetryDelaySeconds: 300, MaxIncidentsCount: 3, UserBaseDN: "ou=people", UserSubtree: true,
		UserObjectClass: "inetOrgPerson", UserIDAttribute: "uid", UserRealNameAttribute: "cn",
		UserEmailAddressAttribute: "mail", LDAPGroupsAsRoles: true, GroupType: "static", GroupBaseDN: "ou=groups",
		GroupSubtree: true, GroupObjectClass: "groupOfNames", GroupIDAttribute: "cn", GroupMemberAttribute: "member",
		GroupMemberFormat: "${dn}",
	}
}

func TestDirectoryRestsAndTriesAgain(t *testing.T) {
	counter := newAcceptCounter(t)
	clock := &fakeClock{t: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	d, err := New(localServer(counter.port()), "", withClock(clock.now))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if _, _, err := d.Login(ctx, "demo-ldap-reader", "secret-1"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if got := counter.seen(t); got != 3 {
		t.Fatalf("3 attempts made %d connections", got)
	}
	if _, _, err := d.Login(ctx, "demo-ldap-reader", "secret-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("resting connection: %v", err)
	}
	if got := counter.seen(t); got != 3+1 {
		t.Fatalf("a resting connection dialled: %d connections", got)
	}
	clock.advance(300 * time.Second)
	if _, _, err := d.Login(ctx, "demo-ldap-reader", "secret-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("after the rest: %v", err)
	}
	if got := counter.seen(t); got != 3+1+1+1 {
		t.Fatalf("after the rest no new connection: %d connections", got)
	}
}

func TestCancelledContextIsNoIncident(t *testing.T) {
	counter := newAcceptCounter(t)
	d, err := New(localServer(counter.port()), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 5; i++ {
		_, _, err := d.Login(ctx, "demo-ldap-reader", "secret-1")
		if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled context: %v", err)
		}
	}
	if d.breaker.failures() != 0 {
		t.Fatalf("a cancelled context counted %d incidents", d.breaker.failures())
	}
}

func TestChainOrder(t *testing.T) {
	var dirs []*Directory
	for _, order := range []int{3, 1, 2, 1} {
		s := localServer(1)
		s.Order = order
		s.ID = "demo-" + string(rune('a'+len(dirs)))
		d, err := New(s, "")
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, d)
	}
	c := NewChain(dirs...)
	var got []string
	for _, d := range c.dirs {
		got = append(got, d.server.ID)
	}
	if want := []string{"demo-b", "demo-d", "demo-c", "demo-a"}; !slices.Equal(got, want) {
		t.Fatalf("chain order %v, want %v", got, want)
	}
}

// A bind with an empty name and a password is anonymous on some servers, so
// an entry without a DN is never bound as.
func TestPickEntry(t *testing.T) {
	one := ldap.NewEntry("uid=a,ou=people,dc=example,dc=com", nil)
	if got, err := pickEntry([]*ldap.Entry{one}); err != nil || got != one {
		t.Fatalf("one entry: %v, %v", got, err)
	}
	for name, entries := range map[string][]*ldap.Entry{
		"none":     nil,
		"two":      {one, one},
		"empty dn": {ldap.NewEntry("", nil)},
		"blank dn": {ldap.NewEntry("  ", nil)},
	} {
		if _, err := pickEntry(entries); err != ErrInvalidCredentials {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A context that ends while the directory is asked is no incident either.
func TestContextDeadlineIsNoIncident(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		for _, c := range held {
			_ = c.Close()
		}
	})
	s := localServer(ln.Addr().(*net.TCPAddr).Port)
	s.MaxIncidentsCount = 1
	s.ConnectionTimeoutSeconds = 30
	d, err := New(s, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := d.Login(ctx, "demo-ldap-reader", "secret-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	if d.breaker.failures() != 0 || !d.breaker.allow() {
		t.Fatalf("an ended context counted %d incidents", d.breaker.failures())
	}
}
