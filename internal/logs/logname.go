package logs

import "strings"

// plainName is a name without "..", slash, backslash or control byte: a
// name in the log directory itself (docs/ui/API.md, section Logs).
func plainName(name string) bool {
	if name == "" || name == "." || strings.Contains(name, "..") {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool { return r == '/' || r == '\\' || r < 0x20 || r == 0x7f })
}
