package server

import (
	"net/http"
	"net/netip"
	"os"
	"strings"

	"github.com/mirosso25/pipesmith-registry/internal/pystr"
)

// The Python server runs under uvicorn with proxy_headers on: from a client
// in the trusted hosts, X-Forwarded-Proto sets the scheme of the request and
// X-Forwarded-For its client. The trusted hosts come from the environment
// variable FORWARDED_ALLOW_IPS, as uvicorn reads it, else forwardedDefault.
// This is uvicorn/middleware/proxy_headers.py of uvicorn 0.54.0.

const forwardedDefault = "127.0.0.1,::1"

// forwardedAllowIPs is the value uvicorn reads at start.
func forwardedAllowIPs() string {
	if value, ok := os.LookupEnv("FORWARDED_ALLOW_IPS"); ok {
		return value
	}
	return forwardedDefault
}

// network is an IP network of ipaddress.ip_network: an address with its
// host bits clear and a prefix length.
type network struct {
	addr netip.Addr
	bits int
}

func (n network) contains(ip netip.Addr) bool {
	ip = ip.WithZone("") // ipaddress compares the bits only
	if ip.Is4() != n.addr.Is4() {
		return false
	}
	prefix, err := ip.Prefix(n.bits)
	return err == nil && prefix.Addr() == n.addr
}

// trustedHosts is _TrustedHosts.
type trustedHosts struct {
	always   bool
	literals map[string]bool
	hosts    map[netip.Addr]bool
	networks []network
}

func newTrustedHosts(value string) *trustedHosts {
	t := &trustedHosts{always: value == "*", literals: map[string]bool{}, hosts: map[netip.Addr]bool{}}
	if t.always {
		return t
	}
	for _, item := range strings.Split(value, ",") {
		host := pystr.Strip(item)
		if strings.Contains(host, "/") {
			if n, ok := parseNetwork(host); ok {
				t.networks = append(t.networks, n)
			} else {
				t.literals[host] = true
			}
			continue
		}
		if ip, ok := parseIP(host); ok {
			t.hosts[ip] = true
		} else {
			t.literals[host] = true
		}
	}
	return t
}

// trusts is _TrustedHosts.__contains__ of a latin-1 text.
func (t *trustedHosts) trusts(host string) bool {
	if t.always {
		return true
	}
	if host == "" {
		return false
	}
	if ip, ok := parseIP(host); ok {
		if t.hosts[ip] {
			return true
		}
		for _, n := range t.networks {
			if n.contains(ip) {
				return true
			}
		}
		return false
	}
	return t.literals[latin1Text(host)]
}

// clientAddress is get_trusted_client_address: the last host of the list
// that is not trusted, else the first.
func (t *trustedHosts) clientAddress(forwardedFor string) (string, string) {
	hosts := strings.Split(forwardedFor, ",")
	for i := range hosts {
		hosts[i] = latin1Strip(hosts[i])
	}
	if !t.always {
		for i := len(hosts) - 1; i >= 0; i-- {
			if host, port := parseHostPort(hosts[i]); !t.trusts(host) {
				return host, port
			}
		}
	}
	return parseHostPort(hosts[0])
}

// forwarded is ProxyHeadersMiddleware for one request from peer host and
// port: the scheme the application sees and the client of the access line,
// in UTF-8.
func (t *trustedHosts) forwarded(peerHost, peerPort string, h http.Header) (string, string) {
	scheme, client := "http", peerHost+":"+peerPort
	if !t.trusts(peerHost) {
		return scheme, client
	}
	if values := h.Values("X-Forwarded-Proto"); len(values) > 0 {
		switch proto := latin1Strip(values[len(values)-1]); proto {
		case "http", "https", "ws", "wss":
			scheme = proto
		}
	}
	if values := h.Values("X-Forwarded-For"); len(values) > 0 {
		if host, port := t.clientAddress(strings.Join(values, ", ")); host != "" {
			client = latin1Text(host) + ":" + port
		}
	}
	return scheme, client
}

// parseHostPort is _parse_host_port: host and port, the port as "%d" of
// int() writes it, "0" where there is none.
func parseHostPort(value string) (string, string) {
	if strings.HasPrefix(value, "[") {
		end := strings.Index(value, "]")
		if end < 0 {
			return value, "0"
		}
		host, rest := value[1:end], value[end+1:]
		if rest == "" {
			return host, "0"
		}
		if !strings.HasPrefix(rest, ":") {
			return value, "0"
		}
		if port, ok := pyIntText(rest[1:]); ok {
			return host, port
		}
		return host, "0"
	}
	if strings.Count(value, ":") == 1 {
		host, port, _ := strings.Cut(value, ":")
		if text, ok := pyIntText(port); ok {
			return host, text
		}
		return value, "0"
	}
	return value, "0"
}

// pyIntText is "%d" % int(s) of a latin-1 text; false where int() raises.
func pyIntText(s string) (string, bool) {
	n, err := pystr.ParseInt(latin1Text(s))
	if err != nil {
		return "", false
	}
	return n.String(), true
}

// parseIP is ipaddress.ip_address of a latin-1 text. A zone holds no "%",
// as in Python.
func parseIP(host string) (netip.Addr, bool) {
	if i := strings.IndexByte(host, '%'); i >= 0 && strings.Contains(host[i+1:], "%") {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Is4() && ip.Zone() != "" {
		return netip.Addr{}, false
	}
	return ip, true
}

// parseNetwork is ipaddress.ip_network(text) with strict=True: a prefix
// length of ASCII digits, for IPv4 also a netmask or a host mask, and no
// host bits set.
func parseNetwork(text string) (network, bool) {
	address, mask, _ := strings.Cut(text, "/")
	if strings.Contains(mask, "/") {
		return network{}, false
	}
	ip, ok := parseIP(address)
	if !ok || ip.Is4() && strings.Contains(address, "%") {
		return network{}, false
	}
	size := ip.BitLen()
	bits, ok := prefixLength(mask, size)
	if !ok && ip.Is4() {
		bits, ok = maskLength(mask)
	}
	if !ok {
		return network{}, false
	}
	plain := ip.WithZone("")
	prefix, err := plain.Prefix(bits)
	if err != nil || prefix.Addr() != plain {
		return network{}, false // host bits set
	}
	return network{addr: plain, bits: bits}, true
}

// prefixLength is _prefix_from_prefix_string.
func prefixLength(s string, size int) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		if n = n*10 + int(s[i]-'0'); n > size {
			return 0, false
		}
	}
	return n, true
}

// maskLength is _prefix_from_ip_string of IPv4: the length of a netmask,
// else of a host mask.
func maskLength(s string) (int, bool) {
	ip, err := netip.ParseAddr(s)
	if err != nil || !ip.Is4() {
		return 0, false
	}
	b := ip.As4()
	value := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	for _, v := range []uint32{value, ^value} {
		ones := 0
		for ones < 32 && v&(1<<(31-ones)) != 0 {
			ones++
		}
		if ones == 32 || v<<ones == 0 {
			return ones, true
		}
	}
	return 0, false
}
