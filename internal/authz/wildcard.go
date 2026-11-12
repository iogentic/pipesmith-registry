// Package authz holds the rights of the old product: its wildcard rule, the
// permission string of each privilege kind, the built in roles and
// privileges, and the expansion of roles to permission strings.
//
// Sources: Apache Shiro shiro-root-1.13.0 WildcardPermission, and the
// privilege descriptors and security contributors of the public source of
// the old product, tag release-3.70.1-02, read 2026-11-12.
package authz

import (
	"errors"
	"slices"
	"strings"
)

// Permission is a parsed permission string: parts split by ':', each part a
// set of values split by ','.
type Permission struct {
	parts []part
}

type part struct {
	wild   bool     // the part holds the value *
	values []string // the values of the part, sorted, without repeats
}

// ErrSyntax is the error of Parse for a string that is no permission.
var ErrSyntax = errors.New("authz: not a permission string")

// Parse reads a permission string as Shiro does without case sensitivity:
// the string is trimmed and lower cased, a part of only commas or a string
// of only colons is refused, and trailing empty parts are dropped.
func Parse(s string) (Permission, error) {
	s = strings.ToLower(javaTrim(s))
	if s == "" {
		return Permission{}, ErrSyntax
	}
	texts := javaSplit(s, ":")
	if len(texts) == 0 {
		return Permission{}, ErrSyntax
	}
	parts := make([]part, 0, len(texts))
	for _, text := range texts {
		values := javaSplit(text, ",")
		if len(values) == 0 {
			return Permission{}, ErrSyntax
		}
		slices.Sort(values)
		values = slices.Compact(values)
		_, wild := slices.BinarySearch(values, "*")
		parts = append(parts, part{wild: wild, values: values})
	}
	return Permission{parts: parts}, nil
}

// javaTrim is String.trim of Java: it removes every character up to and
// including the space from both ends.
func javaTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })
}

// javaSplit is String.split of Java for a literal separator: without a match
// the whole string is the only element, else trailing empty elements are
// dropped.
func javaSplit(s, sep string) []string {
	elements := strings.Split(s, sep)
	if len(elements) == 1 {
		return elements
	}
	for len(elements) > 0 && elements[len(elements)-1] == "" {
		elements = elements[:len(elements)-1]
	}
	return elements
}

// containsAll reports whether every value of q is a value of p.
func (p part) containsAll(q part) bool {
	for _, v := range q.values {
		if _, found := slices.BinarySearch(p.values, v); !found {
			return false
		}
	}
	return true
}

// Implies reports whether p, as a granted permission, implies q.
func (p Permission) Implies(q Permission) bool {
	return p.implies(q, false)
}

// ImpliesSome reports whether p implies at least one permission that q
// describes when each part of q that holds * stands for any one value. It is
// the reading the interface contract gives a * in the permission of a screen.
func (p Permission) ImpliesSome(q Permission) bool {
	return p.implies(q, true)
}

func (p Permission) implies(q Permission, some bool) bool {
	if len(p.parts) == 0 || len(q.parts) == 0 {
		return false
	}
	for i, want := range q.parts {
		if i >= len(p.parts) {
			return true
		}
		have := p.parts[i]
		if have.wild || (some && want.wild) {
			continue
		}
		if !have.containsAll(want) {
			return false
		}
	}
	for _, extra := range p.parts[len(q.parts):] {
		if !extra.wild {
			return false
		}
	}
	return true
}

// String gives the parts joined by ':' and the values by ','.
func (p Permission) String() string {
	texts := make([]string, len(p.parts))
	for i, pt := range p.parts {
		texts[i] = strings.Join(pt.values, ",")
	}
	return strings.Join(texts, ":")
}

// Implies parses both strings and reports whether granted implies requested.
// A string that does not parse implies nothing and is implied by nothing.
func Implies(granted, requested string) bool {
	return impliesStrings(granted, requested, false)
}

// ImpliesSome is Permission.ImpliesSome on two strings.
func ImpliesSome(granted, requested string) bool {
	return impliesStrings(granted, requested, true)
}

func impliesStrings(granted, requested string, some bool) bool {
	q, err := Parse(requested)
	if err != nil {
		return false
	}
	p, err := Parse(granted)
	return err == nil && p.implies(q, some)
}

// AnyImplies reports whether one of the granted strings implies requested.
func AnyImplies(granted []string, requested string) bool {
	return anyImplies(granted, requested, false)
}

// AnyImpliesSome reports whether one of the granted strings implies some
// permission that requested describes.
func AnyImpliesSome(granted []string, requested string) bool {
	return anyImplies(granted, requested, true)
}

func anyImplies(granted []string, requested string, some bool) bool {
	q, err := Parse(requested)
	if err != nil {
		return false
	}
	for _, g := range granted {
		if p, err := Parse(g); err == nil && p.implies(q, some) {
			return true
		}
	}
	return false
}
