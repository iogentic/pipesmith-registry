package auth

import (
	"strings"

	"golang.org/x/text/unicode/norm"
)

// limiterName is a user name as the limiter counts it (finding A1 of the
// security assessment, review R5): folded as RFC 4518 prepares a string for
// a match, so the spellings a directory takes as one account are one pair.
// The code points section 2.2 maps to nothing are removed and those it maps
// to SPACE become spaces, section 2.3 normalizes to Unicode form KC, the name
// counts in lower case, and runs of spaces count as one with the outer ones
// cut, as section 2.6.1 makes them insignificant. Only the key of the
// limiter takes this form; the local users and the directories get the name
// as userName gives it.
func limiterName(user string) string {
	mapped := strings.Map(func(r rune) rune {
		switch {
		case inRanges(r, foldedAway):
			return -1
		case inRanges(r, foldedToSpace):
			return ' '
		}
		return r
	}, user)
	return strings.Join(strings.Fields(strings.ToLower(norm.NFKC.String(mapped))), " ")
}

// foldedAway are the code points RFC 4518 section 2.2 maps to nothing: soft
// hyphens, the combining grapheme joiner, the variation selectors (the RFC
// writes them FF00-FE0F; they are FE00-FE0F), the object replacement
// character, the code points of a control function and the zero width space.
var foldedAway = [][2]rune{
	{0x0000, 0x0008}, {0x000E, 0x001F}, {0x007F, 0x0084}, {0x0086, 0x009F}, {0x00AD, 0x00AD}, {0x034F, 0x034F},
	{0x06DD, 0x06DD}, {0x070F, 0x070F}, {0x1806, 0x1806}, {0x180B, 0x180E}, {0x200B, 0x200F}, {0x202A, 0x202E},
	{0x2060, 0x2063}, {0x206A, 0x206F}, {0xFE00, 0xFE0F}, {0xFEFF, 0xFEFF}, {0xFFF9, 0xFFFC}, {0x1D173, 0x1D17A},
	{0xE0001, 0xE0001}, {0xE0020, 0xE007F},
}

// foldedToSpace are the code points RFC 4518 section 2.2 maps to SPACE: the
// tabulations, line ends and NEXT LINE, and every separator of space, line
// or paragraph.
var foldedToSpace = [][2]rune{
	{0x0009, 0x000D}, {0x0020, 0x0020}, {0x0085, 0x0085}, {0x00A0, 0x00A0}, {0x1680, 0x1680}, {0x2000, 0x200A},
	{0x2028, 0x2029}, {0x202F, 0x202F}, {0x205F, 0x205F}, {0x3000, 0x3000},
}

func inRanges(r rune, ranges [][2]rune) bool {
	for _, span := range ranges {
		if r >= span[0] && r <= span[1] {
			return true
		}
	}
	return false
}
