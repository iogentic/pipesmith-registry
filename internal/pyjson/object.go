// Package pyjson reads and writes JSON byte for byte as Python's json module
// does, in the forms the Python server uses: json.dumps with and without an
// indent, Starlette's JSONResponse, and json.loads of bytes and of text.
//
// Python strings are Go strings in UTF-8. A lone surrogate, which a Python
// string can hold, is the three byte sequence that UTF-8 would give it
// (ED A0 80 for U+D800). Decode writes surrogates that way; Encode reads them
// back and reads a byte that is not UTF-8 as Python's surrogateescape does
// (byte 0xFF is U+DCFF).
//
// Decoded values are *Object, []any, string, bool, nil, int64 (an integer
// that fits), *big.Int (one that does not) and float64.
package pyjson

import "iter"

// Object is a JSON object with its keys in insertion order, as a Python dict.
// The zero value is an empty object. Use it through a pointer.
type Object struct {
	keys   []string
	values map[string]any
}

// NewObject returns an empty object.
func NewObject() *Object {
	return &Object{}
}

// Set stores value under key and returns o. A key that exists keeps its
// position, as in a Python dict.
func (o *Object) Set(key string, value any) *Object {
	if o.values == nil {
		o.values = map[string]any{}
	}
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
	return o
}

// Get returns the value under key and whether the key exists.
func (o *Object) Get(key string) (any, bool) {
	value, ok := o.values[key]
	return value, ok
}

// Has reports whether key exists.
func (o *Object) Has(key string) bool {
	_, ok := o.values[key]
	return ok
}

// Delete removes key. Setting it again puts it at the end.
func (o *Object) Delete(key string) {
	if _, ok := o.values[key]; !ok {
		return
	}
	delete(o.values, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// Len returns the number of keys.
func (o *Object) Len() int {
	return len(o.keys)
}

// Keys returns the keys in order.
func (o *Object) Keys() []string {
	return append([]string(nil), o.keys...)
}

// All yields the keys and values in order.
func (o *Object) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for _, key := range o.keys {
			if !yield(key, o.values[key]) {
				return
			}
		}
	}
}
