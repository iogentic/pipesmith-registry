package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
)

// Ruling P5-8 of the phase 5 session: an address the remote names, the
// target of a redirect or the realm of a token challenge, is never dialled
// when it is a loopback, link local or private address, or the unspecified
// one, unless the remote address of the repository itself resolves to
// addresses of that kind only. The check runs at dial time on the resolved
// address, so a name that resolves to an inner address is caught too. It
// never applies to the remote address of the repository, which an
// administrator set.

// ErrInnerAddress is the refusal of a dial to an inner address that the
// remote named.
var ErrInnerAddress = errors.New("the remote named an inner address")

// addressKind is the kind of an address for ruling P5-8.
type addressKind int

const (
	kindPublic addressKind = iota
	kindLoopback
	kindLinkLocal
	kindPrivate
	kindUnspecified
	kindMulticast
)

func (k addressKind) String() string {
	switch k {
	case kindLoopback:
		return "loopback"
	case kindLinkLocal:
		return "link local"
	case kindPrivate:
		return "private"
	case kindUnspecified:
		return "unspecified"
	case kindMulticast:
		return "multicast"
	}
	return "public"
}

// innerPrefixes are private ranges beyond netip.Addr.IsPrivate: the shared
// address space of carrier networks (RFC 6598), which overlay networks use
// inside an organisation, and the site local range of IPv6 (deprecated by
// RFC 3879, still routed by some networks).
var innerPrefixes = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("fec0::/10")}

// kindOf is the kind of an address. A mapped IPv4 address counts as that
// IPv4 address; the whole of 0.0.0.0/8, which dials this host, counts as
// unspecified.
func kindOf(a netip.Addr) addressKind {
	a = a.Unmap()
	switch {
	case !a.IsValid(), a.IsUnspecified(), a.Is4() && a.As4()[0] == 0:
		return kindUnspecified
	case a.IsLoopback():
		return kindLoopback
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(), a.IsInterfaceLocalMulticast():
		return kindLinkLocal
	case a.IsMulticast():
		return kindMulticast
	case a.IsPrivate():
		return kindPrivate
	}
	for _, p := range innerPrefixes {
		if p.Contains(a.WithZone("")) {
			return kindPrivate
		}
	}
	return kindPublic
}

// Closed reports an address of a closed network: every kind but public
// (ruling ST11 of leg 6-stores). The rule of a proxy create in
// internal/admin and the dial guard read this one set.
func Closed(a netip.Addr) bool { return kindOf(a) != kindPublic }

// classifier gives the kind of a dialled address; the tests put one here
// that calls a remote on loopback public.
type classifier func(netip.AddrPort) addressKind

func realKind(ap netip.AddrPort) addressKind { return kindOf(ap.Addr()) }

// guard holds the remote address of a repository and the dial check of the
// addresses its remote names.
type guard struct {
	host, port string // the remote address of the repository, host in lower case
	classify   classifier
	// publicOnly refuses every address of a closed network, the remote
	// address of the repository among them (ruling ST11)
	publicOnly bool
	// lookup resolves the host of the remote address; nil is the resolver
	// of the system. Tests put one here.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
}

// own reports a dial of the remote address of the repository: the address
// the transport dials, host and port.
func (g *guard) own(address string) bool {
	return strings.EqualFold(address, net.JoinHostPort(g.host, g.port))
}

// remoteKinds are the kinds of the addresses the remote address of the
// repository resolves to now.
func (g *guard) remoteKinds(ctx context.Context) (map[addressKind]bool, error) {
	var addresses []netip.Addr
	if a, err := netip.ParseAddr(g.host); err == nil {
		addresses = []netip.Addr{a}
	} else if g.lookup != nil {
		if addresses, err = g.lookup(ctx, g.host); err != nil {
			return nil, err
		}
	} else if addresses, err = net.DefaultResolver.LookupNetIP(ctx, "ip", g.host); err != nil {
		return nil, err
	}
	port, err := net.LookupPort("tcp", g.port)
	if err != nil {
		return nil, err
	}
	kinds := map[addressKind]bool{}
	for _, a := range addresses {
		kinds[g.classify(netip.AddrPortFrom(a.Unmap(), uint16(port)))] = true // #nosec G115 -- LookupPort gives 0 to 65535.
	}
	return kinds, nil
}

// check is the control of a dial to an address the remote named, on the
// address the dialer resolved: a public address passes, an inner one only
// when every address the remote address of the repository resolves to is of
// that kind, so that a remote name with one inner address among public ones
// opens nothing.
func (g *guard) check(ctx context.Context, _, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %q is no address", ErrInnerAddress, address)
	}
	kind := g.classify(ap)
	if kind == kindPublic {
		return nil
	}
	if g.publicOnly {
		return fmt.Errorf("%w: a %s address, and the remote of this repository may reach public addresses only", ErrInnerAddress, kind)
	}
	kinds, err := g.remoteKinds(ctx)
	if err != nil {
		return fmt.Errorf("%w: the kind of the remote address is not known: %v", ErrInnerAddress, err)
	}
	if len(kinds) == 1 && kinds[kind] {
		return nil // every address of the remote has the kind: an inner remote, set by an administrator
	}
	return fmt.Errorf("%w: a %s address, and the remote address of the repository is none", ErrInnerAddress, kind)
}
