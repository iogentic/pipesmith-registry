package repos_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/repos"
)

// WholeNumber reads the integers the old product writes as floating point
// numbers exactly, and refuses a fraction, a value outside int64 and
// anything that is no JSON number.
func TestWholeNumber(t *testing.T) {
	for _, c := range []struct {
		in   any
		want int64
		err  error
	}{
		{json.Number("14511"), 14511, nil},
		{json.Number("14511.0"), 14511, nil},
		{json.Number("14511.000"), 14511, nil},
		{json.Number("-3.0"), -3, nil},
		{json.Number("-0.0"), 0, nil},
		{json.Number("1440E-1"), 144, nil},
		{json.Number("5.36870912E9"), 5368709120, nil},
		{json.Number("5.36870912e+9"), 5368709120, nil},
		{json.Number("0.0e999999999999999999999"), 0, nil},
		{json.Number("9223372036854775807.0"), math.MaxInt64, nil},
		{json.Number("-9223372036854775808"), math.MinInt64, nil},
		{json.Number("-9.223372036854775808E18"), math.MinInt64, nil},
		{json.Number("14511.5"), 0, repos.ErrFraction},
		{json.Number("100.10"), 0, repos.ErrFraction},
		{json.Number("1e-1"), 0, repos.ErrFraction},
		{json.Number("123456789012345678901e-2"), 0, repos.ErrFraction},
		{json.Number("1e-999999999999999999999"), 0, repos.ErrFraction},
		{json.Number("9223372036854775808"), 0, repos.ErrRange},
		{json.Number("9.3e18"), 0, repos.ErrRange},
		{json.Number("1e400"), 0, repos.ErrRange},
		{json.Number("1e999999999999999999999"), 0, repos.ErrRange},
		{json.Number("1/2"), 0, repos.ErrNoNumber},
		{json.Number("0x10"), 0, repos.ErrNoNumber},
		{json.Number(""), 0, repos.ErrNoNumber},
		{"14511", 0, repos.ErrNoNumber},
		{true, 0, repos.ErrNoNumber},
		{nil, 0, repos.ErrNoNumber},
		{[]any{json.Number("1")}, 0, repos.ErrNoNumber},
	} {
		got, err := repos.WholeNumber(c.in)
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("WholeNumber(%#v) = %d, %v; want %d, %v", c.in, got, err, c.want, c.err)
		}
	}
}

// The texts of the errors are part of the refusals of the import.
func TestWholeNumberErrorTexts(t *testing.T) {
	for err, want := range map[error]string{
		repos.ErrNoNumber: "is no number",
		repos.ErrFraction: "has a fraction",
		repos.ErrRange:    "is outside the range of a 64 bit integer",
	} {
		if err.Error() != want {
			t.Errorf("error %q, want %q", err, want)
		}
	}
}
