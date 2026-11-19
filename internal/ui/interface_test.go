package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The tests of this file pin by content what the owner ordered by name in the
// interface: the links to the manual and to the API page in the top bar, and
// the line of the copyright holder in the About dialog. The test of the taken
// commit in ui_test.go sees that js/app.js changed, not what it holds.

func embeddedApp(t *testing.T) string {
	t.Helper()
	raw, err := fs.ReadFile(files, "web/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// between is the text of app from the one place of start up to the next end.
func between(t *testing.T, app, start, end string) string {
	t.Helper()
	if n := strings.Count(app, start); n != 1 {
		t.Fatalf("js/app.js holds %q %d times, want once", start, n)
	}
	from := strings.Index(app, start)
	to := strings.Index(app[from:], end)
	if to < 0 {
		t.Fatalf("js/app.js holds no %q after %q", end, start)
	}
	return app[from : from+to]
}

// The top bar holds a link Documentation to the manual and a link API to the
// API page. Both are relative, so they work below a path prefix, and both
// open a new tab without a handle on the opener.
func TestTopBarLinksToTheManualAndTheAPIPage(t *testing.T) {
	bar := between(t, embeddedApp(t), `{ class: "top-end" }`, "userMenu()")
	anchors := regexp.MustCompile(`h\("a", \{[^}]*\}\)`).FindAllString(bar, -1)
	for _, link := range []struct{ text, href string }{
		{"Documentation", "manual/"},
		{"API", "manual/api.html"},
	} {
		var found []string
		for _, a := range anchors {
			if strings.Contains(a, `text: "`+link.text+`"`) {
				found = append(found, a)
			}
		}
		if len(found) != 1 {
			t.Errorf("the top bar holds %d links with the text %q, want one", len(found), link.text)
			continue
		}
		for _, want := range []string{`href: "` + link.href + `"`, `target: "_blank"`, `rel: "noopener"`} {
			if !strings.Contains(found[0], want) {
				t.Errorf("the link %q of the top bar lacks %s: %s", link.text, want, found[0])
			}
		}
	}
}

// copyrightLine is the SHA-256 of the copyright line of the About dialog,
// without the white space around it. tools/check_text.py allows the same
// line of js/app.js by the same digest. The owner ordered the line on
// 2026-11-19, and the holder is named in js/app.js alone, so this test
// compares a digest and names nobody.
const copyrightLine = "8ce26d2b3c0d4d1720d6bc56b9dae53538676e413a40e65b6ac3a056750c4aa8"

// The About dialog holds one line Copyright (c) <year> <holder>, between its
// title and its buttons, and it is the line the owner ordered.
func TestAboutNamesTheCopyrightHolder(t *testing.T) {
	app := embeddedApp(t)
	dialog := between(t, app, `title: "About"`, "actions:")
	lines := regexp.MustCompile(`(?m)^.*Copyright \(c\) .*$`).FindAllString(dialog, -1)
	if len(lines) != 1 {
		t.Fatalf("the About dialog holds %d copyright lines, want one", len(lines))
	}
	line := strings.TrimSpace(lines[0])
	form := regexp.MustCompile(`^h\("p", \{ class: "muted", text: "Copyright \(c\) 20\d\d \p{Lu}\p{L}+( \p{Lu}\p{L}+)+" \}\),?$`)
	if !form.MatchString(line) {
		t.Errorf("the copyright line of the About dialog is not a paragraph Copyright (c) <year> <first name> <last name>")
	}
	sum := sha256.Sum256([]byte(line))
	if got := hex.EncodeToString(sum[:]); got != copyrightLine {
		t.Errorf("the copyright line of the About dialog has the digest %s, want %s: the line the owner ordered changed", got, copyrightLine)
	}
	if n := strings.Count(app, "Copyright (c) "); n != 1 {
		t.Errorf("js/app.js holds %d copyright lines, want the one of the About dialog", n)
	}
}
