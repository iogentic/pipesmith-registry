package repos

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// The errors of WholeNumber. Their texts are part of the refusals of the
// import.
var (
	ErrNoNumber = errors.New("is no number")
	ErrFraction = errors.New("has a fraction")
	ErrRange    = errors.New("is outside the range of a 64 bit integer")

	jsonNumber = regexp.MustCompile(`^(-?)(0|[1-9][0-9]*)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)
)

// WholeNumber reads a JSON number that holds an integer. The old product
// stores many integers as floating point values, written with a decimal
// point or an exponent (14511.0, 5.36870912E9); a zero fraction reads as the
// integer. The value is exact: a fraction however small, a value outside
// int64 and anything that is no JSON number are errors.
func WholeNumber(v any) (int64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, ErrNoNumber
	}
	if i, err := n.Int64(); err == nil {
		return i, nil
	}
	m := jsonNumber.FindStringSubmatch(string(n))
	if m == nil {
		return 0, ErrNoNumber
	}
	sign, fraction, exponent := m[1], m[3], m[4]
	digits := strings.TrimLeft(m[2]+fraction, "0")
	if digits == "" {
		return 0, nil
	}
	// value = digits * 10^shift
	shift := -int64(len(fraction))
	if exponent != "" {
		e, err := strconv.ParseInt(exponent, 10, 64)
		if err != nil || e > 1<<40 || e < -(1<<40) {
			if strings.HasPrefix(exponent, "-") {
				return 0, ErrFraction
			}
			return 0, ErrRange
		}
		shift += e
	}
	significant := strings.TrimRight(digits, "0")
	shift += int64(len(digits) - len(significant))
	if shift < 0 {
		return 0, ErrFraction
	}
	if int64(len(significant))+shift > 19 {
		return 0, ErrRange
	}
	i, err := strconv.ParseInt(sign+significant+strings.Repeat("0", int(shift)), 10, 64)
	if err != nil {
		return 0, ErrRange
	}
	return i, nil
}
