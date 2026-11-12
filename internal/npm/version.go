package npm

import (
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/pystr"
)

// PrePart is one dot separated part of a pre-release, as _pre_part orders
// it: Kind 0 with the Number for digits, Kind 1 with the Name otherwise.
type PrePart struct {
	Kind   int
	Number *big.Int
	Name   string
}

// Key is the sort key of version_key: the numbers of the core, Release 1
// without and 0 with a pre-release part, then the pre-release parts.
type Key struct {
	Numbers []*big.Int
	Release int
	Parts   []PrePart
}

// digitOnly are the characters str.isdigit accepts that are no decimal
// digits (numeric type Digit of Unicode 15.0.0, the database of Python 3.12).
// int() refuses them, so version_key raises ValueError on a part made of
// digits that holds one. TestDigitsWholeCodeSpace checks the table.
var digitOnly = [][2]rune{
	{0x00B2, 0x00B3}, {0x00B9, 0x00B9}, {0x1369, 0x1371}, {0x19DA, 0x19DA}, {0x2070, 0x2070},
	{0x2074, 0x2079}, {0x2080, 0x2089}, {0x2460, 0x2468}, {0x2474, 0x247C}, {0x2488, 0x2490},
	{0x24EA, 0x24EA}, {0x24F5, 0x24FD}, {0x24FF, 0x24FF}, {0x2776, 0x277E}, {0x2780, 0x2788},
	{0x278A, 0x2792}, {0x10A40, 0x10A43}, {0x10E60, 0x10E68}, {0x11052, 0x1105A}, {0x1F100, 0x1F10A},
}

func isDigitRune(r rune) bool {
	if pystr.DecimalValue(r) >= 0 {
		return true
	}
	for _, run := range digitOnly {
		if r >= run[0] && r <= run[1] {
			return true
		}
	}
	return false
}

// isDigits is str.isdigit.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range pystr.Runes(s) {
		if !isDigitRune(r) {
			return false
		}
	}
	return true
}

// number is `int(part) if part.isdigit() else 0`.
func number(part string) (*big.Int, error) {
	if !isDigits(part) {
		return big.NewInt(0), nil
	}
	return pystr.ParseInt(part)
}

// VersionKey is version_key: the numbers of the core, then a release before
// its pre-releases. A part of digits int() cannot read is its ValueError.
func VersionKey(version string) (Key, error) {
	core, _, _ := strings.Cut(version, "+")
	core, pre, dash := strings.Cut(core, "-")
	var key Key
	for _, part := range strings.Split(core, ".") {
		n, err := number(part)
		if err != nil {
			return Key{}, err
		}
		key.Numbers = append(key.Numbers, n)
	}
	if !dash {
		key.Release = 1
		return key, nil
	}
	for _, part := range strings.Split(pre, ".") {
		if isDigits(part) {
			n, err := pystr.ParseInt(part)
			if err != nil {
				return Key{}, err
			}
			key.Parts = append(key.Parts, PrePart{Kind: 0, Number: n})
		} else {
			key.Parts = append(key.Parts, PrePart{Kind: 1, Number: big.NewInt(0), Name: part})
		}
	}
	return key, nil
}

// Compare orders two keys as Python compares the tuples.
func (k Key) Compare(o Key) int {
	for i := 0; i < len(k.Numbers) && i < len(o.Numbers); i++ {
		if c := k.Numbers[i].Cmp(o.Numbers[i]); c != 0 {
			return c
		}
	}
	if c := compareInt(len(k.Numbers), len(o.Numbers)); c != 0 {
		return c
	}
	if c := compareInt(k.Release, o.Release); c != 0 {
		return c
	}
	for i := 0; i < len(k.Parts) && i < len(o.Parts); i++ {
		a, b := k.Parts[i], o.Parts[i]
		if c := compareInt(a.Kind, b.Kind); c != 0 {
			return c
		}
		if c := a.Number.Cmp(b.Number); c != 0 {
			return c
		}
		if c := pystr.Compare(a.Name, b.Name); c != 0 {
			return c
		}
	}
	return compareInt(len(k.Parts), len(o.Parts))
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func keys(versions []string) ([]Key, error) {
	out := make([]Key, len(versions))
	for i, v := range versions {
		k, err := VersionKey(v)
		if err != nil {
			return nil, err
		}
		out[i] = k
	}
	return out, nil
}

// SortVersions is sorted(versions, key=version_key): stable, the input stays.
func SortVersions(versions []string) ([]string, error) {
	ks, err := keys(versions)
	if err != nil {
		return nil, err
	}
	order := make([]int, len(versions))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return ks[a].Compare(ks[b]) })
	out := make([]string, len(order))
	for i, at := range order {
		out[i] = versions[at]
	}
	return out, nil
}

// HighestVersion is highest_version: the highest version without a
// pre-release part, the highest of all when there is none. The first of
// equal keys wins, as max() keeps it.
func HighestVersion(versions []string) (string, error) {
	var releases []string
	for _, v := range versions {
		core, _, _ := strings.Cut(v, "+")
		if !strings.Contains(core, "-") {
			releases = append(releases, v)
		}
	}
	names := releases
	if len(names) == 0 {
		names = versions
	}
	if len(names) == 0 {
		return "", &pyjson.Error{Class: "ValueError", Message: "max() iterable argument is empty"}
	}
	best, bestKey := "", Key{}
	for i, v := range names {
		k, err := VersionKey(v)
		if err != nil {
			return "", err
		}
		if i == 0 || k.Compare(bestKey) > 0 {
			best, bestKey = v, k
		}
	}
	return best, nil
}

// IsoTime is iso_time: datetime.fromtimestamp(ms / 1000, tz=utc) written
// with strftime and the milliseconds of ms. A year outside 1 to 9999 is the
// ValueError of datetime.
func IsoTime(createdMS int64) (string, error) {
	// ms / 1000 is the true quotient rounded once to a float; fromtimestamp
	// splits it into seconds and microseconds rounded half to even.
	d, _ := new(big.Rat).SetFrac(big.NewInt(createdMS), big.NewInt(1000)).Float64()
	whole, frac := math.Modf(d)
	micros := math.RoundToEven(frac * 1e6)
	switch {
	case micros >= 1e6:
		whole++
	case micros < 0:
		whole--
	}
	when := time.Unix(int64(whole), 0).UTC()
	year := when.Year()
	if year < 1 || year > 9999 {
		return "", &pyjson.Error{Class: "ValueError", Message: fmt.Sprintf("year %d is out of range", year)}
	}
	millis := createdMS % 1000
	if millis < 0 {
		millis += 1000 // Python's % has the sign of the divisor
	}
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d.%03dZ", year, when.Month(), when.Day(),
		when.Hour(), when.Minute(), when.Second(), millis), nil
}
