package repos

import (
	"net/url"
	"strings"
)

// PublicURL is a remote address without its user information, for every
// answer and every print. An address that parses without user information
// comes back byte for byte. One that parses with user information loses
// the authority up to its last "@". One that does not parse loses
// everything between "://" and the last "@" of the text, when there is
// one. The stored definitions keep the address as given.
func PublicURL(raw string) string {
	u, err := url.Parse(raw)
	if err == nil && u.User == nil {
		return raw
	}
	start := strings.Index(raw, "://")
	if start < 0 {
		return raw
	}
	start += len("://")
	end := len(raw)
	if err == nil {
		if i := strings.IndexAny(raw[start:], "/?#"); i >= 0 {
			end = start + i
		}
	}
	at := strings.LastIndex(raw[start:end], "@")
	if at < 0 {
		return raw
	}
	return raw[:start] + raw[start+at+1:]
}

// publicURLOf is PublicURL of an optional address.
func publicURLOf(raw *string) *string {
	if raw == nil {
		return nil
	}
	public := PublicURL(*raw)
	return &public
}

// PublicValue is a copy of an attribute value with every string that holds
// "://" through PublicURL; maps and lists are copied, other values kept.
func PublicValue(value any) any {
	switch v := value.(type) {
	case string:
		if strings.Contains(v, "://") {
			return PublicURL(v)
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = PublicValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = PublicValue(item)
		}
		return out
	default:
		return v
	}
}
