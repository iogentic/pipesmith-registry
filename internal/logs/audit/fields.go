package audit

import (
	"reflect"
	"slices"
	"strings"
	"unicode"
)

// TagName and TagSecret mark a field of a record that holds a password, a
// hash, a token, a key or the name of a secret file: audit:"secret". A line
// never names such a field and never holds its value.
const (
	TagName   = "audit"
	TagSecret = "secret"
)

// Secret reports whether f carries the tag of a secret field.
func Secret(f reflect.StructField) bool { return f.Tag.Get(TagName) == TagSecret }

// Changed are the names of the fields that differ between before and after,
// two values of one type, sorted: the JSON name of a field, else its Go name
// in snake case, joined by dots below the top. Secret fields and what they
// hold are skipped. Slices are compared element by element; a slice or map
// whose length or content differs, and a pointer that is nil on one side
// only, name the field itself.
func Changed(before, after any) []string {
	a, b := reflect.ValueOf(before), reflect.ValueOf(after)
	if !a.IsValid() || !b.IsValid() || a.Type() != b.Type() {
		return nil
	}
	names := map[string]bool{}
	walk("", a, b, names)
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func walk(prefix string, a, b reflect.Value, out map[string]bool) {
	mark := func() {
		if prefix != "" {
			out[prefix] = true
		}
	}
	switch a.Kind() {
	case reflect.Pointer, reflect.Interface:
		switch {
		case a.IsNil() && b.IsNil():
		case a.IsNil() || b.IsNil():
			mark()
		case a.Elem().Type() != b.Elem().Type():
			mark()
		default:
			walk(prefix, a.Elem(), b.Elem(), out)
		}
	case reflect.Struct:
		t := a.Type()
		exported := false
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			exported = true
			if Secret(f) {
				continue
			}
			walk(join(prefix, FieldName(f)), a.Field(i), b.Field(i), out)
		}
		if !exported && !reflect.DeepEqual(a.Interface(), b.Interface()) {
			mark() // a value such as time.Time
		}
	case reflect.Slice, reflect.Array:
		if !structLike(a.Type().Elem()) || a.Len() != b.Len() {
			if !reflect.DeepEqual(a.Interface(), b.Interface()) {
				mark()
			}
			return
		}
		for i := range a.Len() {
			walk(prefix, a.Index(i), b.Index(i), out)
		}
	default:
		if a.CanInterface() && !reflect.DeepEqual(a.Interface(), b.Interface()) {
			mark()
		}
	}
}

// structLike is a struct type, or a pointer to one.
func structLike(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// FieldName is the name of f in a line: its JSON name, else its Go name in
// snake case.
func FieldName(f reflect.StructField) string {
	if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
		return name
	}
	var b strings.Builder
	runes := []rune(f.Name)
	for i, r := range runes {
		if unicode.IsUpper(r) {
			if i > 0 && (unicode.IsLower(runes[i-1]) || i+1 < len(runes) && unicode.IsLower(runes[i+1])) {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
