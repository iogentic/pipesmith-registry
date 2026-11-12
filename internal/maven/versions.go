package maven

import (
	"slices"
	"unicode/utf8"

	"github.com/iogentic/pipesmith-registry/internal/pystr"
)

// Rank of a qualifier against the release itself (rank of ""). Unknown
// qualifiers sort after "sp".
var qualifierRank = map[string]int{
	"alpha": 0, "beta": 1, "milestone": 2, "rc": 3, "cr": 3, "snapshot": 4,
	"": 5, "ga": 5, "final": 5, "release": 5, "sp": 6,
}

const unknownRank = 7

// token is an int or a word of _tokens. text is a slice of the version:
// the decimal digits of a number, which compare by value, or the word.
type token struct {
	number bool
	text   string
}

var zero = token{number: true, text: "0"}

// tokenizer gives the tokens of _tokens one at a time, so that comparing
// two versions holds no token list: _tokens splits the stripped and lowered
// version at "." and "-" and between digits and letters, and drops the
// zeros at the end of a "-" segment and the zeros right before a word.
type tokenizer struct {
	s     string // the version, stripped and lowered
	i     int
	zeros int // zeros still to give before s[i:]
}

// span is the maximal run at s[i:] of decimal digits or of other
// characters, which stops at "." and "-": its end and whether it is digits.
func span(s string, i int) (int, bool) {
	r, width := utf8.DecodeRuneInString(s[i:])
	digits := pystr.DecimalValue(r) >= 0
	end := i + width
	for end < len(s) && s[end] != '.' && s[end] != '-' {
		r, width := utf8.DecodeRuneInString(s[end:])
		if (pystr.DecimalValue(r) >= 0) != digits {
			break
		}
		end += width
	}
	return end, digits
}

func allZero(digits string) bool {
	for _, r := range digits {
		if pystr.DecimalValue(r) != 0 {
			return false
		}
	}
	return true
}

func (t *tokenizer) next() (token, bool) {
	if t.zeros > 0 {
		t.zeros--
		return zero, true
	}
	for t.i < len(t.s) {
		if c := t.s[t.i]; c == '.' || c == '-' {
			t.i++
			continue
		}
		start := t.i
		end, digits := span(t.s, start)
		if !digits || !allZero(t.s[start:end]) {
			t.i = end
			return token{number: digits, text: t.s[start:end]}, true
		}
		// a zero stays when a number other than zero follows it in its
		// segment, with only zeros between them
		count, j := 1, end
		for {
			for j < len(t.s) && t.s[j] == '.' {
				j++
			}
			if j == len(t.s) || t.s[j] == '-' {
				break
			}
			e, d := span(t.s, j)
			if !d {
				break
			}
			if !allZero(t.s[j:e]) {
				t.i, t.zeros = j, count-1
				return zero, true
			}
			count, j = count+1, e
		}
		t.i = j
	}
	return token{}, false
}

// checkTokens raises what int() raises in _tokens, for the first token in
// order: a number past 4300 digits, or a word of digits int() does not read
// such as a superscript.
func checkTokens(lowered string) error {
	for i := 0; i < len(lowered); {
		if c := lowered[i]; c == '.' || c == '-' {
			i++
			continue
		}
		end, digits := span(lowered, i)
		text := lowered[i:end]
		if digits && utf8.RuneCountInString(text) > pystr.MaxIntDigits || !digits && isDigitText(text) {
			_, err := pystr.ParseInt(text)
			return err
		}
		i = end
	}
	return nil
}

// isDigitText is str.isdigit of a non-empty text.
func isDigitText(s string) bool {
	for _, r := range s {
		if !isDigitRune(r) {
			return false
		}
	}
	return s != ""
}

// lowered is version.strip().lower().
func lowered(version string) string {
	return pyLower(pystr.Strip(version))
}

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	}
	return 0
}

