package server

import "testing"

// A write that a browser session authenticates needs the request token on
// the client path as on every other route (docs/plans/phases.md, section
// Identity); Basic credentials need none, and a read needs none.
func TestSessionWritesNeedTheRequestToken(t *testing.T) {
	w := startIngest(t, map[string]map[string]any{"demo-raw": storage("ALLOW")})
	const cookie, token = "fake-session=s1", "csrf-s1"
	target := "/repository/demo-raw/docs/session.txt"
	for _, method := range []string{"PUT", "DELETE"} {
		r := do(t, w.addr(), method, target, []byte("x"), "Cookie", cookie)
		if r.status != 403 || string(r.body) != csrfRefusal+"\n" {
			t.Fatalf("%s without the token: %d %q", method, r.status, r.body)
		}
	}
	if r := do(t, w.addr(), "PUT", target, []byte("x"), "Cookie", cookie, "X-Pipesmith-Csrf", token); r.status != 201 {
		t.Fatalf("PUT with the token: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "GET", target, nil, "Cookie", cookie); r.status != 200 || string(r.body) != "x" {
		t.Fatalf("GET: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "DELETE", target, nil, "Cookie", cookie, "X-Pipesmith-Csrf", token); r.status != 204 {
		t.Fatalf("DELETE with the token: %d %q", r.status, r.body)
	}
	if r := do(t, w.addr(), "PUT", target, []byte("y"), "basic", asAdmin); r.status != 201 {
		t.Fatalf("PUT with Basic: %d %q", r.status, r.body)
	}
	if files := w.workFiles(); len(files) != 0 {
		t.Fatalf("work files %v", files)
	}
}
