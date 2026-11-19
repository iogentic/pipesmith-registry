package rest

import (
	"strings"
	"testing"
)

func TestSearchValueThatIsNoTextGetsA400(t *testing.T) {
	f := newFixture(t)
	for _, pair := range [][2]string{
		{"q", "\xff"}, {"name", "\xc3\x28"}, {"group", "\xed\xa0\x80"}, {"q", "a\x00b"}, {"repository", "\xff"},
		{"maven.groupId", "com\x00example"}, {"npm.scope", "@\xfe"}, {"version", "1.0\xc0"},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s=%q: panic %v", pair[0], pair[1], r)
				}
			}()
			got := message(t, mustRefuse(t, f, 400, pair[0], pair[1]))
			if !strings.Contains(got, pair[0]) {
				t.Errorf("%s=%q: message %q does not name the parameter", pair[0], pair[1], got)
			}
		}()
	}
	if comps, _ := search(t, f.admin(), 200, "q", "readme"); len(comps) != 1 {
		t.Errorf("a plain value: %v", short(comps))
	}
}
