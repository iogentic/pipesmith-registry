package release

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
)

var hostName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

var portText = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)

// checkHealthAddress refuses a health_address that is not host:port, with an
// IP address or a DNS name as host and a port from 1 to 65535, and writes it
// back in the form net.JoinHostPort gives.
func (c *UpdateConfig) checkHealthAddress(values map[string]json.RawMessage) error {
	if _, ok := values["health_address"]; !ok {
		return nil
	}
	bad := func(reason string) error {
		return fmt.Errorf("health_address %q is no host:port: %s", c.HealthAddress, reason)
	}
	host, port, err := net.SplitHostPort(c.HealthAddress)
	if err != nil {
		return bad(err.Error())
	}
	n, err := strconv.Atoi(port)
	if !portText.MatchString(port) || err != nil || n > 65535 {
		return bad("the port is not a number from 1 to 65535")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() {
			return bad("the host is the unspecified address")
		}
		c.HealthAddress = net.JoinHostPort(ip.String(), port)
		return nil
	}
	if len(host) > 253 || !hostName.MatchString(host) {
		return bad("the host is no IP address and no DNS name")
	}
	c.HealthAddress = net.JoinHostPort(host, port)
	return nil
}
