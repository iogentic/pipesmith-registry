package admin

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"
)

// The criteria of a cleanup policy that this server knows (decision A9).
const (
	CriterionLastBlobUpdatedDays = "lastBlobUpdatedDays"
	CriterionLastDownloadedDays  = "lastDownloadedDays"
)

var knownCriteria = []string{CriterionLastBlobUpdatedDays, CriterionLastDownloadedDays}

// maxDays is the largest number of days a float64 holds exactly.
const maxDays = 1 << 53

// CheckCriteria checks the criteria of a create, save or preview of a
// cleanup policy, a decoded object: each known criterion is absent, null or
// a whole number of 1 or more, and at least one is set. A criterion of
// another name, and a value that is no such number, are refused with 400 at
// criteria.<name>, in the order of the names; a policy with no criterion
// set, and no other refusal, with 400 at criteria. stored holds the
// criteria of the stored policy on a save, and is nil for a create and a
// preview. A save never refuses a value the stored policy holds (ruling
// R11b): a criterion whose value equals the stored value of its name passes,
// whatever its name, and the refusal at criteria does not come when the
// criteria equal the stored ones key by key.
func CheckCriteria(criteria, stored map[string]any) error {
	var list Refusals
	set := 0
	for _, name := range slices.Sorted(maps.Keys(criteria)) {
		value := criteria[name]
		held, ok := stored[name]
		kept := ok && sameValue(value, held)
		switch {
		case !slices.Contains(knownCriteria, name):
			if !kept {
				list = append(list, &Refusal{Code: CodeCriterionUnknown, Field: "criteria." + name, Message: fmt.Sprintf(
					"the cleanup criterion %s is not known to this server", name)})
			}
		case value == nil:
		case !wholeDays(value):
			if !kept {
				list = append(list, &Refusal{Code: CodeCriterionValue, Field: "criteria." + name, Message: fmt.Sprintf(
					"the cleanup criterion %s needs a whole number of days, 1 or more", name)})
			}
		default:
			set++
		}
	}
	if len(list) == 0 && set == 0 && !(stored != nil && sameObject(criteria, stored)) {
		list = append(list, &Refusal{Code: CodeCriteriaMissing, Field: "criteria",
			Message: "a cleanup policy needs at least one criterion"})
	}
	return list.err()
}

// Runnable reports whether a stored cleanup policy may run, and names its
// criteria of another name, sorted. A policy of an import that holds such a
// criterion is kept and listed and never runs; so is a policy whose known
// criteria the request check refuses.
func Runnable(criteria map[string]any) (bool, []string) {
	unknown := []string{}
	for name := range criteria {
		if !slices.Contains(knownCriteria, name) {
			unknown = append(unknown, name)
		}
	}
	slices.Sort(unknown)
	return len(unknown) == 0 && CheckCriteria(criteria, nil) == nil, unknown
}

// wholeDays reports whether a value is a whole number of 1 or more: a
// number of the JSON decoder, as float64 or json.Number, or a Go integer.
func wholeDays(v any) bool {
	switch x := v.(type) {
	case int:
		return x >= 1 && int64(x) <= maxDays
	case int64:
		return x >= 1 && x <= maxDays
	case float64:
		return x >= 1 && x <= maxDays && x == math.Trunc(x)
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n >= 1 && n <= maxDays
		}
		f, err := x.Float64()
		return err == nil && f >= 1 && f <= maxDays && f == math.Trunc(f)
	}
	return false
}

// sameValue reports whether two decoded values are equal, so that the two
// JSON decoders agree: numbers (float64, json.Number, a Go integer) are
// compared by their exact value, objects and lists key by key and item by
// item, every other value as it is.
func sameValue(a, b any) bool {
	if x, ok := exactNumber(a); ok {
		y, ok := exactNumber(b)
		return ok && x.Cmp(y) == 0
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		return ok && sameObject(x, y)
	case []any:
		y, ok := b.([]any)
		return ok && slices.EqualFunc(x, y, sameValue)
	case nil, string, bool:
		return a == b
	}
	return false
}

// sameObject reports whether two objects hold the same keys with equal
// values.
func sameObject(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		other, ok := b[key]
		if !ok || !sameValue(value, other) {
			return false
		}
	}
	return true
}

// exactNumber gives the exact value of a decoded number.
func exactNumber(v any) (*big.Rat, bool) {
	switch x := v.(type) {
	case int:
		return new(big.Rat).SetInt64(int64(x)), true
	case int64:
		return new(big.Rat).SetInt64(x), true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, false
		}
		return new(big.Rat).SetFloat64(x), true
	case json.Number:
		return new(big.Rat).SetString(string(x))
	}
	return nil, false
}
