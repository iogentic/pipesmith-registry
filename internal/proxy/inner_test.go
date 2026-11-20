package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/repos"
	"github.com/iogentic/pipesmith-registry/internal/testkit"
)

// Ruling P5-8 of the phase 5 session: an address the remote names is never
// dialled when it is a loopback, link local, private or unspecified
// address, unless the remote address of the repository itself resolves to
// an address of that kind. The finding came from the security assessment of
// 2026-11-19 (a redirect of the remote to another listener was followed,
// stored and served). Every remote of these tests listens on loopback, so a
// classifier hook calls the remote public where a test needs a remote that
// is not inner; TestAddressKinds proves the real classifier on literal
// addresses.

// TestAddressKinds is the real classifier over literal addresses, the mapped
// forms and the unspecified address among them. The public rows are
// addresses of the documentation ranges and the edges of the private ones;
// none is ever dialled.
func TestAddressKinds(t *testing.T) {
	for _, c := range []struct {
		address string
		want    addressKind
	}{
		{"127.0.0.1", kindLoopback}, {"127.255.255.254", kindLoopback}, {"::1", kindLoopback}, {"::ffff:127.0.0.1", kindLoopback},
		{"0.0.0.0", kindUnspecified}, {"::", kindUnspecified}, {"0.1.2.3", kindUnspecified}, {"::ffff:0.0.0.0", kindUnspecified},
		{"169.254.169.254", kindLinkLocal}, {"169.254.0.1", kindLinkLocal}, {"::ffff:169.254.169.254", kindLinkLocal},
		{"fe80::1", kindLinkLocal}, {"fe80::1%eth0", kindLinkLocal}, {"224.0.0.251", kindLinkLocal}, {"ff02::1", kindLinkLocal},
		{"ff01::1", kindLinkLocal}, {"239.1.2.3", kindMulticast}, {"ff05::1", kindMulticast}, {"ff0e::1", kindMulticast},
		{"10.0.0.1", kindPrivate}, {"10.255.255.255", kindPrivate}, {"172.16.0.1", kindPrivate}, {"172.31.255.255", kindPrivate},
		{"192.168.1.1", kindPrivate}, {"::ffff:10.1.2.3", kindPrivate}, {"::ffff:192.168.0.1", kindPrivate}, {"fc00::1", kindPrivate},
		{"fd12:3456:789a::1", kindPrivate}, {"100.64.0.1", kindPrivate}, {"100.127.255.255", kindPrivate}, {"fec0::1", kindPrivate},
		{"192.0.2.1", kindPublic}, {"198.51.100.7", kindPublic}, {"203.0.113.9", kindPublic}, {"2001:db8::1", kindPublic},
		{"::ffff:192.0.2.1", kindPublic}, {"172.15.255.255", kindPublic}, {"172.32.0.0", kindPublic}, {"100.63.255.255", kindPublic},
		{"100.128.0.0", kindPublic}, {"192.169.0.1", kindPublic}, {"1.0.0.1", kindPublic}, {"11.0.0.1", kindPublic},
	} {
		a, err := netip.ParseAddr(c.address)
		if err != nil {
			t.Fatal(err)
		}
		if got := kindOf(a); got != c.want {
			t.Errorf("%s: %s, want %s", c.address, got, c.want)
		}
	}
}

// addrPortOf is the address and port a URL of a remote names.
func addrPortOf(t *testing.T, raw string) netip.AddrPort {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	ap, err := netip.ParseAddrPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	return ap
}

// publicAt calls the listener at ap public and every other address by its
// kind.
func publicAt(ap netip.AddrPort) classifier {
	return func(dialled netip.AddrPort) addressKind {
		if dialled == ap {
			return kindPublic
		}
		return kindOf(dialled.Addr())
	}
}

const innerPath = "/latest/meta-data/secret.txt"

