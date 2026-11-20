// Package route holds the route table of the server: entries of a pattern of
// the decoded path, the methods, the rule a caller has to meet and the
// handler, matched in their order. The table is built once by New, which
// refuses an entry without a rule, so no route can be served without one.
// The rule and the handler are types of the server; the table only asks a
// rule whether it is empty.
package route

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
)

// Rule is what a route asks of the caller. Empty is true for the zero rule,
// which no entry may carry.
type Rule interface{ Empty() bool }

// Entry is one route.
type Entry[R Rule, H any] struct {
	Pattern *regexp.Regexp
	Methods []string
	Rule    R
	Handle  H
}

// Table is the list of entries in the order they are matched.
type Table[R Rule, H any] struct{ entries []Entry[R, H] }

// New joins the parts in their order into one table. It refuses an entry
// without pattern, without a method, with an empty method, with an empty
// rule or without handler, and names the entry by its place, counted from 1.
func New[R Rule, H any](parts ...[]Entry[R, H]) (*Table[R, H], error) {
	var entries []Entry[R, H]
	for _, part := range parts {
		entries = append(entries, part...)
	}
	for i, e := range entries {
		if err := check(e); err != nil {
			return nil, fmt.Errorf("route: entry %d: %w", i+1, err)
		}
	}
	return &Table[R, H]{entries: entries}, nil
}

// MustNew is New for the tables of the server, which are fixed at build
// time: a refused entry is a defect of the program.
func MustNew[R Rule, H any](parts ...[]Entry[R, H]) *Table[R, H] {
	t, err := New(parts...)
	if err != nil {
		panic(err)
	}
	return t
}

func check[R Rule, H any](e Entry[R, H]) error {
	if e.Pattern == nil {
		return fmt.Errorf("no pattern")
	}
	if len(e.Methods) == 0 {
		return fmt.Errorf("%s has no method", e.Pattern)
	}
	if slices.Contains(e.Methods, "") {
		return fmt.Errorf("%s has an empty method", e.Pattern)
	}
	if e.Rule.Empty() {
		return fmt.Errorf("%s has no rule", e.Pattern)
	}
	if isNil(e.Handle) {
		return fmt.Errorf("%s has no handler", e.Pattern)
	}
	return nil
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Func, reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

// Entries is a copy of the entries in their order.
func (t *Table[R, H]) Entries() []Entry[R, H] {
	out := make([]Entry[R, H], len(t.entries))
	for i, e := range t.entries {
		e.Methods = slices.Clone(e.Methods)
		out[i] = e
	}
	return out
}

// Len is the number of entries.
func (t *Table[R, H]) Len() int { return len(t.entries) }

// Match finds the first entry whose pattern matches the path and whose
// methods hold the method, with the submatches of its pattern. Without one
// it gives the methods of the first entry whose pattern matched, for the
// 405 and its Allow header, or nil when no pattern matched.
func (t *Table[R, H]) Match(method, path string) (*Entry[R, H], []string, []string) {
	var allow []string
	for i := range t.entries {
		e := &t.entries[i]
		match := e.Pattern.FindStringSubmatch(path)
		if match == nil {
			continue
		}
		if slices.Contains(e.Methods, method) {
			return e, match, nil
		}
		if allow == nil {
			allow = slices.Clone(e.Methods)
		}
	}
	return nil, nil, allow
}

// Matches is whether the pattern of any entry matches the path, whatever
// its methods.
func (t *Table[R, H]) Matches(path string) bool {
	for i := range t.entries {
		if t.entries[i].Pattern.MatchString(path) {
			return true
		}
	}
	return false
}

// Changing are the entries that take a method which changes state, in
// their order (ChangesState).
func (t *Table[R, H]) Changing() []Entry[R, H] {
	var out []Entry[R, H]
	for _, e := range t.Entries() {
		if e.ChangesState() {
			out = append(out, e)
		}
	}
	return out
}

// ChangesState is whether the methods hold one besides GET, HEAD and
// OPTIONS.
func ChangesState(methods []string) bool {
	for _, m := range methods {
		switch m {
		case "GET", "HEAD", "OPTIONS":
		default:
			return true
		}
	}
	return false
}

// ChangesState is whether the entry takes a method which changes state.
func (e Entry[R, H]) ChangesState() bool { return ChangesState(e.Methods) }
