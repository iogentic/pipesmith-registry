package server

import (
	"strings"
	"testing"
)

// A query string longer than 8 KiB is refused with 400 on the REST routes
// before the handler reads it; one of 8 KiB is read.
func TestTheQueryStringOfARESTRouteIsCapped(t *testing.T) {
	w := startCataloged(t)
	long := "/service/rest/v1/search?q=" + strings.Repeat("a", 9*1024)
	r := do(t, w.addr(), "GET", long, nil, "basic", asAdmin)
	if r.status != 400 || !strings.Contains(string(r.body), "8192") {
		t.Fatalf("%d %s", r.status, r.body)
	}
	r = do(t, w.addr(), "GET", "/service/rest/v1/assets?repository=demo-raw&x="+strings.Repeat("a", 8192-len("repository=demo-raw&x=")), nil,
		"basic", asAdmin)
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
}
