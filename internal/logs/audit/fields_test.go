package audit_test

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/logs"
	"github.com/iogentic/pipesmith-registry/internal/logs/audit"
	"github.com/iogentic/pipesmith-registry/internal/logship"
	"github.com/iogentic/pipesmith-registry/internal/state"
)

// Task 7.17: a walk by reflection over every record type of internal/state.
// A field whose name says it holds a password, a hash, a token, a key, a
// digest or a secret carries audit:"secret"; each type has at least one
// field an audit line can name; a line built from two values of a type
// that differ in every field names every field but the secret ones and
// holds no value of a secret field.

// secretName is a Go field name that says the field holds a secret.
var secretName = regexp.MustCompile(`(?i)password|hash|token|secret|digest|key`)

// notSecret are fields whose name matches secretName and that hold no
// secret, with the reason.
var notSecret = map[string]string{
	"model.LDAPServer.UserPasswordAttribute": "the name of a directory attribute, not its value",
}

// recordTypes are the struct types of this module that the methods of
// *state.Store take or give, with every struct type of their fields, and
// the rows of the settings logs and log_targets, which the store keeps as
// JSON.
func recordTypes() []reflect.Type {
	seen := map[reflect.Type]bool{}
	var out []reflect.Type
	var visit func(t reflect.Type)
	visit = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
			if t.Kind() == reflect.Map {
				visit(t.Key())
			}
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || seen[t] || !strings.Contains(t.PkgPath(), "/internal/") {
			return
		}
		seen[t] = true
		out = append(out, t)
		for i := range t.NumField() {
			visit(t.Field(i).Type)
		}
	}
	store := reflect.TypeFor[*state.Store]()
	for i := range store.NumMethod() {
		m := store.Method(i).Type
		for j := 1; j < m.NumIn(); j++ { // In(0) is the store itself
			visit(m.In(j))
		}
		for j := range m.NumOut() {
			visit(m.Out(j))
		}
	}
	visit(reflect.TypeFor[logship.Target]())
	visit(reflect.TypeFor[logs.Settings]())
	return out
}

// fill sets every exported field of v to a value of its own, a text
// "canary-<n>" for each string, and returns the texts of the secret fields.
func fill(v reflect.Value, n *int, secret bool, secrets *[]string) {
	*n++
	switch v.Kind() {
	case reflect.String:
		text := fmt.Sprintf("canary-%03d-value", *n)
		v.SetString(text)
		if secret {
			*secrets = append(*secrets, text)
		}
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(4000 + *n))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(200 + *n%50))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(*n) + 0.5)
	case reflect.Array:
		for i := range v.Len() {
			fill(v.Index(i), n, false, secrets)
		}
		if secret {
			b := make([]byte, 0, v.Len())
			for i := range v.Len() {
				if v.Index(i).Kind() == reflect.Uint8 {
					b = append(b, byte(v.Index(i).Uint()))
				}
			}
			*secrets = append(*secrets, hex.EncodeToString(b), base64.StdEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b))
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0), n, secret, secrets)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k, n, secret, secrets)
		if e.Kind() == reflect.Interface {
			e.Set(reflect.ValueOf(fmt.Sprintf("canary-%03d-any", *n)))
		} else {
			fill(e, n, secret, secrets)
		}
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(p.Elem(), n, secret, secrets)
		v.Set(p)
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(time.Date(2026, 11, 20, 10, 0, *n%60, 0, time.UTC)))
			return
		}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if f.IsExported() {
				fill(v.Field(i), n, secret || audit.Secret(f), secrets)
			}
		}
	}
}

func TestSecretFieldsOfEveryRecordType(t *testing.T) {
	types := recordTypes()
	var names []string
	for _, rt := range types {
		names = append(names, rt.String())
	}
	for _, want := range []string{"model.User", "model.Token", "model.ImportedKey", "model.LDAPServer", "model.Repository",
		"state.TaskRun", "logship.Target", "logs.Settings"} {
		if !slices.Contains(names, want) {
			t.Fatalf("the walk misses %s: %v", want, names)
		}
	}
	tagged := 0
	for _, rt := range types {
		audited := 0
		for i := range rt.NumField() {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			full := rt.String() + "." + f.Name
			switch {
			case audit.Secret(f):
				tagged++
			case secretName.MatchString(f.Name) && notSecret[full] == "":
				t.Errorf("%s holds a secret by its name and has no tag %s:%q", full, audit.TagName, audit.TagSecret)
			default:
				audited++
			}
		}
		if audited == 0 && rt.NumField() > 0 {
			t.Errorf("%s has no field an audit line can name", rt)
		}
		var before, after = reflect.New(rt).Elem(), reflect.New(rt).Elem()
		var n int
		var secrets []string
		fill(after, &n, false, &secrets)
		changed := audit.Changed(before.Interface(), after.Interface())
		for i := range rt.NumField() {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			named := slices.ContainsFunc(changed, func(c string) bool {
				return c == audit.FieldName(f) || strings.HasPrefix(c, audit.FieldName(f)+".")
			})
			if audit.Secret(f) && named {
				t.Errorf("%s.%s is secret and the line names it: %v", rt, f.Name, changed)
			}
			if !audit.Secret(f) && !named {
				t.Errorf("%s.%s changed and the line does not name it: %v", rt, f.Name, changed)
			}
		}
		line := string(logs.AuditLine(audit.Capped(logs.AuditEvent{Subject: audit.Subject, Action: "record.update",
			ObjectType: rt.Name(), Result: audit.OK, Fields: changed}), time.Now()))
		for _, secret := range secrets {
			if strings.Contains(line, secret) {
				t.Errorf("%s: the line holds the secret value %q: %s", rt, secret, line)
			}
		}
	}
	if tagged < 4 {
		t.Errorf("%d secret fields tagged, want at least the four of User, Token, ImportedKey and logship.Target", tagged)
	}
	t.Logf("%d record types walked: %v; %d secret fields", len(types), names, tagged)
}
