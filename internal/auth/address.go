package auth

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// TrustedProxies are the networks of the reverse proxies whose forwarded
// headers count: the list trusted_proxies of config.json (decision E8 of the
// execution plan, N6 of the phase 3 plan). The default is empty, so no
// forwarded header counts, from loopback neither: behind an SSH tunnel every
// client comes from loopback and could name an address of its own choice.
type TrustedProxies []netip.Prefix

func (t TrustedProxies) trusts(ip netip.Addr) bool {
	for _, p := range t {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// peer is the host of the connection, and its IP address when it has one.
func peer(r *http.Request) (string, netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr, netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host, netip.Addr{}, false
	}
	return host, ip.Unmap(), true
}

// ClientAddress is the address of the caller for the sign in limits: the
// address of the connection, and behind a trusted proxy the right most
// address of X-Forwarded-For that is no trusted proxy. The address of the
// connection stands when the header names no such address before an entry
// that is no bare IP address (a port, brackets, a zone, an empty entry or a
// word such as unknown), or names trusted proxies only (review focus 4).
func (t TrustedProxies) ClientAddress(r *http.Request) string {
	host, ip, ok := peer(r)
	if !ok || !t.trusts(ip) {
		return host
	}
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return host
	}
	entries := strings.Split(strings.Join(values, ","), ",")
	for i := len(entries) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.Trim(entries[i], " \t"))
		if err != nil || addr.Zone() != "" {
			return host
		}
		if addr = addr.Unmap(); !t.trusts(addr) {
			return addr.String()
		}
	}
	return host
}

// Secure reports whether the caller reached the server over TLS: the
// connection is TLS, or it comes from a trusted proxy and X-Forwarded-Proto
// names https for a hop. The session cookie is Secure then (review 6,
// finding 4).
func (t TrustedProxies) Secure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if _, ip, ok := peer(r); !ok || !t.trusts(ip) {
		return false
	}
	for _, value := range r.Header.Values("X-Forwarded-Proto") {
		for _, scheme := range strings.Split(value, ",") {
			if strings.EqualFold(strings.Trim(scheme, " \t"), "https") {
				return true
			}
		}
	}
	return false
}

// budgetKey is the client address a budget of failures counts for (limiter
// rule B, row 3-8 of review 3): an IPv4 address, the /64 of an IPv6 address
// (row 3-16), and any other address as it stands. The pair key keeps the
// full address.
func budgetKey(address string) string {
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return address
	}
	if ip = ip.Unmap(); ip.Is4() {
		return ip.String()
	}
	return netip.PrefixFrom(ip.WithZone(""), 64).Masked().String()
}
