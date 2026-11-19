package server

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/iogentic/pipesmith-registry/internal/ui"
)

func manualIndex(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../ui/manual/index.html")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestManualIsServedWithoutSecurity(t *testing.T) {
	a := startSecured(t, nil)
	got := do(t, a.addr(), "GET", "/manual/", nil)
	if got.status != 200 || !bytes.Equal(got.body, manualIndex(t)) {
		t.Fatalf("GET /manual/: %d, %d bytes, want 200 and the embedded index.html", got.status, len(got.body))
	}
	if got.header.Get("Content-Security-Policy") != ui.Policy {
		t.Errorf("Content-Security-Policy %q", got.header.Get("Content-Security-Policy"))
	}
	page := do(t, a.addr(), "GET", "/", nil)
	if page.status != 200 || !bytes.Equal(page.body, pageHTML) {
		t.Fatalf("GET /: %d, %d bytes, want the page of the reference", page.status, len(page.body))
	}
}

func TestManualIsPublicWithSecurity(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	for _, target := range []string{"/manual/", "/manual/manual.css"} {
		if got := a.send("GET", target); got.status != 200 {
			t.Errorf("GET %s without credentials: %d %q, want 200", target, got.status, got.body)
		}
	}
	got := a.send("GET", "/manual")
	if got.status != 308 || got.header.Get("Location") != "manual/" {
		t.Fatalf("GET /manual: %d, Location %q, want 308 and manual/", got.status, got.header.Get("Location"))
	}
}

func TestManualPassesTheHostGate(t *testing.T) {
	for name, security := range map[string]*fakeSecurity{"off": nil, "on": newFakeSecurity()} {
		a := newSecureApp(t, security)
		if got := a.send("GET", "/manual/", "Host", "other.example"); got.status != 403 {
			t.Errorf("security %s: GET /manual/ with another Host: %d, want 403", name, got.status)
		}
	}
}

func TestManualRequestIsLoggedLikeAnInterfaceFile(t *testing.T) {
	a := newSecureApp(t, newFakeSecurity())
	a.send("GET", "/js/app.js")
	a.send("GET", "/manual/manual.css")
	client := regexp.MustCompile(`^INFO:     \S+ - `)
	var uiLine, manualLine string
	for _, line := range strings.Split(a.access.String(), "\n") {
		switch {
		case strings.Contains(line, " /js/app.js "):
			uiLine = client.ReplaceAllString(line, "INFO:     <client> - ")
		case strings.Contains(line, " /manual/manual.css "):
			manualLine = client.ReplaceAllString(line, "INFO:     <client> - ")
		}
	}
	if uiLine == "" || manualLine == "" {
		t.Fatalf("access log %q", a.access.String())
	}
	if want := strings.Replace(uiLine, "/js/app.js", "/manual/manual.css", 1); manualLine != want {
		t.Errorf("access line %q, want %q", manualLine, want)
	}
}

// The lists of routes without sign in in the chapters Sign In and Security
// (the elements of class public-routes) name exactly the paths of the routes
// of rule public that apiRoutes reads. A placeholder <...> of the manual and
// a group {}, {+} or {*} of a template both count as {}; the method before a
// path is left out.
func TestManualNamesThePublicRoutes(t *testing.T) {
	read, err := apiRoutes()
	if err != nil {
		t.Fatal(err)
	}
	group := regexp.MustCompile(`\{[+*]?\}`)
	code := map[string]bool{}
	for _, r := range read {
		if r.rule.kind == rulePublic && !r.refused {
			code[group.ReplaceAllString(r.template, "{}")] = true
		}
	}
	if len(code) == 0 {
		t.Fatal("apiRoutes holds no route of rule public")
	}
	index := manualIndex(t)
	unescape := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")
	placeholder := regexp.MustCompile(`<[^<>]*>`)
	for _, chapter := range []string{"sign-in", "security"} {
		section := regexp.MustCompile(`(?s)<section id="` + chapter + `">.*?</section>`).Find(index)
		lists := regexp.MustCompile(`(?s)<span class="public-routes">(.*?)</span>`).FindAllSubmatch(section, -1)
		if len(lists) == 0 {
			t.Errorf("the chapter %s holds no list of class public-routes", chapter)
			continue
		}
		manual := map[string]bool{}
		for _, list := range lists {
			for _, m := range regexp.MustCompile(`<code>(?:[A-Z]+ )?(/[^<]*)</code>`).FindAllSubmatch(list[1], -1) {
				manual[placeholder.ReplaceAllString(unescape.Replace(string(m[1])), "{}")] = true
			}
		}
		for path := range code {
			if !manual[path] {
				t.Errorf("the chapter %s does not name %s, a route of rule public", chapter, path)
			}
		}
		for path := range manual {
			if !code[path] {
				t.Errorf("the chapter %s names %s, which is no route of rule public", chapter, path)
			}
		}
	}
}
