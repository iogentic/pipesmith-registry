package pystr

import (
	"sort"
	"unicode"
)

// foldRune is str.casefold of one code point: the full folding where
// CaseFolding.txt has one, else the simple folding (status C).
func foldRune(r rune) []rune {
	if full, ok := fullFolds[r]; ok {
		return []rune(full)
	}
	if isSurrogate(r) || newerThanPython(r) {
		return []rune{r}
	}
	switch {
	case r == 0x0131:
		// Dotless i has no folding of its own; its upper case I folds to i.
		return []rune{r}
	case r >= 0x13A0 && r <= 0x13F5:
		// Cherokee folds to its capital letters, the other way round.
		return []rune{r}
	case r >= 0x13F8 && r <= 0x13FD, r >= 0xAB70 && r <= 0xABBF:
		return []rune{unicode.ToUpper(r)}
	}
	if lower := unicode.ToLower(r); lower != r {
		return []rune{lower}
	}
	// A lower case letter folds to the lower case of its upper case:
	// U+017F long s folds to s, U+03C2 final sigma to sigma.
	if upper := unicode.ToUpper(r); upper != r {
		if lower := unicode.ToLower(upper); lower != upper {
			return []rune{lower}
		}
	}
	return []rune{r}
}

// IsPrintable is str.isprintable of one character: false for the categories
// Cc, Cf, Cs, Co, Cn, Zl, Zp and Zs except the space. repr() writes such a
// character as an escape.
func IsPrintable(r rune) bool {
	if isSurrogate(r) || newerThanPython(r) {
		return false
	}
	return unicode.IsPrint(r)
}

// DecimalValue is unicodedata.decimal: the digit a character of category
// Nd stands for, or -1.
func DecimalValue(r rune) int {
	i := sort.Search(len(decimalZeros), func(i int) bool { return decimalZeros[i] > r }) - 1
	if i >= 0 && r-decimalZeros[i] < 10 {
		return int(r - decimalZeros[i])
	}
	return -1
}

// newerThanPython reports whether r was unassigned in Unicode 15.0.0.
func newerThanPython(r rune) bool {
	i := sort.Search(len(assignedAfter15), func(i int) bool { return assignedAfter15[i][1] >= r })
	return i < len(assignedAfter15) && assignedAfter15[i][0] <= r
}
