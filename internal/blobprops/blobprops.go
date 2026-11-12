// Package blobprops parses the properties file of a blob in a file blob
// store: a Java properties text in ISO 8859-1.
//
// Strings follow the convention of internal/pyjson for a Python str: UTF-8,
// with a lone surrogate as the three byte sequence UTF-8 would give it.
package blobprops

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Keys of the properties a blob needs.
const (
	RepoKey = "@Bucket.repo-name"
	NameKey = "@BlobStore.blob-name"
	TypeKey = "@BlobStore.content-type"
)

// maxIntDigits is the digit limit of int(str) in Python 3.12.
const maxIntDigits = 4300

// BlobProps is what a properties file says about its blob.
type BlobProps struct {
	Repo        string  // @Bucket.repo-name
	Path        string  // @BlobStore.blob-name without a leading slash
	Size        int64   // 0 when Overflow says so
	SHA1        *string // nil when absent or not 40 hex digits
	ContentType *string // @BlobStore.content-type, nil when absent or empty
	CreatedMS   int64   // creationTime, 0 when Overflow says so
	Deleted     bool    // key "deleted" present with value true
	IsCopy      bool    // key "originalLocation" present: a soft delete copy, not a blob
	// Overflow is set when size or creationTime is a number beyond 64 bits.
	// Python keeps such a number, and its index then fails to store a live
	// entry; the field lets a store fail the same way.
	Overflow bool
}

// ValueError is the ValueError Python's int() raises for a number with more
// digits than its limit. It escapes parse() in Python and fails a scan.
type ValueError struct {
	Message string
}

func (e *ValueError) Error() string { return e.Message }

// Parse reads a properties file. It returns nil, nil where Python returns
// None: repo or blob name missing, size or creationTime missing or not a
// number, a malformed escape, or a lone surrogate in a used value.
func Parse(data []byte) (*BlobProps, error) {
	text := make([]rune, len(data))
	for i, b := range data {
		text[i] = rune(b)
	}
	items, err := load(text)
	if err != nil {
		return nil, nil
	}
	repo := items[RepoKey]
	path := strings.TrimLeft(items[NameKey], "/")
	sizeText, hasSize := items["size"]
	size, sizeOK, sizeOver, err := number(sizeText, hasSize, false)
	if err != nil {
		return nil, err
	}
	createdText, hasCreated := items["creationTime"]
	created, createdOK, createdOver, err := number(createdText, hasCreated, true)
	if err != nil {
		return nil, err
	}
	if repo == "" || path == "" || !sizeOK || !createdOK {
		return nil, nil
	}
	var contentType *string
	if value := items[TypeKey]; value != "" {
		contentType = &value
	}
	if !utf8.ValidString(repo) || !utf8.ValidString(path) || (contentType != nil && !utf8.ValidString(*contentType)) {
		return nil, nil
	}
	props := &BlobProps{
		Repo:        repo,
		Path:        path,
		Size:        size,
		ContentType: contentType,
		CreatedMS:   created,
		Deleted:     asciiLower(strings.TrimFunc(items["deleted"], pyIsSpace)) == "true",
		Overflow:    sizeOver || createdOver,
	}
	if sha := asciiLower(strings.TrimFunc(items["sha1"], pyIsSpace)); isSHA1(sha) {
		props.SHA1 = &sha
	}
	_, props.IsCopy = items["originalLocation"]
	return props, nil
}

// Load returns the keys and values of a properties text as
// java.util.Properties.load reads them. A later key wins.
func Load(text string) (map[string]string, error) {
	return load(pyRunes(text))
}

func load(text []rune) (map[string]string, error) {
	items := map[string]string{}
	for _, line := range logicalLines(text) {
		key, value := split(line)
		k, err := unescape(key)
		if err != nil {
			return nil, err
		}
		v, err := unescape(value)
		if err != nil {
			return nil, err
		}
		items[k] = v
	}
	return items, nil
}

func isPropsSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\f'
}

func trimLeftSpace(line []rune) []rune {
	for len(line) > 0 && isPropsSpace(line[0]) {
		line = line[1:]
	}
	return line
}

// naturalLines splits at "\r\n", "\r" and "\n", as re.split does: a text
// that ends with a line end gives an empty last line.
func naturalLines(text []rune) [][]rune {
	var lines [][]rune
	start := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\r':
			lines = append(lines, text[start:i])
			if i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
			start = i + 1
		case '\n':
			lines = append(lines, text[start:i])
			start = i + 1
		}
	}
	return append(lines, text[start:])
}

func logicalLines(text []rune) [][]rune {
	natural := naturalLines(text)
	var out [][]rune
	for i := 0; i < len(natural); {
		line := trimLeftSpace(natural[i])
		i++
		if len(line) == 0 || line[0] == '#' || line[0] == '!' {
			continue
		}
		var joined []rune
		for continues(line) {
			joined = append(joined, line[:len(line)-1]...)
			if i >= len(natural) {
				line = nil
				break
			}
			line = trimLeftSpace(natural[i])
			i++
		}
		out = append(out, append(joined, line...))
	}
	return out
}

// continues reports whether the line ends with an odd number of backslashes.
func continues(line []rune) bool {
	count := 0
	for i := len(line) - 1; i >= 0 && line[i] == '\\'; i-- {
		count++
	}
	return count%2 == 1
}

