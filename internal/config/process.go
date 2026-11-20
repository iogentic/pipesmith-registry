package config

import (
	"fmt"
	"math/big"
	"net/netip"
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
	"github.com/iogentic/pipesmith-registry/internal/pystr"
)

// Process values of config.json that the reference does not know. Each is
// written and shown only while it differs from its default, so config.json
// and config --json of a default start stay those of the reference.

// ParseTrustedProxies reads the entries of trusted_proxies (decision E8 of
// the execution plan): an IP address or a network in CIDR form, without a
// zone, without bits set past the prefix length, and an IPv4 address in its
// IPv4 form, so that a value means one thing only.
func ParseTrustedProxies(entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for _, entry := range entries {
		p, err := parseProxy(entry)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func parseProxy(entry string) (netip.Prefix, error) {
	notAddress := fmt.Errorf("trusted_proxies: %s is not an IP address or a network in CIDR form", pystr.Repr(entry))
	var p netip.Prefix
	if strings.Contains(entry, "/") {
		parsed, err := netip.ParsePrefix(entry)
		if err != nil {
			return netip.Prefix{}, notAddress
		}
		if parsed != parsed.Masked() {
			return netip.Prefix{}, fmt.Errorf("trusted_proxies: %s sets bits past its prefix length; the network is %s",
				pystr.Repr(entry), parsed.Masked())
		}
		p = parsed
	} else {
		addr, err := netip.ParseAddr(entry)
		if err != nil || addr.Zone() != "" {
			return netip.Prefix{}, notAddress
		}
		p = netip.PrefixFrom(addr, addr.BitLen())
	}
	if p.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("trusted_proxies: %s is an IPv4 address in IPv6 form; write it as IPv4", pystr.Repr(entry))
	}
	return p, nil
}

// The defaults of export_max_files and export_max_bytes, those of
// export.DefaultLimits.
const (
	defaultExportMaxFiles = 20_000
	defaultExportMaxBytes = 2 << 30
)

// ExportBound reads a value of export_max_files or export_max_bytes, of
// config.json or of its serve option named what: a whole number above 0
// that fits in 64 bits.
func ExportBound(what string, value any) (int64, error) {
	var n int64
	switch x := value.(type) {
	case int64:
		n = x
	case *big.Int:
		if x.IsInt64() {
			n = x.Int64()
		}
	}
	if n < 1 {
		return 0, valueError("%s is not a whole number above 0: %s", what, pyRepr(value))
	}
	return n, nil
}

func pyRepr(value any) string {
	switch x := value.(type) {
	case string:
		return pystr.Repr(x)
	case bool:
		if x {
			return "True"
		}
		return "False"
	case *big.Int:
		return x.String()
	}
	return fmt.Sprint(value)
}

// applyProcess reads the process values of this product from config.json.
func applyProcess(cfg *Config, data *pyjson.Object) error {
	path := cfg.ConfigPath()
	for _, field := range []struct {
		key    string
		target *string
	}{{"tls_cert", &cfg.TLSCert}, {"tls_key", &cfg.TLSKey}} {
		if value := get(data, field.key); value != nil {
			if !nonEmptyString(value) {
				return valueError("%s: %s is not a path", path, field.key)
			}
			*field.target = value.(string)
		}
	}
	if value := get(data, "trusted_proxies"); value != nil {
		list, ok := value.([]any)
		if !ok {
			return valueError("%s: trusted_proxies is not a list of addresses", path)
		}
		entries := make([]string, 0, len(list))
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return valueError("%s: trusted_proxies is not a list of addresses", path)
			}
			entries = append(entries, s)
		}
		if _, err := ParseTrustedProxies(entries); err != nil {
			return valueError("%s: %s", path, err)
		}
		if len(entries) > 0 {
			cfg.TrustedProxies = entries
		}
	}
	for _, field := range []struct {
		key    string
		target *int64
		def    int64
	}{{"export_max_files", &cfg.ExportMaxFiles, defaultExportMaxFiles}, {"export_max_bytes", &cfg.ExportMaxBytes, defaultExportMaxBytes}} {
		if value := get(data, field.key); value != nil {
			n, err := ExportBound(field.key, value)
			if err != nil {
				return valueError("%s: %s", path, err)
			}
			if n != field.def {
				*field.target = n
			}
		}
	}
	return nil
}

// ProcessItems are the process values that differ from their defaults, as
// key and JSON value in the order config.json holds them: tls_cert, tls_key
// (decision E9 of the execution plan), trusted_proxies (E8),
// export_max_files and export_max_bytes (B18).
func (c *Config) ProcessItems() [][2]any {
	var items [][2]any
	if c.TLSCert != "" {
		items = append(items, [2]any{"tls_cert", c.TLSCert})
	}
	if c.TLSKey != "" {
		items = append(items, [2]any{"tls_key", c.TLSKey})
	}
	if len(c.TrustedProxies) > 0 {
		list := make([]any, len(c.TrustedProxies))
		for i, entry := range c.TrustedProxies {
			list[i] = entry
		}
		items = append(items, [2]any{"trusted_proxies", list})
	}
	if c.ExportMaxFiles != 0 && c.ExportMaxFiles != defaultExportMaxFiles {
		items = append(items, [2]any{"export_max_files", c.ExportMaxFiles})
	}
	if c.ExportMaxBytes != 0 && c.ExportMaxBytes != defaultExportMaxBytes {
		items = append(items, [2]any{"export_max_bytes", c.ExportMaxBytes})
	}
	return items
}

// checkProcess refuses a process value Load would refuse, before Save
// writes anything.
func checkProcess(cfg *Config) error {
	if _, err := ParseTrustedProxies(cfg.TrustedProxies); err != nil {
		return valueError("%s", err)
	}
	if cfg.ExportMaxFiles < 0 {
		return valueError("export_max_files is not a whole number above 0: %d", cfg.ExportMaxFiles)
	}
	if cfg.ExportMaxBytes < 0 {
		return valueError("export_max_bytes is not a whole number above 0: %d", cfg.ExportMaxBytes)
	}
	return nil
}

// saveProcess adds the process values that differ from their defaults to
// the document of config.json.
func saveProcess(cfg *Config, doc *pyjson.Object) {
	for _, item := range cfg.ProcessItems() {
		doc.Set(item[0].(string), item[1])
	}
}
