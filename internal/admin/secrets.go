package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Shown is how an answer shows a secret field.
type Shown string

// How an answer shows a secret field.
const (
	ShownNull   Shown = "null"   // the key is in the answer with the value null
	ShownAbsent Shown = "absent" // the key is left out
	ShownCreate Shown = "create" // the answer of the create holds it once, every other answer leaves it out
)

// SecretField is a secret field of a contract body (decision A11): the kind
// of record, the dot path into the body, and how an answer shows it.
type SecretField struct {
	Kind  Kind
	Path  string
	Shown Shown
}

// secretFields are the secret fields of the contract bodies. The two
// repository fields are present with null, as in the mock of the
// interface, which also leaves the LDAP password out.
var secretFields = []SecretField{
	{KindRepository, "httpClient.authentication.password", ShownNull},
	{KindRepository, "httpClient.authentication.bearerToken", ShownNull},
	{KindLDAPConnection, "authPassword", ShownAbsent},
	{KindLDAPLoginCheck, "connection.authPassword", ShownAbsent},
	{KindLDAPLoginCheck, "password", ShownAbsent},
	{KindUser, "password", ShownAbsent},
	{KindAccount, "currentPassword", ShownAbsent},
	{KindAccount, "newPassword", ShownAbsent},
	{KindToken, "secret", ShownCreate},
}

// SecretFields lists every secret field of a contract body.
func SecretFields() []SecretField { return slices.Clone(secretFields) }

// secretWords mark a secret key wherever it occurs. They are the words of
// internal/importer; a test reads them from its source.
var secretWords = []string{"password", "secret", "token", "key", "passphrase", "credential"}

// Redact prepares a record of the kind, a decoded object in the shape of
// the contract, for an answer: a secret field shown as null is set to null
// where its parent object is present, every other secret field is left
// out. With create, a field the answer of the create shows stays. The
// record given is not changed.
func Redact(kind Kind, record map[string]any, create bool) map[string]any {
	out, _ := deepCopy(record).(map[string]any)
	for _, f := range secretFields {
		if f.Kind != kind || create && f.Shown == ShownCreate {
			continue
		}
		parent, key := parentOf(out, f.Path)
		if parent == nil {
			continue
		}
		if f.Shown == ShownNull {
			parent[key] = nil
		} else {
			delete(parent, key)
		}
	}
	return out
}

// Leaks walks the JSON form of an answer, of any Go value, and gives,
// sorted by key in each object, the path of every key that holds a secret
// with a value that is neither null nor empty: a key at a path of the
// table, or a key whose name holds a secret word. created names the kind
// whose create answer this is, or is empty; a field the table shows in the
// answer of the create of that kind may then hold its value at the top of
// the answer. A value without a JSON form gives one entry that says so. It
// is a guard for tests.
func Leaks(answer any, created Kind) []string {
	data, err := json.Marshal(answer)
	if err != nil {
		return []string{"no JSON form: " + err.Error()}
	}
	var decoded any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return []string{"no JSON form: " + err.Error()}
	}
	var found []string
	leaks(decoded, "", nil, created, &found)
	return found
}

func leaks(v any, path string, keys []string, created Kind, found *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(x)) {
			child := key
			if path != "" {
				child = path + "." + key
			}
			childKeys := append(slices.Clip(keys), key)
			if !empty(x[key]) && secretKey(childKeys) && !shownOnCreate(created, child) {
				*found = append(*found, child)
				continue
			}
			leaks(x[key], child, childKeys, created, found)
		}
	case []any:
		for i, item := range x {
			leaks(item, fmt.Sprintf("%s[%d]", path, i), keys, created, found)
		}
	}
}

// SecretKey reports whether one key holds a secret word or is the last key
// of a secret field of the table.
func SecretKey(key string) bool { return secretKey([]string{key}) }

// secretKey reports whether the keys from the top end with the path of a
// field of the table, or the last key holds a secret word.
func secretKey(keys []string) bool {
	last := strings.ToLower(keys[len(keys)-1])
	for _, word := range secretWords {
		if strings.Contains(last, word) {
			return true
		}
	}
	for _, f := range secretFields {
		path := strings.Split(f.Path, ".")
		if len(keys) >= len(path) && slices.Equal(keys[len(keys)-len(path):], path) {
			return true
		}
	}
	return false
}

// shownOnCreate reports whether the answer of the create of the kind shows
// the field at the path.
func shownOnCreate(created Kind, path string) bool {
	return created != "" && slices.ContainsFunc(secretFields, func(f SecretField) bool {
		return f.Kind == created && f.Shown == ShownCreate && f.Path == path
	})
}

// empty reports whether a decoded value is null, an empty string, an empty
// object or an empty list.
func empty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
}

// SecretSave is what a save does with a stored secret.
type SecretSave string

// What a save does with a stored secret.
const (
	KeepSecret    SecretSave = "keep"
	ReplaceSecret SecretSave = "replace"
)

// SecretOnSave says what a save of a repository does with the stored secret
// of a field of the table: a body that sends the field absent, null or
// empty keeps it, any other value replaces it. It stores nothing.
func SecretOnSave(body map[string]any, path string) SecretSave {
	parent, key := parentOf(body, path)
	if parent == nil {
		return KeepSecret
	}
	if v, ok := parent[key]; !ok || v == nil || v == "" {
		return KeepSecret
	}
	return ReplaceSecret
}

// parentOf gives the object that holds the last key of the dot path, and
// that key, or nil when an object on the way is missing.
func parentOf(m map[string]any, path string) (map[string]any, string) {
	keys := strings.Split(path, ".")
	for _, key := range keys[:len(keys)-1] {
		next, ok := m[key].(map[string]any)
		if !ok {
			return nil, ""
		}
		m = next
	}
	if m == nil {
		return nil, ""
	}
	return m, keys[len(keys)-1]
}

// deepCopy copies the objects and lists of a decoded value.
func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if x == nil {
			return x
		}
		out := make(map[string]any, len(x))
		for key, item := range x {
			out[key] = deepCopy(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = deepCopy(item)
		}
		return out
	}
	return v
}
