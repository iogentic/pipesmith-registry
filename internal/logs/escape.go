package logs

// Escape is text escaped as a field of the classic line, for a line of text
// outside this package such as the access line on standard error: every
// byte outside 0x21 to 0x7e, the quote and the backslash as \xHH with lower
// case hex digits. An empty text stays empty, and a text without such a
// byte comes back as it is.
func Escape(text string) string {
	if text == "" {
		return ""
	}
	if b := appendClassic(nil, text, false); len(b) != len(text) {
		return string(b) // an escape writes 4 bytes for 1, so a longer field holds one
	}
	return text
}