// The client refuses a redirect to an inner address of another kind than
// the remote address, also when a name resolves to it, follows one within
// the kind of the remote address and one to a public address, and dials the
// remote address of the repository whatever its kind.
func TestClientDialsNoInnerAddressTheRemoteNames(t *testing.T) {
	remote := testkit.NewRemote(t)
	inner := testkit.NewRemote(t)
	inner.Script(innerPath, testkit.OK("CANARY-INNER-SERVICE-7d21"))
	_, innerPort, _ := net.SplitHostPort(strings.TrimPrefix(inner.URL(), "http://"))
	everything := func(kind addressKind) classifier { return func(netip.AddrPort) addressKind { return kind } }
	remoteAt, innerAt := addrPortOf(t, remote.URL()), addrPortOf(t, inner.URL())
	for _, c := range []struct {
		name     string
		location string
		classify classifier
		followed bool
	}{
		{"a second listener on loopback, the remote public", inner.URL() + innerPath, publicAt(remoteAt), false},
		{"a name that resolves to loopback, the remote public", inner.URLAt("localhost") + innerPath, publicAt(remoteAt), false},
		{"the unspecified address with the port of a loopback listener", "http://0.0.0.0:" + innerPort + innerPath, nil, false},
		{"loopback, the remote on loopback", inner.URL() + innerPath, nil, true},
		{"a listener the hook calls public", inner.URL() + innerPath, func(ap netip.AddrPort) addressKind {
			if ap == innerAt {
				return kindPublic
			}
			return kindOf(ap.Addr())
		}, true},
		{"private to private, the remote private", inner.URL() + innerPath, everything(kindPrivate), true},
	} {
		remote.Reset()
		inner.Reset()
		remote.Script("/raw/docs/moved.txt", testkit.Redirect(http.StatusFound, c.location))
		client, err := NewClient(Options{Repository: "demo-raw-proxy", Settings: repos.ProxySettings{RemoteURL: remote.URL() + "/raw/",
			TimeoutSeconds: 5, Retries: 2}, classify: c.classify})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		resp, err := client.Do(ctx, http.MethodGet, "docs/moved.txt", "", nil)
		switch {
		case c.followed && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.followed:
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != 200 || string(body) != "CANARY-INNER-SERVICE-7d21" || inner.Count(innerPath) != 1 {
				t.Errorf("%s: %d %q, %d requests at the inner listener", c.name, resp.StatusCode, body, inner.Count(innerPath))
			}
		case err == nil:
			_ = resp.Body.Close()
			t.Errorf("%s: the redirect was followed: %d", c.name, resp.StatusCode)
		case !errors.Is(err, ErrInnerAddress):
			t.Errorf("%s: refused for another reason: %v", c.name, err)
		case inner.Count(innerPath) != 0 || remote.Count("/raw/docs/moved.txt") != 1:
			t.Errorf("%s: %d requests at the inner listener, %d at the remote", c.name, inner.Count(innerPath),
				remote.Count("/raw/docs/moved.txt"))
		}
		cancel()
		client.Close()
	}
	// the remote address of the repository is dialled whatever its kind
	remote.Script("/raw/docs/guide.txt", testkit.OK("the guide"))
	for _, kind := range []addressKind{kindUnspecified, kindLinkLocal, kindPrivate, kindLoopback} {
		client, err := NewClient(Options{Repository: "demo-raw-proxy", Settings: repos.ProxySettings{RemoteURL: remote.URL() + "/raw/",
			TimeoutSeconds: 5}, classify: everything(kind)})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(context.Background(), http.MethodGet, "docs/guide.txt", "", nil)
		if err != nil {
			t.Fatalf("a remote address of the kind %s: %v", kind, err)
		}
		_ = resp.Body.Close()
		client.Close()
	}
}

