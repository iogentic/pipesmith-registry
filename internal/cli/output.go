package cli

import (
	"fmt"
	"io"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mirosso25/pipesmith-registry/internal/pyjson"
	"github.com/mirosso25/pipesmith-registry/internal/pystr"
)

// size is _size: B below 1000, else one decimal of kB, MB, GB or TB.
func size(count *big.Int) string {
	if count == nil {
		return "-"
	}
	value, _ := new(big.Float).SetInt(count).Float64()
	for _, unit := range []string{"B", "kB", "MB", "GB", "TB"} {
		if value < 1000 || unit == "TB" {
			if unit == "B" {
				return fmt.Sprintf("%d B", int64(value))
			}
			return strconv.FormatFloat(value, 'f', 1, 64) + " " + unit
		}
		value /= 1000
	}
	return count.String() + " B"
}

func sizeOf(count int64) string { return size(big.NewInt(count)) }

// timeText is _time: the UTC minute of a creation time, "-" for none, zero
// or one outside the years 1 to 9999. The float steps are those of
// datetime.fromtimestamp.
func timeText(ms *int64) string {
	if ms == nil || *ms == 0 {
		return "-"
	}
	t := float64(*ms) / 1000
	whole, frac := math.Modf(t)
	frac = math.RoundToEven(frac * 1e6)
	if frac >= 1e6 {
		whole++
	} else if frac < 0 {
		whole--
	}
	if whole < -9.2e18 || whole > 9.2e18 {
		return "-"
	}
	when := time.Unix(int64(whole), 0).UTC()
	if when.Year() < 1 || when.Year() > 9999 {
		return "-"
	}
	return when.Format("2006-01-02 15:04")
}

// width is len() of a str: code points, an undecodable byte counting as one.
func width(s string) int {
	return len(pystr.Runes(s))
}

func rstrip(s string) string {
	for s != "" {
		r, size := utf8.DecodeLastRuneInString(s)
		if r == utf8.RuneError && size <= 1 || !pystr.IsSpace(r) {
			break
		}
		s = s[:len(s)-size]
	}
	return s
}

// table is _table: columns padded to their widest cell, the right ones
// right aligned, the last one not padded, trailing white space cut. A line
// with a lone surrogate fails as print() fails on the strict UTF-8 standard
// output of Python: the lines before it are written, the error is returned.
func table(w io.Writer, rows [][]string, right ...int) error {
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for column, cell := range row {
			widths[column] = max(widths[column], width(cell))
		}
	}
	for _, row := range rows {
		cells := make([]string, len(row))
		for column, cell := range row {
			pad := strings.Repeat(" ", widths[column]-width(cell))
			switch {
			case slices.Contains(right, column):
				cells[column] = pad + cell
			case column == len(row)-1:
				cells[column] = cell
			default:
				cells[column] = cell + pad
			}
		}
		line := rstrip(strings.Join(cells, "  "))
		if err := encodable(line); err != nil {
			return err
		}
		fmt.Fprintln(w, line)
	}
	return nil
}

// encodable is str.encode("utf-8") of Python: the first run of surrogates
// fails it. Positions count code points.
func encodable(s string) error {
	runes := pystr.Runes(s)
	for i, r := range runes {
		if !isSurrogate(r) {
			continue
		}
		end := i + 1
		for end < len(runes) && isSurrogate(runes[end]) {
			end++
		}
		message := fmt.Sprintf(`'utf-8' codec can't encode character '\u%04x' in position %d: surrogates not allowed`, r, i)
		if end > i+1 {
			message = fmt.Sprintf("'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed", i, end-1)
		}
		return &pyjson.Error{Class: "UnicodeEncodeError", Message: message}
	}
	return nil
}

func isSurrogate(r rune) bool {
	return r >= 0xD800 && r <= 0xDFFF
}

// thousands is format(n, ",").
func thousands(n int) string {
	text := strconv.Itoa(n)
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign, text = "-", text[1:]
	}
	var b strings.Builder
	for i, c := range text {
		if i > 0 && (len(text)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return sign + b.String()
}