func split(line []rune) ([]rune, []rune) {
	n := len(line)
	i := 0
	for i < n {
		ch := line[i]
		if ch == '\\' {
			i += 2
			continue
		}
		if ch == '=' || ch == ':' || isPropsSpace(ch) {
			break
		}
		i++
	}
	// A key that ends with a lone backslash steps past the end; Python's
	// slices clamp there.
	i = min(i, n)
	key := line[:i]
	j := i
	for j < n && isPropsSpace(line[j]) {
		j++
	}
	if j < n && (line[j] == '=' || line[j] == ':') {
		j++
	}
	for j < n && isPropsSpace(line[j]) {
		j++
	}
	return key, line[j:]
}

func isHex(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
}

// hexValue is the value of a digit that isHex accepted.
func hexValue(r rune) rune {
	switch {
	case r >= 'a':
		return r - 'a' + 10
	case r >= 'A':
		return r - 'A' + 10
	}
	return r - '0'
}

func unescape(text []rune) (string, error) {
	if !slices.Contains(text, '\\') {
		return pyString(text), nil
	}
	out := make([]rune, 0, len(text))
	for i, n := 0, len(text); i < n; {
		ch := text[i]
		i++
		if ch != '\\' {
			out = append(out, ch)
			continue
		}
		if i >= n {
			break
		}
		ch = text[i]
		i++
		switch ch {
		case 'u':
			digits := text[i:min(i+4, n)]
			if len(digits) < 4 || !isHex(digits[0]) || !isHex(digits[1]) || !isHex(digits[2]) || !isHex(digits[3]) {
				return "", &ValueError{Message: `malformed \uxxxx escape`}
			}
			var value rune
			for _, d := range digits {
				value = value<<4 | hexValue(d)
			}
			out = append(out, value)
			i += 4
		case 't':
			out = append(out, '\t')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 'f':
			out = append(out, '\f')
		default:
			out = append(out, ch)
		}
	}
	if slices.ContainsFunc(out, isSurrogate) {
		// Java writes a character beyond the BMP as two \uXXXX units.
		out = joinPairs(out)
	}
	return pyString(out), nil
}

func isSurrogate(r rune) bool {
	return r >= 0xD800 && r <= 0xDFFF
}

// joinPairs joins a high surrogate and the low one after it, as Python's
// round trip through UTF-16 with surrogatepass does; a lone one stays.
func joinPairs(in []rune) []rune {
	out := in[:0:0]
	for i := 0; i < len(in); i++ {
		r := in[i]
		if r >= 0xD800 && r <= 0xDBFF && i+1 < len(in) && in[i+1] >= 0xDC00 && in[i+1] <= 0xDFFF {
			out = append(out, 0x10000+(r-0xD800)<<10+(in[i+1]-0xDC00))
			i++
			continue
		}
		out = append(out, r)
	}
	return out
}

// pyString writes code points as UTF-8 with a surrogate in its three byte form.
func pyString(runes []rune) string {
	b := make([]byte, 0, len(runes))
	for _, r := range runes {
		if isSurrogate(r) {
			b = append(b, 0xED, 0x80|byte(r>>6&0x3F), 0x80|byte(r&0x3F))
			continue
		}
		b = utf8.AppendRune(b, r)
	}
	return string(b)
}

// pyRunes reads a string in the form of pyString back into code points. A
// byte that is not UTF-8 is U+DC00 plus the byte, as surrogateescape reads it.
func pyRunes(s string) []rune {
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && width == 1 {
			if i+2 < len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
				r, width = 0xD000|rune(s[i+1]&0x3F)<<6|rune(s[i+2]&0x3F), 3
			} else {
				r = 0xDC00 + rune(s[i])
			}
		}
		out = append(out, r)
		i += width
	}
	return out
}

// pyIsSpace is str.isspace of Python 3.12 for one code point.
func pyIsSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0D, r >= 0x1C && r <= 0x20:
		return true
	case r < 0x85:
		return false
	case r == 0x85, r == 0xA0, r == 0x1680, r >= 0x2000 && r <= 0x200A,
		r == 0x2028, r == 0x2029, r == 0x202F, r == 0x205F, r == 0x3000:
		return true
	}
	return false
}

// asciiLower lowers A to Z only. str.lower() of Python also lowers other
// letters, but no letter outside ASCII lowers into "true" or a hex digit,
// so the outcome of each use here is the same (tools/vectors/blobprops.py
// tests every letter that lowers into ASCII).
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

func isSHA1(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}

// number is _number of Python: the stripped value when it is an optional
// sign (a minus only when signed) and ASCII digits. ok is false where Python
// gives None; over is set when Python's int does not fit 64 bits.
func number(value string, present, signed bool) (n int64, ok, over bool, err error) {
	if !present {
		return 0, false, false, nil
	}
	value = strings.TrimFunc(value, pyIsSpace)
	digits := value
	if digits != "" && (digits[0] == '+' || signed && digits[0] == '-') {
		digits = digits[1:]
	}
	if digits == "" {
		return 0, false, false, nil
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false, false, nil
		}
	}
	if len(digits) > maxIntDigits {
		return 0, false, false, &ValueError{Message: fmt.Sprintf(
			"Exceeds the limit (%d digits) for integer string conversion: value has %d digits;"+
				" use sys.set_int_max_str_digits() to increase the limit", maxIntDigits, len(digits))}
	}
	n, err = strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, true, true, nil
	}
	return n, true, false, nil
}