// The turned around hand over test of the security assessment, in Go: a
// miss whose remote redirects to an inner address answers 502, stores
// nothing and leaves the proxy ready, and the log line names the refusal
// with the address quoted.
func TestProxyRefusesARedirectToAnInnerAddress(t *testing.T) {
	remote := testkit.NewRemote(t)
	inner := testkit.NewRemote(t)
	inner.Script(innerPath, testkit.OK("CANARY-INNER-SERVICE-7d21"))
	remote.Script("/raw/docs/moved.txt", testkit.Redirect(http.StatusFound, inner.URL()+innerPath))
	repo := repos.Repo{Name: "demo-raw-proxy", Format: "raw", Type: "proxy", Attributes: map[string]any{
		"proxy": map[string]any{"remoteUrl": remote.URL() + "/raw/"}}}
	log := &lines{}
	p := New(Config{Logger: slog.New(log), classify: publicAt(addrPortOf(t, remote.URL()))})
	defer p.Close()
	stored := 0
	q := Query{Repo: repo, Path: "docs/moved.txt", Store: func(f Fetched) error {
		stored++
		_, err := io.Copy(io.Discard, f.Body)
		return err
	}}
	for i := 0; i < 4; i++ {
		outcome, err := p.Get(context.Background(), q)
		if err != nil || outcome != Failed {
			t.Fatalf("request %d: outcome %v, error %v, want failed", i+1, outcome, err)
		}
	}
	if stored != 0 || inner.Count(innerPath) != 0 {
		t.Fatalf("%d stores, %d requests at the inner listener", stored, inner.Count(innerPath))
	}
	if state := p.State(repo); state.Status != Ready {
		t.Fatalf("a refused redirect counts as a failure of the remote: %+v", state)
	}
	text := strings.Join(log.all(), "\n")
	if !strings.Contains(text, "the remote named an inner address") || !strings.Contains(text, `"docs/moved.txt"`) {
		t.Fatalf("the log does not name the refusal:\n%s", text)
	}
}

// The exception of ruling P5-8 holds only when every address the remote
// address of the repository resolves to has the kind of the dialled one: a
// remote name that resolves to a public and a link local address does not
// open the link local range to its redirects.
func TestGuardAsksEveryAddressOfTheRemote(t *testing.T) {
	linkLocal := netip.MustParseAddr("169.254.169.254")
	for _, c := range []struct {
		name     string
		resolved []netip.Addr
		allowed  bool
	}{
		{"link local only", []netip.Addr{netip.MustParseAddr("169.254.10.10")}, true},
		{"public and link local", []netip.Addr{netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("169.254.10.10")}, false},
		{"public only", []netip.Addr{netip.MustParseAddr("203.0.113.7")}, false},
	} {
		g := &guard{host: "mirror.example.test", port: "443", classify: realKind,
			lookup: func(context.Context, string) ([]netip.Addr, error) { return c.resolved, nil }}
		err := g.check(context.Background(), "tcp4", netip.AddrPortFrom(linkLocal, 80).String(), nil)
		if allowed := err == nil; allowed != c.allowed {
			t.Errorf("%s: a dial of %s gives %v", c.name, linkLocal, err)
		}
		if err != nil && !errors.Is(err, ErrInnerAddress) {
			t.Errorf("%s: %v is no ErrInnerAddress", c.name, err)
		}
	}
}

// Ruling ST11 of leg 6-stores: a repository whose remote a subject without
// the full permission set set carries PublicOnly, and its client refuses
// every dial to a closed address, the dial of its own remote address among
// them. The check runs on the address each dial resolves, so a name that
// resolves to loopback only at dial time is caught.
func TestPublicOnlyRefusesEveryClosedDial(t *testing.T) {
	remote := testkit.NewRemote(t)
	remote.Script("/raw/docs/a.txt", testkit.OK("CANARY-CLOSED-SERVICE-41b9"))
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(remote.URL(), "http://"))
	for _, c := range []struct {
		base       string
		publicOnly bool
		reached    bool
	}{
		{remote.URLAt("localhost"), true, false},
		{remote.URL(), true, false},
		{"http://0.0.0.0:" + port, true, false},
		{remote.URLAt("localhost"), false, true},
	} {
		remote.Reset()
		client, err := NewClient(Options{Repository: "demo-raw-proxy", Settings: repos.ProxySettings{RemoteURL: c.base + "/raw/",
			TimeoutSeconds: 5, Retries: 0, PublicOnly: c.publicOnly}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		resp, err := client.Do(ctx, http.MethodGet, "docs/a.txt", "", nil)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		cancel()
		client.Close()
		switch {
		case c.reached && (err != nil || resp.StatusCode != http.StatusOK):
			t.Errorf("%s public only %v: %v", c.base, c.publicOnly, err)
		case !c.reached && (err == nil || !errors.Is(err, ErrInnerAddress)):
			t.Errorf("%s public only %v: dialled, error %v", c.base, c.publicOnly, err)
		}
		if got := remote.Count("/raw/docs/a.txt"); got != map[bool]int{true: 1, false: 0}[c.reached] {
			t.Errorf("%s public only %v: the remote saw %d requests", c.base, c.publicOnly, got)
		}
	}
}
