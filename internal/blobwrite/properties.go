// Package blobwrite writes blobs into a file blob store in the layout of the
// old product (phase 4 plan, section Layout): the bytes file and the
// properties file of a new blob in the directory of the current minute in
// UTC, the bytes renamed first and the properties last, one line in the
// reconciliation log, and the delete mark that the old product writes. Work
// files live in content/tmp of the store. Only a store with the writable
// flag of internal/stores is ever written.
package blobwrite

import (
	"fmt"
	"slices"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const upperHex = "0123456789ABCDEF"

// EntryLines are the entry lines that java.util.Properties.store of Java 21
// writes for items: the keys in their natural order, which is the order of
// UTF-16 code units, each line "key=value" with the escapes of the class and
// "\n" as line end. Every character outside printable ASCII becomes a \uXXXX
// escape, so the text is ASCII and also ISO 8859-1. A key or value that is
// not UTF-8 is an error: a Java string cannot hold it.
func EntryLines(items map[string]string) ([]byte, error) {
	type key struct {
		text  string
		units []uint16
	}
	keys := make([]key, 0, len(items))
	for k, v := range items {
		if !utf8.ValidString(k) {
			return nil, fmt.Errorf("properties key %q is not UTF-8", k)
		}
		if !utf8.ValidString(v) {
			return nil, fmt.Errorf("properties value of %q is not UTF-8", k)
		}
		keys = append(keys, key{k, utf16.Encode([]rune(k))})
	}
	slices.SortFunc(keys, func(a, b key) int { return slices.Compare(a.units, b.units) })
	var out []byte
	for _, k := range keys {
		out = escape(out, k.units, true)
		out = append(out, '=')
		out = escape(out, utf16.Encode([]rune(items[k.text])), false)
		out = append(out, '\n')
	}
	return out, nil
}

// escape is saveConvert of java.util.Properties with escUnicode set: a key
// escapes every space, a value its first character only when that is one.
func escape(out []byte, units []uint16, isKey bool) []byte {
	for x, c := range units {
		if c > '=' && c < 0x7f {
			if c == '\\' {
				out = append(out, '\\')
			}
			out = append(out, byte(c))
			continue
		}
		switch c {
		case ' ':
			if x == 0 || isKey {
				out = append(out, '\\')
			}
			out = append(out, ' ')
		case '\t':
			out = append(out, '\\', 't')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\f':
			out = append(out, '\\', 'f')
		case '=', ':', '#', '!':
			out = append(out, '\\', byte(c))
		default:
			if c < 0x20 || c > 0x7e {
				out = append(out, '\\', 'u', upperHex[c>>12&0xF], upperHex[c>>8&0xF], upperHex[c>>4&0xF], upperHex[c&0xF])
			} else {
				out = append(out, byte(c))
			}
		}
	}
	return out
}

// Properties is a whole properties file as the old product writes it: a
// comment with the time stamp yyyy-MM-dd HH:mm:ss,SSSZ, a comment with the
// date in the form of Date.toString, then EntryLines. Both comments are in
// UTC (W9 of the phase 4 plan).
func Properties(items map[string]string, now time.Time) ([]byte, error) {
	lines, err := EntryLines(items)
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	header := "#" + now.Format("2006-01-02 15:04:05,000-0700") + "\n#" + now.Format("Mon Jan 02 15:04:05 MST 2006") + "\n"
	return append([]byte(header), lines...), nil
}
