package maven

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mirosso25/pipesmith-registry/internal/pystr"
)

// The str methods of Python 3.12 that maven.py calls: lower and isdigit.
// The tables come from tables_gen.go, measured on the reference Python.

type runeRange struct{ first, last rune }

type lowerRun struct {
	first, last rune
	stride      rune
	delta       rune
}

func inRuns(runs []runeRange, r rune) bool {
	i := sort.Search(len(runs), func(i int) bool { return runs[i].last >= r })
	return i < len(runs) && runs[i].first <= r
}

func lowerRune(r rune) rune {
	i := sort.Search(len(lowerRuns), func(i int) bool { return lowerRuns[i].last >= r })
	if i < len(lowerRuns) {
		run := lowerRuns[i]
		if run.first <= r && (r-run.first)%run.stride == 0 {
			return r + run.delta
		}
	}
	return r
}

// finalSigma is the Final_Sigma rule of str.lower for a capital sigma at i:
// a cased letter before it and none after it, case ignorable characters
// between them not counted.
func finalSigma(runes []rune, i int) bool {
	j := i - 1
	for j >= 0 && inRuns(caseIgnorableRuns, runes[j]) {
		j--
	}
	if j < 0 || !inRuns(sigmaCasedRuns, runes[j]) {
		return false
	}
	j = i + 1
	for j < len(runes) && inRuns(caseIgnorableRuns, runes[j]) {
		j++
	}
	return j == len(runes) || !inRuns(sigmaCasedRuns, runes[j])
}

// pyLower is str.lower. ASCII text lowers byte by byte, without the rune
// slices of the general case.
func pyLower(s string) string {
	upper := false
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return lowerRunes(s)
		}
		upper = upper || s[i] >= 'A' && s[i] <= 'Z'
	}
	if !upper {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

func lowerRunes(s string) string {
	runes := pystr.Runes(s)
	out := make([]rune, 0, len(runes))
	for i, r := range runes {
		switch {
		case r == 0x3A3:
			if finalSigma(runes, i) {
				out = append(out, 0x3C2)
			} else {
				out = append(out, 0x3C3)
			}
		case lowerMulti[r] != "":
			out = append(out, []rune(lowerMulti[r])...)
		default:
			out = append(out, lowerRune(r))
		}
	}
	return pystr.FromRunes(out)
}

// isDigitRune is str.isdigit of one character: a decimal digit or another
// digit such as a superscript.
func isDigitRune(r rune) bool {
	return pystr.DecimalValue(r) >= 0 || inRuns(digitRuns, r)
}

// pyIsDigit is str.isdigit.
func pyIsDigit(s string) bool {
	runes := pystr.Runes(s)
	for _, r := range runes {
		if !isDigitRune(r) {
			return false
		}
	}
	return len(runes) > 0
}
