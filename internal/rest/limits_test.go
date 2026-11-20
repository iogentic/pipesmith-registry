package rest

import (
	"testing"
	"time"
)

func TestSearchTimeoutIsADeadlineWithACap(t *testing.T) {
	f := newFixture(t)
	for _, value := range []string{"0", "-1", "x", "1.5", "121", "99999999999999999999"} {
		_, v := search(t, f.admin(), 400, "timeout", value)
		message(t, v)
	}
	for _, value := range []string{"1", "120"} {
		comps, _ := search(t, f.admin(), 200, "timeout", value)
		if len(comps) != len(allComponents) {
			t.Fatalf("timeout %s: %d components", value, len(comps))
		}
	}
	// the timeout is part of the query: a token of another timeout is refused
	if d, ok := searchDeadline(query("timeout", "7")); !ok || d != 7*time.Second {
		t.Fatalf("%v %v", d, ok)
	}
	if d, ok := searchDeadline(query()); !ok || d != searchTimeout {
		t.Fatalf("%v %v", d, ok)
	}
}

func TestASearchPastItsTimeoutAnswers503(t *testing.T) {
	f := newFixture(t)
	env := f.admin()
	past := time.Now().Add(-time.Second)
	// a deadline of the search, not of the request: 503 with a message
	a, err := env.withDeadline(past, func(env *Env) (Answer, error) { return Search(env, query()) })
	message(t, body(t, a, err, 503))
	a, err = env.withDeadline(past, func(env *Env) (Answer, error) { return SearchAssets(env, query()) })
	message(t, body(t, a, err, 503))
}
