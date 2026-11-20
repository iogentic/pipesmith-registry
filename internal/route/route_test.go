package route

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

type testRule struct{ name string }

func (r testRule) Empty() bool { return r.name == "" }

type testHandle func() string

func entry(pattern string, methods []string, rule string, name string) Entry[testRule, testHandle] {
	return Entry[testRule, testHandle]{
		Pattern: regexp.MustCompile(pattern),
		Methods: methods,
		Rule:    testRule{rule},
		Handle:  func() string { return name },
	}
}

func TestNewRefusesAnEntryWithoutRule(t *testing.T) {
	good := entry(`^/a$`, []string{"GET"}, "public", "a")
	cases := map[string]Entry[testRule, testHandle]{
		"empty rule":   entry(`^/b$`, []string{"GET"}, "", "b"),
		"no pattern":   {Methods: []string{"GET"}, Rule: testRule{"public"}, Handle: func() string { return "c" }},
		"no method":    entry(`^/d$`, nil, "public", "d"),
		"empty method": entry(`^/e$`, []string{""}, "public", "e"),
		"no handler":   {Pattern: regexp.MustCompile(`^/f$`), Methods: []string{"GET"}, Rule: testRule{"public"}},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			table, err := New([]Entry[testRule, testHandle]{good}, []Entry[testRule, testHandle]{bad})
			if err == nil || table != nil {
				t.Fatalf("New accepted an entry with %s", name)
			}
			if !strings.Contains(err.Error(), "entry 2") {
				t.Errorf("the error %q does not name the entry by its place", err)
			}
		})
	}
}

func TestMustNewPanicsOnAnEmptyRule(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustNew did not panic on an empty rule")
		}
	}()
	MustNew([]Entry[testRule, testHandle]{entry(`^/x$`, []string{"GET"}, "", "x")})
}

func TestNewKeepsThePartsInOrder(t *testing.T) {
	a := entry(`^/a$`, []string{"GET"}, "public", "a")
	b := entry(`^/b$`, []string{"GET"}, "public", "b")
	c := entry(`^/c$`, []string{"GET"}, "public", "c")
	table := MustNew([]Entry[testRule, testHandle]{a, b}, nil, []Entry[testRule, testHandle]{c})
	var names []string
	for _, e := range table.Entries() {
		names = append(names, e.Handle())
	}
	if !slices.Equal(names, []string{"a", "b", "c"}) {
		t.Fatalf("entries %v, want a, b, c", names)
	}
	if table.Len() != 3 {
		t.Fatalf("Len %d, want 3", table.Len())
	}
}

func TestEntriesIsACopy(t *testing.T) {
	table := MustNew([]Entry[testRule, testHandle]{entry(`^/a$`, []string{"GET"}, "public", "a")})
	list := table.Entries()
	list[0].Methods[0] = "POST"
	list[0].Rule = testRule{}
	again := table.Entries()
	if again[0].Methods[0] != "GET" || again[0].Rule.Empty() {
		t.Fatal("a change of the returned list changed the table")
	}
}

func TestMatchTakesTheFirstEntryWithTheMethod(t *testing.T) {
	table := MustNew([]Entry[testRule, testHandle]{
		entry(`^/item/([^/]+)$`, []string{"PUT"}, "admin", "put"),
		entry(`^/item/(special)$`, []string{"GET"}, "public", "special"),
		entry(`^/item/([^/]+)$`, []string{"GET", "HEAD"}, "public", "get"),
		entry(`^/item/(.*)$`, []string{"GET"}, "public", "late"),
	})
	cases := []struct {
		method, path, want string
		match              []string
	}{
		{"GET", "/item/special", "special", []string{"/item/special", "special"}},
		{"GET", "/item/one", "get", []string{"/item/one", "one"}},
		{"HEAD", "/item/one", "get", []string{"/item/one", "one"}},
		{"PUT", "/item/one", "put", []string{"/item/one", "one"}},
		{"GET", "/item/a/b", "late", []string{"/item/a/b", "a/b"}},
	}
	for _, c := range cases {
		e, match, allow := table.Match(c.method, c.path)
		if e == nil {
			t.Errorf("%s %s: no entry", c.method, c.path)
			continue
		}
		if got := e.Handle(); got != c.want || !slices.Equal(match, c.match) || allow != nil {
			t.Errorf("%s %s: entry %s, match %q, allow %v; want %s, %q, nil", c.method, c.path, got, match, allow, c.want, c.match)
		}
	}
}

// A path whose patterns match only with other methods gives the methods of
// the first such entry, as the 405 with its Allow header needs them.
func TestMatchGivesTheMethodsOfTheFirstPartialEntry(t *testing.T) {
	table := MustNew([]Entry[testRule, testHandle]{
		entry(`^/w$`, []string{"PUT", "POST"}, "admin", "write"),
		entry(`^/w$`, []string{"DELETE"}, "admin", "delete"),
		entry(`^/r$`, []string{"GET"}, "public", "read"),
	})
	e, match, allow := table.Match("GET", "/w")
	if e != nil || match != nil || !slices.Equal(allow, []string{"PUT", "POST"}) {
		t.Fatalf("GET /w: entry %v, match %q, allow %v; want nil, nil, [PUT POST]", e != nil, match, allow)
	}
	e, match, allow = table.Match("GET", "/none")
	if e != nil || match != nil || allow != nil {
		t.Fatalf("GET /none: entry %v, match %q, allow %v; want all nil", e != nil, match, allow)
	}
}

func TestMatchesAnyMethod(t *testing.T) {
	table := MustNew([]Entry[testRule, testHandle]{entry(`^/w/$`, []string{"PUT"}, "admin", "write")})
	if !table.Matches("/w/") || table.Matches("/w") {
		t.Fatal("Matches must ask the patterns alone")
	}
}

func TestChangesState(t *testing.T) {
	cases := []struct {
		methods []string
		want    bool
	}{
		{[]string{"GET"}, false},
		{[]string{"GET", "HEAD"}, false},
		{[]string{"OPTIONS"}, false},
		{[]string{"PUT"}, true},
		{[]string{"GET", "POST"}, true},
		{[]string{"DELETE"}, true},
		{[]string{"PATCH"}, true},
		{nil, false},
	}
	for _, c := range cases {
		if got := ChangesState(c.methods); got != c.want {
			t.Errorf("ChangesState(%v) = %v, want %v", c.methods, got, c.want)
		}
		e := entry(`^/x$`, c.methods, "public", "x")
		if got := e.ChangesState(); got != c.want {
			t.Errorf("Entry.ChangesState with %v = %v, want %v", c.methods, got, c.want)
		}
	}
}

func TestChanging(t *testing.T) {
	table := MustNew([]Entry[testRule, testHandle]{
		entry(`^/a$`, []string{"GET"}, "public", "a"),
		entry(`^/b$`, []string{"GET", "POST"}, "admin", "b"),
		entry(`^/c$`, []string{"DELETE"}, "admin", "c"),
	})
	var names []string
	for _, e := range table.Changing() {
		names = append(names, e.Handle())
	}
	if !slices.Equal(names, []string{"b", "c"}) {
		t.Fatalf("Changing gave %v, want b, c", names)
	}
}
