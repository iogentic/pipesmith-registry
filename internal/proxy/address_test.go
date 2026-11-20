package proxy

import (
	"errors"
	"net/url"
	"testing"
)

// The address of a request to a remote is the remote address of the
// repository plus the request path (decision 5.11): no segment leaves the
// base path, an encoded slash stays encoded, the path cannot change scheme,
// host or port, and the query of the remote address stays in front of the
// query of the request.
func TestJoin(t *testing.T) {
	for _, c := range []struct {
		name, base, path, query string
		want                    string // empty for a refusal
	}{
		{"a file below the base path", "https://repo.example.com/maven2/", "/com/example/a/1.0/a-1.0.jar", "",
			"https://repo.example.com/maven2/com/example/a/1.0/a-1.0.jar"},
		{"a base path without the final slash", "https://repo.example.com/maven2", "/com/example/a.jar", "",
			"https://repo.example.com/maven2/com/example/a.jar"},
		{"a base without a path", "https://registry.example.com", "/demo", "", "https://registry.example.com/demo"},
		{"a path without the leading slash", "https://repo.example.com/maven2/", "com/example/a.jar", "",
			"https://repo.example.com/maven2/com/example/a.jar"},
		{"the root of the base", "https://registry.example.com/npm/", "/", "", "https://registry.example.com/npm/"},
		{"a final slash stays", "https://registry.example.com/", "/v2/", "", "https://registry.example.com/v2/"},
		{"an encoded slash stays encoded", "https://registry.example.com/", "/@demo%2Fpkg", "",
			"https://registry.example.com/@demo%2Fpkg"},
		{"a lower case encoded slash", "https://registry.example.com/", "/@demo%2fpkg/-/pkg-1.0.0.tgz", "",
			"https://registry.example.com/@demo%2Fpkg/-/pkg-1.0.0.tgz"},
		{"an encoded scheme and host stay a segment", "https://repo.example.com/maven2/", "/http:%2F%2Fevil.example.org/x", "",
			"https://repo.example.com/maven2/http:%2F%2Fevil.example.org/x"},
		{"a question mark of the path stays in the path", "https://repo.example.com/maven2/", "/a%3Fb=1/c.jar", "",
			"https://repo.example.com/maven2/a%3Fb=1/c.jar"},
		{"characters a path segment must escape", "https://repo.example.com/raw/", "/docs/read me #1.txt", "",
			"https://repo.example.com/raw/docs/read%20me%20%231.txt"},
		{"the port of the base", "https://files.example.com:8443/raw/", "/a.txt", "", "https://files.example.com:8443/raw/a.txt"},
		{"the query of the request", "https://registry.example.com/", "/v2/demo/tags/list", "n=10&last=1.0",
			"https://registry.example.com/v2/demo/tags/list?n=10&last=1.0"},
		{"the query of the base", "https://files.example.com/raw/?channel=stable", "/a.txt", "",
			"https://files.example.com/raw/a.txt?channel=stable"},
		{"the query of the base before the one of the request", "https://files.example.com/raw/?channel=stable", "/a.txt", "x=1",
			"https://files.example.com/raw/a.txt?channel=stable&x=1"},
		{"a segment ..", "https://repo.example.com/maven2/", "/../secret.txt", "", ""},
		{"a segment .. inside", "https://repo.example.com/maven2/", "/a/b/../../../secret.txt", "", ""},
		{"an encoded segment ..", "https://repo.example.com/maven2/", "/a/%2e%2E/b", "", ""},
		{"a segment that decodes to ../", "https://repo.example.com/maven2/", "/a/..%2Fsecret", "", ""},
		{"a segment that decodes to a backslash and ..", "https://repo.example.com/maven2/", "/a/..%5Csecret", "", ""},
		{"a segment .", "https://repo.example.com/maven2/", "/a/./b", "", ""},
		{"an empty segment", "https://repo.example.com/maven2/", "//evil.example.org/x", "", ""},
		{"a scheme and host as the path", "https://repo.example.com/maven2/", "http://evil.example.org/x", "", ""},
		{"an escape that does not decode", "https://repo.example.com/maven2/", "/a%zz", "", ""},
		{"a fragment in the query", "https://registry.example.com/", "/a", "x=1#top", ""},
		{"a control character in the query", "https://registry.example.com/", "/a", "x=1\r\nHost: evil", ""},
	} {
		base, err := url.Parse(c.base)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Join(base, c.path, c.query)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: %q was joined to %s", c.name, c.path, got)
			} else if !errors.Is(err, ErrPath) {
				t.Errorf("%s: the error %v is no ErrPath", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
		if got.Scheme != base.Scheme || got.Host != base.Host || got.User != nil || got.Fragment != "" {
			t.Errorf("%s: %s left the origin of %s", c.name, got, base)
		}
		if got.RequestURI() != got.EscapedPath()+map[bool]string{true: "?" + got.RawQuery, false: ""}[got.RawQuery != ""] {
			t.Errorf("%s: request target %s", c.name, got.RequestURI())
		}
	}
}

// Join never changes the base it is given.
func TestJoinLeavesTheBase(t *testing.T) {
	base, _ := url.Parse("https://files.example.com/raw/?channel=stable")
	before := *base
	if _, err := Join(base, "/a/b.txt", "x=1"); err != nil {
		t.Fatal(err)
	}
	if *base != before {
		t.Fatalf("the base became %s", base)
	}
}
