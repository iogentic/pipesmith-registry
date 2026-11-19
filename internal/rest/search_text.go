package rest

import (
	"strings"
	"unicode/utf8"
)

// isText is a search value the search can match: valid UTF-8 without a NUL
// byte.
func isText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func noTextMessage(name string) string {
	return "the search parameter " + name + " is no text: it must be UTF-8 without a NUL byte"
}

// scopePrefix is the "@" an npm scope may start with.
func scopePrefix(name string) string {
	if name == "npm.scope" {
		return "@"
	}
	return ""
}
