package pystr

import (
	"fmt"
	"math/big"

	"github.com/iogentic/pipesmith-registry/internal/pyjson"
)

// MaxIntDigits is sys.get_int_max_str_digits() of Python.
const MaxIntDigits = 4300

// ParseInt is int(s) of a str with base 10. The error is a *pyjson.Error of
// class ValueError with the message of Python.
func ParseInt(s string) (*big.Int, error) {
	// CPython first turns every character above ASCII into ASCII: a space
	// becomes ' ', a decimal digit its ASCII digit, anything else '?' and
	// ends the text there. Characters below 127 stay, so 0x1c to 0x1f,
	// though str.isspace counts them, are no white space for int().
	buf := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		r, width := next(s, i)
		i += width
		if r < 127 {
			buf = append(buf, byte(r)) // #nosec G115 -- r is below 127 on this branch.
		} else if IsSpace(r) {
			buf = append(buf, ' ')
		} else if d := DecimalValue(r); d >= 0 {
			buf = append(buf, byte('0'+d)) // #nosec G115 -- d is a decimal digit, 0 to 9.
		} else {
			buf = append(buf, '?')
			break
		}
	}
	invalid := func() error {
		text := []rune(Repr(s))
		if len(text) > 200 {
			text = text[:200] // %.200R
		}
		return &pyjson.Error{Class: "ValueError", Message: "invalid literal for int() with base 10: " + FromRunes(text)}
	}
	// The parse has to take all of buf; a NUL, a '?' or any other byte
	// left over makes the literal invalid.
	p := 0
	for p < len(buf) && asciiSpace(buf[p]) {
		p++
	}
	negative := false
	if p < len(buf) && (buf[p] == '+' || buf[p] == '-') {
		negative = buf[p] == '-'
		p++
	}
	if p < len(buf) && buf[p] == '_' {
		return nil, invalid()
	}
	digits := make([]byte, 0, len(buf)-p)
	prev := byte(0)
	for p < len(buf) && (isDigit(buf[p]) || buf[p] == '_') {
		if buf[p] == '_' {
			if prev == '_' {
				return nil, invalid()
			}
		} else {
			digits = append(digits, buf[p])
		}
		prev = buf[p]
		p++
	}
	if prev == '_' || len(digits) == 0 {
		return nil, invalid()
	}
	if len(digits) > MaxIntDigits {
		return nil, &pyjson.Error{Class: "ValueError", Message: fmt.Sprintf(
			"Exceeds the limit (%d digits) for integer string conversion: value has %d digits; "+
				"use sys.set_int_max_str_digits() to increase the limit", MaxIntDigits, len(digits))}
	}
	for p < len(buf) && asciiSpace(buf[p]) {
		p++
	}
	if p != len(buf) {
		return nil, invalid()
	}
	n, _ := new(big.Int).SetString(string(digits), 10)
	if negative {
		n.Neg(n)
	}
	return n, nil
}

func asciiSpace(c byte) bool {
	return c == ' ' || (c >= '\t' && c <= '\r')
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}
