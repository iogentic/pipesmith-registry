package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

func TestCriteriaVectors(t *testing.T) {
	var file struct {
		Family string `json:"family"`
		Note   string `json:"note"`
		Cases  []struct {
			Row      string          `json:"row"`
			Op       string          `json:"op"` // request, save, stored
			Criteria json.RawMessage `json:"criteria"`
			Stored   json.RawMessage `json:"stored"`
			Runs     bool            `json:"runs"`
			Unknown  []string        `json:"unknown"`
			Refusals *[]wantRefusal  `json:"refusals"`
		} `json:"cases"`
	}
	readJSON(t, "criteria", &file)
	counts := tally{}
	for i, c := range file.Cases {
		for _, numbers := range []bool{false, true} {
			label := fmt.Sprintf("case %d (%s %s %s, json.Number %v)", i, c.Row, c.Op, c.Criteria, numbers)
			criteria := decodeCriteria(t, label, c.Criteria, numbers)
			switch c.Op {
			case "request":
				checkRefusals(t, label, CheckCriteria(criteria, nil), c.Refusals)
			case "save":
				stored := decodeCriteria(t, label, c.Stored, !numbers)
				if stored == nil {
					t.Fatalf("%s: a save needs stored criteria", label)
				}
				checkRefusals(t, label, CheckCriteria(criteria, stored), c.Refusals)
			case "stored":
				runs, unknown := Runnable(criteria)
				if runs != c.Runs || !slices.Equal(unknown, c.Unknown) {
					t.Errorf("%s: runs %v, unknown %q; want %v, %q", label, runs, unknown, c.Runs, c.Unknown)
				}
			default:
				t.Fatalf("%s: unknown op", label)
			}
		}
		switch {
		case c.Op == "stored":
			counts.add(c.Row, !c.Runs)
		case c.Refusals != nil:
			counts.add(c.Row, len(*c.Refusals) > 0)
		}
	}
	counts.check(t, "criteria", "R11", "R6")
}

// decodeCriteria decodes a criteria object, with numbers as json.Number
// when numbers is set, else as float64. Absent and null give nil.
func decodeCriteria(t *testing.T, label string, raw json.RawMessage, numbers bool) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var criteria map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if numbers {
		dec.UseNumber()
	}
	if err := dec.Decode(&criteria); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	return criteria
}

// A value of one decoder equals the same value of the other; a value of
// another type or another number does not.
func TestSameValue(t *testing.T) {
	equal := [][2]any{{30.0, json.Number("30")}, {json.Number("30.0"), 30.0}, {json.Number("30"), json.Number("3e1")},
		{1, json.Number("1")}, {int64(7), 7.0}, {nil, nil}, {"x", "x"}, {true, true},
		{map[string]any{"a": []any{1.0}}, map[string]any{"a": []any{json.Number("1")}}},
		{json.Number("9007199254740993"), json.Number("9007199254740993")}}
	for _, p := range equal {
		if !sameValue(p[0], p[1]) || !sameValue(p[1], p[0]) {
			t.Errorf("%v (%T) and %v (%T) are equal", p[0], p[0], p[1], p[1])
		}
	}
	differ := [][2]any{{30.0, json.Number("31")}, {"3", 3.0}, {nil, 0.0}, {true, 1.0},
		{map[string]any{"a": 1.0}, map[string]any{"a": 1.0, "b": nil}}, {[]any{1.0}, []any{1.0, 2.0}},
		{json.Number("9007199254740993"), 9007199254740992.0}}
	for _, p := range differ {
		if sameValue(p[0], p[1]) || sameValue(p[1], p[0]) {
			t.Errorf("%v (%T) and %v (%T) differ", p[0], p[0], p[1], p[1])
		}
	}
}

// A whole number of days comes as a decoded number or as a Go integer.
func TestWholeDays(t *testing.T) {
	for _, v := range []any{1, int64(30), 7.0, json.Number("365"), json.Number("2.0")} {
		if !wholeDays(v) {
			t.Errorf("%v (%T) is a whole number of days", v, v)
		}
	}
	for _, v := range []any{0, int64(-3), 0.5, json.Number("1.5"), json.Number("x"), "3", false, nil, 1e300} {
		if wholeDays(v) {
			t.Errorf("%v (%T) is no whole number of 1 or more", v, v)
		}
	}
}
