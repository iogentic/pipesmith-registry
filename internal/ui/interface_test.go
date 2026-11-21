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

// Ruling SR5 and SR6 of leg 6-stores-b: the create of a hosted repository
// offers the stores it may write into and a new store, whose name and path
// travel as storage.blobStoreName and storage.newBlobStore; a proxy offers
// local-write only.
func TestTheRepositoryFormPicksOrCreatesAStore(t *testing.T) {
	raw, err := fs.ReadFile(files, "web/js/pages/repositories/editor.js")
	if err != nil {
		t.Fatal(err)
	}
	editor := string(raw)
	for _, want := range []string{
		`const NEW_STORE = "`,
		`label: "New blob store"`,
		`name: "newStore.name"`,
		`name: "newStore.path"`,
		`show: (v) => v.storage && v.storage.blobStoreName === NEW_STORE`,
		`body.storage.newBlobStore = { path: (values.newStore && values.newStore.path) || null };`,
		`body.storage.blobStoreName = values.newStore.name;`,
		`delete body.newStore;`,
		// review R12: a relative path is read below stores/, so the placeholder does not start with it
		`placeholder: "<name>",`,
		`type === "hosted" ? s.writable : type === "proxy" ? s.name === "local-write" : true`,
		`if (creating && type !== "group" && !offered.some((s) => s.name === "local-write")) offered.unshift({ name: "local-write", writable: true });`,
	} {
		if !strings.Contains(editor, want) {
			t.Errorf("js/pages/repositories/editor.js holds no %q", want)
		}
	}
}

// Ruling SR13 (finding F2 of the test host): the blob store page renders
// without the count of the marked blobs, asks for it after, shows Counting
// while the server counts and Not counted on a failure. Ruling SR11 (F1):
// the text of an unavailable store names both causes. Ruling SC1 of leg
// 6-stores-c: a count with a reason shows Not counted with that reason.
func TestTheBlobStorePageDoesNotWaitForTheMarkedCount(t *testing.T) {
	raw, err := fs.ReadFile(files, "web/js/pages/storage/blob-store-editor.js")
	if err != nil {
		t.Fatal(err)
	}
	editor := string(raw)
	load := between(t, editor, "  async function load() {", "\n  }\n")
	if strings.Contains(load, "deleted(") {
		t.Errorf("load waits for the marked count:\n%s", load)
	}
	for _, want := range []string{
		`known.marked = await api.blobStores.deleted(name);`,
		`if (known.marked && known.marked.counting && polls < MARKED_POLLS) {`,
		`text: "Counting"`,
		"text: `Not counted: ${marked.failed}`",
		"if (marked && marked.reason && marked.blobCount == null) return h(\"span\", { class: \"muted\", text: `Not counted: ${marked.reason}` });",
		"ctx.main.append(h(\"div\", { class: \"split store-facts\" }, usage, users));\n    loadMarked(0);",
		"or the folder is empty while the index holds files of it",
		// review H8: after the last poll the page says so
		`if (known.marked && known.marked.counting && polls >= MARKED_POLLS) known.marked.stopped = true;`,
		`text: "Still counting; open the page again later to see the count."`,
	} {
		if !strings.Contains(editor, want) {
			t.Errorf("js/pages/storage/blob-store-editor.js holds no %q", want)
		}
	}
	list, err := fs.ReadFile(files, "web/js/pages/storage/blob-stores.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(list), "or the folder is empty while the index holds files of it") {
		t.Error("js/pages/storage/blob-stores.js names one cause of an unavailable store only")
	}
}