// compareNumbers compares two runs of decimal digits by their value.
func compareNumbers(a, b string) int {
	for a != "" {
		r, width := utf8.DecodeRuneInString(a)
		if pystr.DecimalValue(r) != 0 {
			break
		}
		a = a[width:]
	}
	for b != "" {
		r, width := utf8.DecodeRuneInString(b)
		if pystr.DecimalValue(r) != 0 {
			break
		}
		b = b[width:]
	}
	if n, m := utf8.RuneCountInString(a), utf8.RuneCountInString(b); n != m {
		return sign(n - m)
	}
	for a != "" {
		ra, wa := utf8.DecodeRuneInString(a)
		rb, wb := utf8.DecodeRuneInString(b)
		if da, db := pystr.DecimalValue(ra), pystr.DecimalValue(rb); da != db {
			return sign(da - db)
		}
		a, b = a[wa:], b[wb:]
	}
	return 0
}

// compareToken is _compare_token; a missing token stands for None.
func compareToken(a token, haveA bool, b token, haveB bool) int {
	switch {
	case !haveA && !haveB:
		return 0
	case !haveA:
		return -compareToken(b, true, token{}, false)
	}
	if a.number {
		if !haveB {
			if allZero(a.text) {
				return 0
			}
			return 1
		}
		if b.number {
			return compareNumbers(a.text, b.text)
		}
		return 1 // a number is newer than any qualifier
	}
	other := ""
	if haveB {
		if b.number {
			return -1
		}
		other = b.text
	}
	rankA, ok := qualifierRank[a.text]
	if !ok {
		rankA = unknownRank
	}
	rankB, ok := qualifierRank[other]
	if !ok {
		rankB = unknownRank
	}
	if rankA != rankB {
		return sign(rankA - rankB)
	}
	if rankA == unknownRank {
		return pystr.Compare(a.text, other)
	}
	return 0
}

// compareLowered is the loop of compare_versions over two checked versions.
func compareLowered(left, right string) int {
	a, b := tokenizer{s: left}, tokenizer{s: right}
	for {
		x, haveX := a.next()
		y, haveY := b.next()
		if !haveX && !haveY {
			return 0
		}
		if result := compareToken(x, haveX, y, haveY); result != 0 {
			return result
		}
	}
}

// CompareVersions is compare_versions: -1, 0 or 1 as version a is older,
// equal or newer than b. The error is the ValueError of int() for a number
// past 4300 digits or a digit int() does not read.
func CompareVersions(a, b string) (int, error) {
	left := lowered(a)
	if err := checkTokens(left); err != nil {
		return 0, err
	}
	right := lowered(b)
	if err := checkTokens(right); err != nil {
		return 0, err
	}
	return compareLowered(left, right), nil
}

// sortVersions is versions.sort(key=cmp_to_key(compare_versions)), stable
// as Python's sort. A list of one is not compared at all; in a longer one
// every version is, so a version that raises makes the sort raise. When
// several versions raise, Python raises for the one its sort compares
// first; this returns the error of the first in list order.
func sortVersions(versions []string) error {
	if len(versions) < 2 {
		return nil
	}
	keys := make(map[string]string, len(versions))
	for _, v := range versions {
		if _, ok := keys[v]; ok {
			continue
		}
		key := lowered(v)
		if err := checkTokens(key); err != nil {
			return err
		}
		keys[v] = key
	}
	slices.SortStableFunc(versions, func(a, b string) int { return compareLowered(keys[a], keys[b]) })
	return nil
}

// newest is _newest: the first of the greatest versions, "" for an empty
// list. compare_versions(value, best) reads value before best, and so does
// this when it looks for an error.
func newest(values []string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	best, bestKey := values[0], ""
	for i, value := range values[1:] {
		key := lowered(value)
		if err := checkTokens(key); err != nil {
			return "", err
		}
		if i == 0 {
			bestKey = lowered(best)
			if err := checkTokens(bestKey); err != nil {
				return "", err
			}
		}
		if compareLowered(key, bestKey) > 0 {
			best, bestKey = value, key
		}
	}
	return best, nil
}
