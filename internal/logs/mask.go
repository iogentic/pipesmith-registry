package logs

import "strings"

// Mark replaces the value of a masked query parameter.
const Mark = "***"

// maskedNames are the query parameters whose values reach no log line (L9).
var maskedNames = []string{"token", "password", "secret", "key", "access_token"}

// MaskQuery is query with the value of each parameter named token, password,
// secret, key or access_token replaced by Mark. The name match ignores case
// and decodes percent escapes once. Everything else stays as it is: the
// order, the separators & and ;, a parameter without a value, a repeated
// name. A query without a value to mask is returned unchanged.
func MaskQuery(query string) string {
	var b strings.Builder
	rest, masked := query, false
	for {
		i := strings.IndexAny(rest, "&;")
		part := rest
		if i >= 0 {
			part = rest[:i]
		}
		if name, _, hasValue := strings.Cut(part, "="); hasValue && maskedName(name) {
			if !masked {
				b.Grow(len(query))
				b.WriteString(query[:len(query)-len(rest)])
				masked = true
			}
			b.WriteString(name)
			b.WriteString("=" + Mark)
		} else if masked {
			b.WriteString(part)
		}
		if i < 0 {
			break
		}
		if masked {
			b.WriteByte(rest[i])
		}
		rest = rest[i+1:]
	}
	if !masked {
		return query
	}
	return b.String()
}

// MaskTarget is a request target with its query, the part after the first
// ?, masked by MaskQuery.
func MaskTarget(target string) string {
	path, query, found := strings.Cut(target, "?")
	if !found {
		return target
	}
	if masked := MaskQuery(query); masked != query {
		return path + "?" + masked
	}
	return target
}

// maskedName reports whether a parameter name, percent decoded once, is one
// of maskedNames in any case.
func maskedName(name string) bool {
	if strings.IndexByte(name, '%') >= 0 {
		name = unescapeOnce(name)
	}
	for _, m := range maskedNames {
		if strings.EqualFold(name, m) {
			return true
		}
	}
	return false
}

// unescapeOnce decodes each %HH of s; a % without two hex digits stays.
func unescapeOnce(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b = append(b, hexValue(s[i+1])<<4|hexValue(s[i+2]))
			i += 2
			continue
		}
		b = append(b, s[i])
	}
	return string(b)
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func hexValue(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}
